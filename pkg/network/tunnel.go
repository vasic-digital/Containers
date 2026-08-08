package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// errNoTunnel is the sentinel wrapped by CloseTunnel when no tunnel exists on
// the requested local port. CloseAll / CloseAllForHost use errors.Is to treat a
// tunnel that a concurrent reaper already removed (already gone) as success
// rather than surfacing a spurious failure.
var errNoTunnel = errors.New("no tunnel on port")

// TunnelManager creates and manages SSH tunnels between local and
// remote hosts.
type TunnelManager interface {
	// CreateTunnel establishes an SSH tunnel to the named host.
	CreateTunnel(
		ctx context.Context,
		hostName string,
		spec TunnelSpec,
	) (*TunnelInfo, error)

	// CloseTunnel closes the tunnel on the given local port.
	CloseTunnel(localPort string) error

	// ListTunnels returns all active tunnels.
	ListTunnels() []TunnelInfo

	// CloseAllForHost closes all tunnels to a specific host.
	CloseAllForHost(hostName string) error

	// CloseAll closes every active tunnel.
	CloseAll() error
}

// DefaultTunnelManager implements TunnelManager using SSH CLI.
type DefaultTunnelManager struct {
	mu          sync.Mutex
	tunnels     map[string]*tunnelEntry // keyed by local port
	hostManager remote.HostManager
	allocator   *PortAllocator
	opts        Options
	logger      logging.Logger
}

type tunnelEntry struct {
	info TunnelInfo
	cmd  *exec.Cmd

	// waitOnce guards the single cmd.Wait() for this process. The reaper
	// goroutine and CloseTunnel both need to reap the ssh process, but
	// exec.Cmd.Wait() must not be called twice (it does an unsynchronized
	// ProcessState read then a second Process.Wait()), so both funnel through
	// wait() and only the first invocation actually calls cmd.Wait().
	waitOnce sync.Once
	waitErr  error
}

// wait reaps the tunnel's ssh process exactly once, returning the (cached)
// result of cmd.Wait() to every caller.
func (e *tunnelEntry) wait() error {
	e.waitOnce.Do(func() { e.waitErr = e.cmd.Wait() })
	return e.waitErr
}

// tunnelBindPollInterval and tunnelBindPollWindow bound the cheap local-bind
// confirmation CreateTunnel performs for TunnelLocal specs immediately after
// cmd.Start() succeeds (NET2-2). Short and bounded so CreateTunnel never
// hangs waiting on a slow or dead ssh process: a real ssh -L forward binds
// its local listener within milliseconds of forking — long before the SSH
// handshake, let alone the remote side of the forward, completes — so this
// window is enough to catch a fork that silently never binds (a malformed
// forward spec, an immediate ssh crash) without materially slowing down the
// common (successful) case.
const (
	tunnelBindPollInterval = 10 * time.Millisecond
	tunnelBindPollWindow   = 200 * time.Millisecond
)

// confirmLocalBind polls isPortAvailable(port) for up to tunnelBindPollWindow,
// returning true the moment something binds the port (isPortAvailable reports
// false — the socket is no longer free) and false if the window elapses with
// the port still free. It never blocks longer than the window: this is a
// cheap local readiness check, NOT proof the remote side of the ssh forward
// is fully established (that remains an honest gap — see the CreateTunnel
// call site).
func confirmLocalBind(port int) bool {
	deadline := time.Now().Add(tunnelBindPollWindow)
	for {
		if !isPortAvailable(port) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(tunnelBindPollInterval)
	}
}

// yesNo renders an SSH -o boolean option value ("yes"/"no").
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// tunnelDestination renders the ssh destination positional ("<user>@<address>")
// for a tunnel host. It is BOTH the final positional argv element tunnelArgs
// appends AND the exact string CreateTunnel's §NET3 leading-dash guard inspects,
// so the guarded value and the spawned value are provably identical (single
// source of truth — a divergent guard would let a rejected form still spawn).
func tunnelDestination(host remote.RemoteHost) string {
	return fmt.Sprintf("%s@%s", host.User, host.Address)
}

