package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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

	info := TunnelInfo{
		Spec:      spec,
		HostName:  hostName,
		State:     TunnelActive,
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

	// Reap the ssh process when it exits, so a tunnel that dies (crashes, is
	// killed out from under us, or the SSH server drops it) is removed from the
	// active set and its port released — instead of lingering forever in
	// State=Active with a zombie child and a permanently-leaked port.
	// autoAllocatedPort (>=0 only when we auto-allocated the port) is passed so
	// the reaper releases ONLY a reservation this tunnel owns. An explicit port
	// was never allocator-managed, so releasing it here could free a *different*
	// live auto-tunnel that happens to hold that same integer.
	go m.reapTunnel(spec.LocalPort, entry, autoAllocatedPort)

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

// reapTunnel blocks until the tunnel's ssh process exits, then removes the
// tunnel from the active set and releases its local port. It is spawned once
// per successfully-created tunnel. Teardown is guarded by pointer identity: if
// CloseTunnel already removed this exact entry, or a later CreateTunnel replaced
// this port with a different tunnel, reapTunnel touches nothing — releasing the
// port here would free a port the replacement (or CloseTunnel) already owns.
func (m *DefaultTunnelManager) reapTunnel(
	localPort string, entry *tunnelEntry, autoAllocatedPort int,
) {
	_ = entry.wait()

	m.mu.Lock()
	cur, ok := m.tunnels[localPort]
	if !ok || cur != entry {
		m.mu.Unlock()
		return
	}
	delete(m.tunnels, localPort)
	m.mu.Unlock()

	if autoAllocatedPort >= 0 {
		m.allocator.Release(autoAllocatedPort)
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
	var fwdArg string
	remoteTarget := spec.RemoteHost
	if remoteTarget == "" {
		remoteTarget = "localhost"
	}

	switch spec.Direction {
	case TunnelRemote:
		fwdArg = fmt.Sprintf("-R %s:%s:%s",
			spec.RemotePort, remoteTarget, spec.LocalPort,
		)
	default: // TunnelLocal
		fwdArg = fmt.Sprintf("-L %s:%s:%s",
			spec.LocalPort, remoteTarget, spec.RemotePort,
		)
	}

	args := []string{
		"-N",
		fwdArg,
		"-o", "StrictHostKeyChecking=no",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		"-p", strconv.Itoa(host.SSHPort()),
	}

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", host.User, host.Address),
	)
	return args
}
