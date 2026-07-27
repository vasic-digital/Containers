package network

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
)

// TestWave20_Network_CreateTunnel_DeadOnArrivalIsQueryableAsFailed proves the
// dead-tunnel-failure fix (Wave-20 NET-HARD N-1).
//
// Historically CreateTunnel returned success with State=TunnelActive the
// instant ssh's cmd.Start() succeeded — i.e. once the ssh PROCESS forked —
// WITHOUT confirming the forward was actually established. NET2-2 (Wave-20
// NET2-HARD) closed that specific gap for TunnelLocal specs with a short,
// bounded local-bind confirmation immediately after Start(): this fake ssh
// (`exit 1`) never binds anything, so CreateTunnel itself now reports
// State=TunnelFailed synchronously, before reapTunnel's independent exit
// capture ever gets a chance to run. Because ssh (for real usage) also runs
// with ExitOnForwardFailure=yes, it exits NON-ZERO on its own an instant
// later when a forward genuinely cannot be set up (remote port already bound,
// auth / host-key mismatch) — that later, asynchronous death is what
// reapTunnel's waitErr-capture handles. A dead-on-arrival tunnel MUST NOT
// silently vanish from ListTunnels either way: the reaper must capture the
// non-zero exit, mark (or confirm) the tunnel State=TunnelFailed, and KEEP it
// queryable so a caller can detect the failure. This closes the
// "absence-of-error masks a dead tunnel" anti-bluff pattern.
//
// GREEN-polarity guard (§11.4.115): PASSes on the fixed tree; a surgical revert
// of the reapTunnel waitErr-capture + TunnelFailed-retain branch (reverting to
// the old `_ = entry.wait()` + unconditional delete) reproduces RED — the reaper
// silently deletes the entry, ListTunnels stays empty forever, and the bounded
// poll below times out.
//
// Deterministic (no timing flake): the fake ssh exits 1 immediately under a live,
// never-cancelled context, so the death is a genuine forward-failure death (NOT a
// ctx-driven teardown). State=TunnelFailed is a stable TERMINAL state — the entry
// is retained, never flips back — so the bounded poll observes a monotonic
// end state, not a transient.
func TestWave20_Network_CreateTunnel_DeadOnArrivalIsQueryableAsFailed(t *testing.T) {
	writeFakeSSH(t, "exit 1") // ssh dies non-zero on its own → forward failure

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
		// LocalPort empty → auto-allocated (so the retained-port invariant is
		// exercised).
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, TunnelFailed, info.State,
		"NET2-2: a local port this ssh double never binds must NOT be reported "+
			"Active, even synchronously from CreateTunnel's own return value")

	port, err := strconv.Atoi(info.Spec.LocalPort)
	require.NoError(t, err)

	// The dead-on-arrival tunnel must become observable as State=TunnelFailed
	// (not silently absent). Preserving the "entry ⇔ auto port allocated"
	// invariant, its port stays reserved until the caller CloseTunnel-s the
	// failed entry.
	require.Eventually(t, func() bool {
		tunnels := mgr.ListTunnels()
		if len(tunnels) != 1 {
			return false
		}
		return tunnels[0].State == TunnelFailed &&
			tunnels[0].Spec.LocalPort == info.Spec.LocalPort &&
			mgr.allocator.IsAllocated(port)
	}, 3*time.Second, 10*time.Millisecond,
		"a dead-on-arrival tunnel must be retained + queryable as State=TunnelFailed, "+
			"not silently deleted")

	// And a caller that detects the failure can clean it up: CloseTunnel removes
	// the failed entry and releases its retained port.
	require.NoError(t, mgr.CloseTunnel(info.Spec.LocalPort))
	require.Empty(t, mgr.ListTunnels(),
		"CloseTunnel on the failed entry must remove it")
	require.False(t, mgr.allocator.IsAllocated(port),
		"closing the failed tunnel must release its retained auto-allocated port")
}

// TestWave20_Network_CreateTunnel_CleanExitStillReaped is the discriminator
// guard: a CLEAN exit (ssh exits 0 — a normal close) must STILL be removed and
// its port released, i.e. the TunnelFailed retention is scoped strictly to
// non-zero forward-failure deaths and must not resurrect cleanly-closed tunnels.
// This pins the CLEAN vs FAILED boundary so a future change cannot broaden the
// retain branch into every exit.
func TestWave20_Network_CreateTunnel_CleanExitStillReaped(t *testing.T) {
	writeFakeSSH(t, "exit 0") // clean exit → today's remove-and-release behavior

	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
	})
	require.NoError(t, err)
	port, err := strconv.Atoi(info.Spec.LocalPort)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(mgr.ListTunnels()) == 0 && !mgr.allocator.IsAllocated(port)
	}, 3*time.Second, 10*time.Millisecond,
		"a cleanly-exited tunnel must be reaped (removed + port released), never retained as failed")
}