// NewTunnelManager creates a DefaultTunnelManager.
func NewTunnelManager(
	hostManager remote.HostManager,
	logger logging.Logger,
	opts ...Option,
) *DefaultTunnelManager {
	o := ApplyOptions(opts)
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultTunnelManager{
		tunnels:     make(map[string]*tunnelEntry),
		hostManager: hostManager,
		allocator:   NewPortAllocator(o.PortRangeStart, o.PortRangeEnd),
		opts:        o,
		logger:      logger,
	}
}

// CreateTunnel establishes an SSH tunnel.
func (m *DefaultTunnelManager) CreateTunnel(
	ctx context.Context,
	hostName string,
	spec TunnelSpec,
) (*TunnelInfo, error) {
	host, err := m.hostManager.GetHost(hostName)
	if err != nil {
		return nil, fmt.Errorf(
			"get host %s: %w", hostName, err,
		)
	}
	if host == nil {
		return nil, fmt.Errorf("host %s not found", hostName)
	}

	// §NET3: the tunnel ssh child is spawned as a bare argv (exec.CommandContext(
	// "ssh", args...), no shell), so shell metacharacters in the destination are
	// inert — but the destination is appended as the FINAL positional argv
	// element (tunnelArgs → tunnelDestination, "<user>@<address>") with no "--"
	// guard, so a destination that BEGINS WITH '-' is parsed by ssh's OWN getopt
	// as an OPTION, not a host. e.g. a host.User of "-oProxyCommand=<cmd>" injects
	// ssh's ProxyCommand (arbitrary command execution). Refuse it BEFORE any port
	// allocation or spawn (ssh has no reliable "--" end-of-options for the host,
	// so rejection is the safe, deterministic fix). Mirrors pkg/egress §EG2-1.
	// Checking the composed destination covers a leading '-' in host.User (the
	// leading token); an empty User yields "@<address>", which begins with '@'
	// and is inert to getopt.
	dest := tunnelDestination(*host)
	if strings.HasPrefix(dest, "-") {
		return nil, fmt.Errorf(
			"refusing ssh tunnel destination %q that begins with '-' "+
				"(would be parsed as an ssh option — argument injection)", dest,
		)
	}

	// Auto-allocate local port if not specified.
	autoAllocatedPort := -1
	if spec.LocalPort == "" {
		port, err := m.allocator.Allocate(spec.Description)
		if err != nil {
			return nil, fmt.Errorf(
				"allocate port: %w", err,
			)
		}
		spec.LocalPort = strconv.Itoa(port)
		autoAllocatedPort = port
	}

	args := m.tunnelArgs(*host, spec)
	cmd := exec.CommandContext(ctx, "ssh", args...)

	if err := cmd.Start(); err != nil {
		// The SSH process never launched, so no tunnelEntry is stored and
		// CloseTunnel will never run to release the port. Release the port we
		// auto-allocated above, otherwise every failed launch permanently
		// leaks one port from the range until the allocator is exhausted.
		if autoAllocatedPort >= 0 {
			m.allocator.Release(autoAllocatedPort)
		}
		return nil, fmt.Errorf(
			"start tunnel to %s: %w", hostName, err,
		)
	}

	// NET2-2: cmd.Start() only proves the ssh process forked — it says
	// nothing about whether the forward was actually established. For local
	// forwards (TunnelLocal), ssh binds its local listener within
	// milliseconds of forking, so a short, bounded poll for "something now
	// owns the local socket" is a cheap, honest readiness signal: report
	// TunnelActive only once confirmed, TunnelFailed if the window elapses
	// with nothing bound — never silently claim success, never hang. This is
	// deliberately narrow (a positive local bind is necessary, not
	// sufficient, proof the remote side of the forward works) and
	// complements, never replaces, reapTunnel's exit-capture below, which
	// keeps catching a later ssh death regardless of what state we report
	// here. TunnelRemote is out of scope: its LocalPort is not necessarily
	// bound by our own ssh process, so isPortAvailable would not be a
	// meaningful signal for it.
	state := TunnelActive
	if spec.Direction == TunnelLocal {
		if localPortNum, perr := strconv.Atoi(spec.LocalPort); perr == nil {
			if !confirmLocalBind(localPortNum) {
				state = TunnelFailed
				m.logger.Info(
					"tunnel local bind not confirmed within %s: "+
						"local port %s via %s",
					tunnelBindPollWindow, spec.LocalPort, hostName,
				)
			}
		}
	}

	info := TunnelInfo{
		Spec:      spec,
		HostName:  hostName,
		State:     state,
		CreatedAt: time.Now(),
		PID:       cmd.Process.Pid,
	}

	entry := &tunnelEntry{info: info, cmd: cmd}

	m.mu.Lock()
	if _, exists := m.tunnels[spec.LocalPort]; exists {
		// A live tunnel already occupies this local port. Overwriting the map
		// entry would orphan its running ssh process (leaked goroutine/zombie,
		// no way to close it) and double-book the port. Reject this request and
		// tear down the process we just started for it. Check-and-insert is one
		// critical section so two concurrent CreateTunnel calls for the same
		// port cannot both win.
		m.mu.Unlock()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = entry.wait()
		if autoAllocatedPort >= 0 {
			m.allocator.Release(autoAllocatedPort)
		}
		return nil, fmt.Errorf(
			"tunnel already active on local port %s", spec.LocalPort,
		)
	}
	m.tunnels[spec.LocalPort] = entry
	m.mu.Unlock()

	// NET2-4: an explicit LocalPort was never recorded in the allocator's own
	// bookkeeping, so IsAllocated / AllocatedCount / ListAllocations
	// under-reported reality for it — only auto-allocated ports were ever
	// visible there, so a caller probing the allocator for a free port could
	// get a false "not allocated" for a live explicit-port tunnel (a wasted
	// spawn-then-reject). Register it now that this tunnel has won the map
	// race above and is the sole owner of this port. CloseTunnel's existing
	// unconditional m.allocator.Release(port) and reapTunnel's clean-exit
	// release (below) already handle releasing it correctly for both
	// origins — m.tunnels remains the real collision guard either way.
	if autoAllocatedPort < 0 {
		if explicitPortNum, perr := strconv.Atoi(spec.LocalPort); perr == nil {
			m.allocator.MarkAllocated(explicitPortNum, spec.Description)
		}
	}

	// Reap the ssh process when it exits, so a tunnel that dies (crashes, is
	// killed out from under us, or the SSH server drops it) is removed from the
	// active set and its port released — instead of lingering forever in
	// State=Active with a zombie child and a permanently-leaked port. ctx is
	// passed so the reaper can tell an intentional teardown (ctx cancelled →
	// exec.CommandContext killed ssh — a normal close) from a tunnel that died on
	// its own (a forward-failure death, which must be surfaced as
	// State=TunnelFailed rather than silently deleted).
	go m.reapTunnel(ctx, spec.LocalPort, entry)

	m.logger.Info(
		"tunnel created: %s %s:%s <-> local:%s via %s",
		spec.Direction, spec.RemoteHost, spec.RemotePort,
		spec.LocalPort, hostName,
	)

	return &info, nil
}

