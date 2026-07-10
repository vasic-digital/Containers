package network

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// writeFakeSSH installs a fake `ssh` binary (a small shell script running the
// given body) at the front of PATH for the duration of the test, so
// CreateTunnel's `exec.CommandContext(ctx, "ssh", ...)` launches it instead of
// the real ssh client. t.Setenv restores PATH on cleanup.
func writeFakeSSH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func wave17TunnelHost() *mockHostManager {
	return &mockHostManager{hosts: map[string]remote.RemoteHost{
		"gpu-1": {Name: "gpu-1", Address: "10.0.0.1", User: "deploy", Port: 22},
	}}
}

// TestWave17_Network_CreateTunnel_ReapsOnProcessDeath proves the reaper:
// when the tunnel's ssh process exits (dies / is dropped by the server), the
// tunnel MUST be removed from the active set and its auto-allocated port
// released — not linger forever in State=Active with a zombie child and a
// permanently-leaked port. RED on the pre-reaper source (polarity MN1 wraps the
// `go m.reapTunnel(...)` spawn in `if false {}` → this Eventually times out).
func TestWave17_Network_CreateTunnel_ReapsOnProcessDeath(t *testing.T) {
	writeFakeSSH(t, "exit 0") // ssh exits immediately → tunnel process dies

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
		// LocalPort empty → auto-allocated, so reaping must release it.
	})
	require.NoError(t, err)
	require.NotNil(t, info)

	port, err := strconv.Atoi(info.Spec.LocalPort)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0 && !mgr.allocator.IsAllocated(port)
	}, 3*time.Second, 10*time.Millisecond,
		"a dead tunnel process must be reaped and its auto-allocated port released")
}

// TestWave17_Network_CreateTunnel_RejectsExplicitPortCollision proves the
// atomic check-and-insert: a second CreateTunnel on the SAME explicit local
// port MUST be rejected, never silently overwrite the live tunnel's map entry
// (which orphans its running ssh process). RED on the pre-fix source (polarity
// MN2 disables the collision check → err2 becomes nil).
func TestWave17_Network_CreateTunnel_RejectsExplicitPortCollision(t *testing.T) {
	writeFakeSSH(t, "sleep 30") // ssh stays alive → tunnel 1 is genuinely live

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	spec := TunnelSpec{Direction: TunnelLocal, LocalPort: "19998", RemotePort: "5432"}

	info1, err1 := mgr.CreateTunnel(context.Background(), "gpu-1", spec)
	require.NoError(t, err1)
	require.NotNil(t, info1)

	info2, err2 := mgr.CreateTunnel(context.Background(), "gpu-1", spec)
	require.Error(t, err2, "explicit-port collision must be rejected, not overwrite the live tunnel")
	require.Nil(t, info2)

	// The original live tunnel is preserved untouched (not overwritten).
	tunnels := mgr.ListTunnels()
	require.Len(t, tunnels, 1)
	require.Equal(t, info1.PID, tunnels[0].PID,
		"the live tunnel's process must survive a rejected colliding request")
}

// TestWave17_Network_CloseTunnel_CoexistsWithReaper is the -race coexistence
// guard: CloseTunnel Kills+reaps the process while the background reaper
// goroutine is also waiting on the same process. Both must funnel the single
// cmd.Wait() through waitOnce (no concurrent/double Wait) and coordinate the
// map delete + single port release via the pointer-identity guard. The proof is
// a clean `go test -race` run plus the post-condition below.
func TestWave17_Network_CloseTunnel_CoexistsWithReaper(t *testing.T) {
	writeFakeSSH(t, "sleep 5") // alive when CloseTunnel fires → real coexistence

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	port, err := strconv.Atoi(info.Spec.LocalPort)
	require.NoError(t, err)

	// CloseTunnel may win the teardown (returns nil) or the reaper may have
	// reaped first (returns "no tunnel on port %s"); both are correct — the
	// tunnel is gone either way, exactly once.
	if cerr := mgr.CloseTunnel(info.Spec.LocalPort); cerr != nil {
		require.Contains(t, cerr.Error(), "no tunnel on port")
	}

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0 && !mgr.allocator.IsAllocated(port)
	}, 3*time.Second, 10*time.Millisecond,
		"after Close/reap coexistence the tunnel is gone and the port released exactly once")
}

// TestWave17_Network_CreateTunnel_ReapsOnContextCancel proves the reaper also
// fires on the ctx-cancel death path: exec.CommandContext kills the ssh process
// when ctx is cancelled, so the tunnel MUST be reaped (removed + auto port
// released) exactly as for a natural process exit. Deterministic — cancel()
// drives the death. RED on the pre-reaper source (polarity MN1).
func TestWave17_Network_CreateTunnel_ReapsOnContextCancel(t *testing.T) {
	writeFakeSSH(t, "sleep 30") // stays alive until ctx cancel kills it

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	ctx, cancel := context.WithCancel(context.Background())
	info, err := mgr.CreateTunnel(ctx, "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	port, err := strconv.Atoi(info.Spec.LocalPort)
	require.NoError(t, err)
	require.True(t, mgr.allocator.IsAllocated(port),
		"an auto-allocated port must be marked allocated while the tunnel is live")

	cancel() // ssh process is killed by CommandContext → reaper must reap

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0 && !mgr.allocator.IsAllocated(port)
	}, 3*time.Second, 10*time.Millisecond,
		"a ctx-cancelled tunnel process must be reaped and its auto port released")
}

// TestWave17_Network_CloseTunnel_AbsentPortReturnsSentinel proves CloseTunnel
// wraps errNoTunnel for an absent port. That sentinel is what CloseAll /
// CloseAllForHost match with errors.Is to treat a concurrently-reaped tunnel as
// success rather than a spurious failure (MINOR-1). Deterministic — no tunnel is
// ever created. RED if the sentinel wrap is reverted to a bare fmt.Errorf
// (polarity MN3): errors.Is no longer matches.
func TestWave17_Network_CloseTunnel_AbsentPortReturnsSentinel(t *testing.T) {
	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	err := mgr.CloseTunnel("40404")
	require.ErrorIs(t, err, errNoTunnel,
		"CloseTunnel on an absent port must wrap errNoTunnel so batch closers can ignore it")
}

// TestWave17_Network_CloseAll_CoexistsWithReaper is a -race guard for MINOR-1:
// several auto-tunnels whose ssh processes exit immediately (reapers firing)
// are torn down by CloseAll concurrently. CloseAll MUST NOT surface a spurious
// "already reaped" error, and every tunnel must end gone. This is a genuine
// race (reaper vs. CloseAll interleave), not a deterministic-polarity guard —
// the deterministic sentinel that underpins the fix is asserted above.
func TestWave17_Network_CloseAll_CoexistsWithReaper(t *testing.T) {
	writeFakeSSH(t, "exit 0") // processes die immediately → reapers race CloseAll

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	for i := 0; i < 5; i++ {
		_, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
			Direction:  TunnelLocal,
			RemotePort: "5432",
		})
		require.NoError(t, err)
	}

	require.NoError(t, mgr.CloseAll(),
		"CloseAll must treat concurrently-reaped tunnels as success, not error")

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0
	}, 3*time.Second, 10*time.Millisecond,
		"all tunnels gone after CloseAll + reaper coexistence")
}
