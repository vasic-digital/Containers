package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// This file covers the Wave-20 NET2-HARD network batch (NET2-1..5), a fresh
// §11.4.118 discovery-pressure audit on top of the already-landed NET-HARD
// dead-tunnel-detection fix (wave20_network_audit_test.go). Each guard below
// asserts the FIXED behavior (§11.4.115 GREEN polarity); the accompanying doc
// comment states the surgical revert that reproduces RED.

// --- NET2-1: forward spec passed as two independent argv elements --------

// TestNET2_1_ForwardSpec_TwoTokenArgv proves the -L/-R forward spec is passed
// to ssh as two independent argv elements (flag, spec), not one
// embedded-space string relying on getopt whitespace tolerance. This uses a
// REAL argv-dumping ssh double — every other fake in this package
// (writeFakeSSH's plain shell bodies) ignores argv entirely, so a broken
// single-string forward spec would never be caught by them.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree (two-token tunnelArgs). A
// surgical revert of tunnelArgs back to the single combined-string form
// (fmt.Sprintf("-L %s:%s:%s", ...) as ONE argv element, "-N", fwdArg, ...)
// reproduces RED — the dumped argv contains one line
// "ARG:-L 18080:db-host:5432" instead of the two separate lines
// "ARG:-L" + "ARG:18080:db-host:5432" this test requires.
func TestNET2_1_ForwardSpec_TwoTokenArgv(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")

	// A real argv-dumping ssh double: it enumerates its OWN argv (not the
	// test's) and writes one "ARG:<value>" line per element, then exits 0.
	writeFakeSSH(t, fmt.Sprintf(
		"for a in \"$@\"; do printf 'ARG:%%s\\n' \"$a\"; done > %s\nexit 0",
		argvLog,
	))

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "18080",
		RemoteHost: "db-host",
		RemotePort: "5432",
	})
	require.NoError(t, err)
	require.NotNil(t, info)

	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(argvLog)
		return rerr == nil && len(data) > 0
	}, 3*time.Second, 10*time.Millisecond,
		"fake ssh must have run and dumped its argv")

	data, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	assert.Contains(t, lines, "ARG:-L",
		"the -L flag must be its own independent argv element")
	assert.Contains(t, lines, "ARG:18080:db-host:5432",
		"the forward spec must be its own independent argv element, "+
			"not embedded in the same argv element as -L")
}

// --- NET2-2: honest local-bind confirmation before reporting Active ------

// TestNET2_2_ConfirmLocalBind_TrueWhenAlreadyBound proves confirmLocalBind
// returns true immediately once something owns the local socket.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting confirmLocalBind
// to unconditionally `return false` (or removing the bind check from
// CreateTunnel entirely, always assuming Active) reproduces RED.
func TestNET2_2_ConfirmLocalBind_TrueWhenAlreadyBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	assert.True(t, confirmLocalBind(port),
		"a port that is already bound must be confirmed immediately")
}

// TestNET2_2_ConfirmLocalBind_FalseAndBoundedWhenNeverBound proves
// confirmLocalBind returns false once its window elapses for a port nothing
// ever binds, AND that it never blocks longer than that bounded window (never
// hang).
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting confirmLocalBind to
// unconditionally `return true` reproduces RED.
func TestNET2_2_ConfirmLocalBind_FalseAndBoundedWhenNeverBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close()) // free again — nothing ever rebinds it

	start := time.Now()
	assert.False(t, confirmLocalBind(port),
		"a port nothing ever binds must not be confirmed")
	assert.Less(t, time.Since(start), 2*time.Second,
		"confirmLocalBind must be bounded and never hang")
}