// CloseTunnel closes the tunnel on the given local port.
func (m *DefaultTunnelManager) CloseTunnel(
	localPort string,
) error {
	m.mu.Lock()
	entry, ok := m.tunnels[localPort]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w %s", errNoTunnel, localPort)
	}
	delete(m.tunnels, localPort)
	m.mu.Unlock()

	port, _ := strconv.Atoi(localPort)
	m.allocator.Release(port)

	if entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
		_ = entry.wait()
	}

	m.logger.Info("tunnel closed: local port %s", localPort)
	return nil
}

// reapTunnel blocks until the tunnel's ssh process exits, then reconciles the
// active set. It is spawned once per successfully-created tunnel. Teardown is
// guarded by pointer identity: if CloseTunnel already removed this exact entry,
// or a later CreateTunnel replaced this port with a different tunnel, reapTunnel
// touches nothing — acting here would free a port the replacement (or
// CloseTunnel) already owns.
//
// Exit disposition, once this entry is still the current one:
//
//   - Dead-on-arrival / forward-failure death (the ssh process exited NON-ZERO
//     on its own, ctx still live): ssh runs with ExitOnForwardFailure=yes, so it
//     exits the instant a forward cannot be established (remote port already
//     bound, auth / host-key failure). CreateTunnel already returned success with
//     State=TunnelActive, so silently deleting the entry here would let the
//     tunnel simply VANISH from ListTunnels with no error or signal — the
//     "absence-of-error masks a dead tunnel" bluff. Instead mark it
//     State=TunnelFailed and KEEP it queryable so a caller polling ListTunnels
//     can detect the failure and CloseTunnel it (which releases the port). The
//     port is deliberately NOT released here, preserving the invariant "an
//     entry in m.tunnels ⟺ its port is still allocated" — this now holds for
//     BOTH auto-allocated AND explicit ports (NET2-4: CreateTunnel registers
//     an explicit LocalPort in the allocator too, right after it wins the map
//     race above).
//
//   - Clean exit (waitErr == nil) OR intentional teardown (ctx cancelled →
//     exec.CommandContext killed ssh — a normal close, not a failure): remove
//     the entry and release its allocator reservation, exactly as before.
//     NET2-4: this release is now unconditional (parsed straight from
//     localPort) because CreateTunnel registers EVERY successfully-created
//     tunnel's port in the allocator, auto-allocated or explicit — previously
//     only auto-allocated ports were released here, silently leaking the
//     allocator's bookkeeping for an explicit-port tunnel that exited cleanly
//     without ever being closed by the caller.
func (m *DefaultTunnelManager) reapTunnel(
	ctx context.Context,
	localPort string, entry *tunnelEntry,
) {
	waitErr := entry.wait()

	m.mu.Lock()
	cur, ok := m.tunnels[localPort]
	if !ok || cur != entry {
		m.mu.Unlock()
		return
	}

	// A non-zero exit that is NOT an intentional ctx-driven teardown means the
	// tunnel died on its own (forward failure). Retain it as TunnelFailed rather
	// than silently deleting it.
	if waitErr != nil && ctx.Err() == nil {
		entry.info.State = TunnelFailed
		m.mu.Unlock()
		m.logger.Info(
			"tunnel failed: local port %s (ssh process exited: %v) — "+
				"retained as State=failed for caller inspection",
			localPort, waitErr,
		)
		return
	}

	delete(m.tunnels, localPort)
	m.mu.Unlock()

	if port, perr := strconv.Atoi(localPort); perr == nil {
		m.allocator.Release(port)
	}
	m.logger.Info(
		"tunnel reaped: local port %s (ssh process exited)", localPort,
	)
}

// ListTunnels returns all active tunnels.
func (m *DefaultTunnelManager) ListTunnels() []TunnelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	infos := make([]TunnelInfo, 0, len(m.tunnels))
	for _, entry := range m.tunnels {
		infos = append(infos, entry.info)
	}
	return infos
}

// CloseAllForHost closes all tunnels to a specific host.
func (m *DefaultTunnelManager) CloseAllForHost(
	hostName string,
) error {
	m.mu.Lock()
	var ports []string
	for port, entry := range m.tunnels {
		if entry.info.HostName == hostName {
			ports = append(ports, port)
		}
	}
	m.mu.Unlock()

	var firstErr error
	for _, port := range ports {
		// A tunnel may be reaped concurrently between the snapshot above and this
		// CloseTunnel; errNoTunnel means it is already gone — the desired end
		// state, not a failure — so only a real close error is recorded.
		if err := m.CloseTunnel(port); err != nil &&
			!errors.Is(err, errNoTunnel) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseAll closes every active tunnel.
func (m *DefaultTunnelManager) CloseAll() error {
	m.mu.Lock()
	ports := make([]string, 0, len(m.tunnels))
	for port := range m.tunnels {
		ports = append(ports, port)
	}
	m.mu.Unlock()

	var firstErr error
	for _, port := range ports {
		// A tunnel may be reaped concurrently between the snapshot above and this
		// CloseTunnel; errNoTunnel means it is already gone — the desired end
		// state, not a failure — so only a real close error is recorded.
		if err := m.CloseTunnel(port); err != nil &&
			!errors.Is(err, errNoTunnel) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *DefaultTunnelManager) tunnelArgs(
	host remote.RemoteHost, spec TunnelSpec,
) []string {
	var fwdFlag, fwdSpec string
	remoteTarget := spec.RemoteHost
	if remoteTarget == "" {
		remoteTarget = "localhost"
	}

	switch spec.Direction {
	case TunnelRemote:
		fwdFlag = "-R"
		fwdSpec = fmt.Sprintf("%s:%s:%s",
			spec.RemotePort, remoteTarget, spec.LocalPort,
		)
	default: // TunnelLocal
		fwdFlag = "-L"
		fwdSpec = fmt.Sprintf("%s:%s:%s",
			spec.LocalPort, remoteTarget, spec.RemotePort,
		)
	}

	// NET2-1: the forward flag and its spec are passed as TWO independent
	// argv elements (matching pkg/egress.buildDynamicForwardArgs' -D form),
	// not one combined "-L host:port:port" string relying on a getopt
	// whitespace-tolerance quirk of the local OpenSSH build. Every test
	// double in this package (writeFakeSSH's shell-script fakes) ignores
	// argv entirely, so a broken single-string forward spec would never be
	// caught without a real argv-dumping double — see
	// TestNET2_1_ForwardSpec_TwoTokenArgv.
	args := []string{
		"-N",
		fwdFlag, fwdSpec,
		"-o", "StrictHostKeyChecking=" + yesNo(m.opts.StrictHostKeyCheck),
		// NET2-6: run the tunnel ssh strictly non-interactively. Both sibling
		// ssh-arg builders in this module already do this — pkg/remote's
		// SSHExecutor.sshArgs and pkg/egress's buildDynamicForwardArgs each
		// pass "-o BatchMode=yes" — and tunnelArgs was the lone omission.
		// Without it, a tunnel whose auth needs a password/passphrase, OR a
		// host-key confirmation prompt (StrictHostKeyChecking=ask, ssh's
		// compiled default), makes ssh read the prompt from the controlling
		// terminal and BLOCK INDEFINITELY: ssh opens /dev/tty for the prompt,
		// bypassing the child's /dev/null stdin, so cmd.Start() returns but the
		// child never proceeds and never dies. That silent hang directly
		// contradicts the "never hang" liveness intent documented on
		// confirmLocalBind (which only bounds the local-bind poll, not the ssh
		// child itself). BatchMode=yes forces ssh to fail fast on any
		// interactive requirement, so the reaper's exit capture surfaces an
		// honest State=TunnelFailed instead of a wedged tunnel. It is a fixed
		// literal (matching both siblings, which hardcode it), so an
		// unconfigured caller's key-based tunnels are unaffected.
		"-o", "BatchMode=yes",
	}

	// NET2-3: keep-alive is now Option-driven (mirrors pkg/remote.Options),
	// defaulting to the SAME literals this function hardcoded before
	// (ServerAliveInterval=30, ServerAliveCountMax=3) so an unconfigured
	// caller's built args are byte-for-byte unchanged.
	if m.opts.KeepAlive > 0 && m.opts.KeepAliveCountMax > 0 {
		args = append(args,
			"-o", fmt.Sprintf(
				"ServerAliveInterval=%d",
				int(m.opts.KeepAlive.Seconds()),
			),
			"-o", fmt.Sprintf(
				"ServerAliveCountMax=%d",
				m.opts.KeepAliveCountMax,
			),
		)
	}

	args = append(args,
		"-o", "ExitOnForwardFailure=yes",
		"-p", strconv.Itoa(host.SSHPort()),
	)

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		tunnelDestination(host),
	)
	return args
}