// TestNET2_2_CreateTunnel_ReportsActiveWhenLocalPortBinds proves the
// end-to-end wiring: when the local port is genuinely bound (simulated here
// by the TEST holding the listener — proving the check is driven by
// isPortAvailable, not by any assumption about which process bound it),
// CreateTunnel reports State=TunnelActive.
//
// §11.4.115 RED/GREEN: this half of the pair still PASSes even on the
// pre-NET2-2 tree (Active was always reported); the RED half of the pair is
// TestNET2_2_CreateTunnel_ReportsFailedWhenLocalPortNeverBinds below, which
// the pre-NET2-2 code fails.
func TestNET2_2_CreateTunnel_ReportsActiveWhenLocalPortBinds(t *testing.T) {
	writeFakeSSH(t, "sleep 5") // never itself binds anything

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	explicitPort := strconv.Itoa(port)

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{})
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  explicitPort,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, TunnelActive, info.State,
		"a local port that is genuinely bound (isPortAvailable==false) "+
			"must be reported Active")
}

// TestNET2_2_CreateTunnel_ReportsFailedWhenLocalPortNeverBinds is the RED
// discriminator: this ssh double forks successfully (cmd.Start() succeeds)
// but never implements real TCP forwarding, so the local port stays free.
// CreateTunnel must NOT report TunnelActive for it.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree (State=TunnelFailed). A
// surgical revert of CreateTunnel to its pre-NET2-2 form (State: TunnelActive
// unconditionally, no bind check) reproduces RED — this assertion fails,
// observing the wrong (Active) state instead.
func TestNET2_2_CreateTunnel_ReportsFailedWhenLocalPortNeverBinds(t *testing.T) {
	writeFakeSSH(t, "sleep 5") // forks fine, never binds anything

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
		// LocalPort empty → auto-allocated; the allocator already confirmed
		// this exact port free immediately before ssh forked, so it is
		// deterministically free at fork time — this fake ssh then never
		// binds it.
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, TunnelFailed, info.State,
		"a local port nothing ever binds within the confirmation window "+
			"must NOT be reported Active")
}

// --- NET2-3: Option-driven StrictHostKeyCheck / KeepAlive / KeepAliveCountMax

// TestNET2_3_TunnelArgs_DefaultsUnchanged proves an unconfigured
// DefaultTunnelManager still builds the EXACT ssh args tunnelArgs hardcoded
// before NET2-3 (StrictHostKeyChecking=no, ServerAliveInterval=30,
// ServerAliveCountMax=3) — the Option-driven refactor must not alter default
// behavior for existing callers.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Changing DefaultOptions()'s
// KeepAlive/KeepAliveCountMax/StrictHostKeyCheck defaults (e.g. to 0/0/true)
// reproduces RED.
func TestNET2_3_TunnelArgs_DefaultsUnchanged(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	host := remote.RemoteHost{Name: "h", Address: "10.0.0.1", User: "user", Port: 22}
	spec := TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemoteHost: "db-server",
		RemotePort: "5432",
	}

	args := mgr.tunnelArgs(host, spec)
	assert.Contains(t, args, "StrictHostKeyChecking=no")
	assert.Contains(t, args, "ServerAliveInterval=30")
	assert.Contains(t, args, "ServerAliveCountMax=3")
}

// TestNET2_3_TunnelArgs_CustomOptionsThreadedIn proves a custom
// WithKeepAlive/WithKeepAliveCountMax/WithStrictHostKeyCheck Option actually
// changes the built ssh args — i.e. tunnelArgs reads from m.opts, not from
// hardcoded literals.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting tunnelArgs to its
// pre-NET2-3 hardcoded form ("StrictHostKeyChecking=no",
// "ServerAliveInterval=30", "ServerAliveCountMax=3" as literals, ignoring
// m.opts) reproduces RED — the custom values below would never appear.
func TestNET2_3_TunnelArgs_CustomOptionsThreadedIn(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{},
		WithKeepAlive(7*time.Second),
		WithKeepAliveCountMax(9),
		WithStrictHostKeyCheck(true),
	)

	host := remote.RemoteHost{Name: "h", Address: "10.0.0.1", User: "user", Port: 22}
	spec := TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemoteHost: "db-server",
		RemotePort: "5432",
	}

	args := mgr.tunnelArgs(host, spec)
	assert.Contains(t, args, "StrictHostKeyChecking=yes")
	assert.Contains(t, args, "ServerAliveInterval=7")
	assert.Contains(t, args, "ServerAliveCountMax=9")
	assert.NotContains(t, args, "StrictHostKeyChecking=no")
}

// --- NET2-4: explicit LocalPort registered in the allocator ---------------

// TestNET2_4_CreateTunnel_ExplicitPort_RegisteredInAllocator proves an
// explicit-LocalPort tunnel is visible to the allocator's own bookkeeping
// (IsAllocated / AllocatedCount), and that CloseTunnel releases it correctly.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting CreateTunnel to
// skip the `m.allocator.MarkAllocated(...)` call for explicit ports
// reproduces RED — IsAllocated(port) stays false and AllocatedCount() stays 0
// even while the tunnel is live.
func TestNET2_4_CreateTunnel_ExplicitPort_RegisteredInAllocator(t *testing.T) {
	writeFakeSSH(t, "sleep 5")

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	require.Equal(t, 0, mgr.allocator.AllocatedCount(),
		"precondition: no ports allocated")

	explicitPort := "24999" // outside WithPortRange, proving MarkAllocated
	// does not require the port to fall inside [start,end).
	explicitPortNum, err := strconv.Atoi(explicitPort)
	require.NoError(t, err)

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  explicitPort,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	require.NotNil(t, info)

	assert.True(t, mgr.allocator.IsAllocated(explicitPortNum),
		"an explicit-port tunnel must be visible to the allocator's own "+
			"bookkeeping, not just m.tunnels")
	assert.Equal(t, 1, mgr.allocator.AllocatedCount())

	require.NoError(t, mgr.CloseTunnel(explicitPort))
	assert.False(t, mgr.allocator.IsAllocated(explicitPortNum),
		"CloseTunnel must release an explicit port's allocator reservation "+
			"exactly like an auto-allocated one")
	assert.Equal(t, 0, mgr.allocator.AllocatedCount())
}

// TestNET2_4_CreateTunnel_ExplicitPort_ReleasedOnCleanExit proves the
// reapTunnel side of NET2-4: an explicit-port tunnel whose ssh process exits
// cleanly WITHOUT the caller ever calling CloseTunnel must still have its
// allocator reservation released by the reaper — otherwise it leaks forever.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting reapTunnel's
// clean-exit release back to `if autoAllocatedPort >= 0 { ... }` (its
// pre-NET2-4 form) reproduces RED — IsAllocated(port) stays true forever
// after this explicit-port tunnel's process has already exited.
func TestNET2_4_CreateTunnel_ExplicitPort_ReleasedOnCleanExit(t *testing.T) {
	writeFakeSSH(t, "exit 0") // clean exit, immediately

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	explicitPort := "24998"
	explicitPortNum, err := strconv.Atoi(explicitPort)
	require.NoError(t, err)

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  explicitPort,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	require.NotNil(t, info)

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0 &&
			!mgr.allocator.IsAllocated(explicitPortNum)
	}, 3*time.Second, 10*time.Millisecond,
		"a cleanly-exited EXPLICIT-port tunnel must be reaped AND its "+
			"allocator reservation released, never leaked")
}

// --- NET2-5: TunnelOverlay.Connect is idempotent (no duplicate members) ---

// TestNET2_5_TunnelOverlay_Connect_DedupesRepeatedContainerID proves a
// repeated Connect for the same containerID does not duplicate its
// membership in the overlay network.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. Reverting Connect to its
// pre-NET2-5 unconditional-append form reproduces RED — members would be
// []string{"container-1", "container-1"} instead of []string{"container-1"}.
func TestNET2_5_TunnelOverlay_Connect_DedupesRepeatedContainerID(t *testing.T) {
	o := NewTunnelOverlay(nil, nil, nil, logging.NopLogger{})
	ctx := context.Background()

	require.NoError(t, o.Create(ctx, "net-a"))
	require.NoError(t, o.Connect(ctx, "net-a", "container-1"))
	require.NoError(t, o.Connect(ctx, "net-a", "container-1")) // repeat

	o.mu.Lock()
	members := append([]string(nil), o.networks["net-a"]...)
	o.mu.Unlock()

	assert.Equal(t, []string{"container-1"}, members,
		"a repeated Connect for the same container must not duplicate its "+
			"membership")
}

// TestNET2_5_TunnelOverlay_Connect_DistinctContainersStillBothAdded is the
// discriminator: dedup must be scoped to the SAME containerID — two DISTINCT
// containers connecting to the same network must both be recorded.
func TestNET2_5_TunnelOverlay_Connect_DistinctContainersStillBothAdded(t *testing.T) {
	o := NewTunnelOverlay(nil, nil, nil, logging.NopLogger{})
	ctx := context.Background()

	require.NoError(t, o.Create(ctx, "net-b"))
	require.NoError(t, o.Connect(ctx, "net-b", "container-1"))
	require.NoError(t, o.Connect(ctx, "net-b", "container-2"))

	o.mu.Lock()
	members := append([]string(nil), o.networks["net-b"]...)
	o.mu.Unlock()

	assert.ElementsMatch(t, []string{"container-1", "container-2"}, members)
}

// --- NET2-6: tunnel ssh runs strictly non-interactively (BatchMode=yes) ----

// TestWave20_NET2_TunnelArgs_BatchModeNonInteractive proves the tunnel ssh
// command is built with `-o BatchMode=yes`, matching the two OTHER ssh-arg
// builders in this module (pkg/remote.SSHExecutor.sshArgs and
// pkg/egress.buildDynamicForwardArgs, both of which hardcode + test it).
// tunnelArgs was the lone omission. Without BatchMode, a tunnel whose auth
// needs a password/passphrase, or a host-key confirmation prompt
// (StrictHostKeyChecking=ask, ssh's compiled default), makes ssh read from the
// controlling terminal and BLOCK INDEFINITELY (ssh prompts on /dev/tty,
// bypassing the child's /dev/null stdin) — the exact "never hang" liveness gap
// the fork/bind guards in this file close, left open on the auth path.
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. A surgical revert removing the
// `"-o", "BatchMode=yes"` element from tunnelArgs reproduces RED — the built
// arg vector no longer contains "BatchMode=yes".
func TestWave20_NET2_TunnelArgs_BatchModeNonInteractive(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	host := remote.RemoteHost{Name: "h", Address: "10.0.0.1", User: "user", Port: 22}
	spec := TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemoteHost: "db-server",
		RemotePort: "5432",
	}

	args := mgr.tunnelArgs(host, spec)

	// Mirrors the NET2-3 StrictHostKeyChecking / ServerAlive assertions above.
	assert.Contains(t, args, "BatchMode=yes",
		"the tunnel ssh command must run non-interactively (BatchMode=yes), "+
			"matching pkg/remote + pkg/egress, so it can never block on an "+
			"interactive auth/host-key prompt")

	// Discriminator (anti-tautology): BatchMode=yes must be the VALUE of an
	// `-o` option — i.e. the argv element immediately preceding it must be the
	// "-o" flag, not some unrelated bare token — so ssh actually parses it as
	// the BatchMode option rather than a positional argument.
	found := false
	for i := 1; i < len(args); i++ {
		if args[i] == "BatchMode=yes" && args[i-1] == "-o" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"BatchMode=yes must be passed as the value of an -o option (preceded "+
			"by the -o flag token)")
}

// TestWave20_NET2_TunnelArgs_BatchModeBothDirections proves the non-interactive
// hardening is applied for BOTH tunnel directions (local -L and remote -R),
// since either direction shells out an ssh child that could otherwise wedge on
// an interactive prompt. Complements the discriminator above by pinning that
// the fix is unconditional, not accidentally scoped to one Direction branch.
func TestWave20_NET2_TunnelArgs_BatchModeBothDirections(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})
	host := remote.RemoteHost{Name: "h", Address: "10.0.0.1", User: "user", Port: 22}

	for _, dir := range []TunnelDirection{TunnelLocal, TunnelRemote} {
		args := mgr.tunnelArgs(host, TunnelSpec{
			Direction:  dir,
			LocalPort:  "8080",
			RemotePort: "5432",
		})
		assert.Contains(t, args, "BatchMode=yes",
			"BatchMode=yes must be present for direction %q", dir)
	}
}
