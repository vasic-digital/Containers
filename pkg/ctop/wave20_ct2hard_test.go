package ctop

// Wave-20 CT2-HARD — GREEN-polarity regression guards for a fresh read-only
// audit of pkg/ctop that surfaced 4 HIGH findings + 1 MED-HIGH honesty
// finding in Run()/Stop()/collectRemote(). Each finding was reproduced
// against the pre-fix code (captured `--- FAIL` / data-race / panic
// evidence — see the fix commit / PR description) via the package's
// existing fake-executor/self-signal seams (mockExecutor, ctopMockHostManager)
// BEFORE the fix landed, per §11.4.115 RED-baseline-on-the-broken-artifact +
// §11.4.146 reproduce-first. This file is the permanent GREEN-polarity guard
// (§11.4.135): every test here PASSES against the fixed code and would FAIL
// (or crash, for CT2-4's panic, or race, for CT2-3) if the corresponding fix
// were reverted.
//
// Findings covered:
//   - CT2-1 (HIGH): Run()'s render() synchronously blocks on
//     collector.Collect(ctx); while blocked, the select loop cannot read
//     sigChan, so SIGINT/SIGTERM sit unread and the TUI (and Ctrl+C) freezes
//     on a single wedged podman/docker subprocess call. Fixed by bounding
//     every refresh with a per-refresh ctx timeout (renderBounded /
//     boundedRenderTimeout in display.go).
//   - CT2-2 (HIGH): SIGWINCH shared sigChan with the quit signals, so a
//     terminal resize killed the TUI. Fixed by dropping SIGWINCH from the
//     quit Notify (render() already calls updateSize() every tick).
//   - CT2-3 (HIGH): `d.cancel = cancel` was written without holding d.mu
//     while Stop() reads it under d.mu — a data race in the canonical
//     `go Run()` + `Stop()` pattern. Fixed by publishing d.cancel under d.mu.
//   - CT2-4 (HIGH): `time.NewTicker(RefreshRate ms)` panics when
//     RefreshRate <= 0 (e.g. a partial DisplayConfig{} literal). Fixed by
//     sanitizeRefreshRate() in both display constructors and in Run().
//   - CT2-5 (MED-HIGH, §11.4.108 honesty): collectRemote() discarded every
//     per-host collectFromHost error and always returned a nil error, so
//     CollectorStats.Errors stayed 0 even on a total remote wipeout. Fixed
//     by counting per-host failures and surfacing them into
//     CollectorStats.Errors (plus a best-effort log.Printf per failure).

import (
	"bytes"
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/remote"
)

// ct2BlockingExecutor blocks Execute() until the supplied ctx is done,
// simulating a genuinely wedged podman/docker subprocess call (a hung
// daemon, an unreachable storage backend, etc.). It never returns on its
// own — only ctx cancellation (direct or via a bounding timeout) can
// unblock it, which is exactly the fake-executor seam CT2-1's repro needs.
type ct2BlockingExecutor struct{}

func (e *ct2BlockingExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestWave20_CT2_1_Run_UnwedgesOnSIGINT_WhileCollectBlocked reproduces the
// exact CT2-1 repro: a blockingExecutor whose Execute blocks until ctx
// cancel; `go d.Run(ctx)`; then a real SIGINT is delivered to this process
// WHILE Run()'s single goroutine is inside its first (permanently-blocked,
// pre-loop) render() call — i.e. NOT inside its `select`. Pre-fix, nothing
// ever calls cancel() in response to the signal sitting unread in sigChan,
// so Run() hangs forever and this test times out. Post-fix, renderBounded's
// per-refresh timeout expires, render() returns, the loop reaches its
// `select`, drains the already-queued sigChan entry, and Run() returns.
func TestWave20_CT2_1_Run_UnwedgesOnSIGINT_WhileCollectBlocked(t *testing.T) {
	exec := &ct2BlockingExecutor{}
	c := NewCollectorWithExecutor("podman", nil, exec)
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background())
	}()

	// Give Run() time to reach its pre-loop `d.renderBounded(ctx, ...)` call
	// — which is permanently wedged by ct2BlockingExecutor — so the signal
	// below is guaranteed to be delivered while Run()'s goroutine is NOT in
	// its `select` (the exact condition CT2-1 describes).
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case <-done:
		// Run() returned within the bound — CT2-1 fixed.
	case <-time.After(8 * time.Second):
		t.Fatal("CT2-1: Run() did not return within bound after SIGINT while wedged inside render()->Collect()->Execute()")
	}
}

// TestWave20_CT2_2_SIGWINCH_DoesNotQuitRun reproduces CT2-2: SIGWINCH must
// NOT be treated as a quit signal. Pre-fix, SIGWINCH shared sigChan with
// SIGINT/SIGTERM and the `case <-sigChan: return nil` branch fired on it,
// so a plain terminal resize killed the TUI. Post-fix, Run() keeps running
// after SIGWINCH and only quits on an explicit Stop()/SIGINT/SIGTERM.
func TestWave20_CT2_2_SIGWINCH_DoesNotQuitRun(t *testing.T) {
	exec := &mockExecutor{responses: map[string][]byte{"podman ps": []byte(`[]`)}}
	c := NewCollectorWithExecutor("podman", nil, exec)
	cfg := DefaultDisplayConfig()
	cfg.RefreshRate = 50
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, cfg, &buf)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background())
	}()

	// Let Run() get through setup and into its steady-state select loop.
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGWINCH))

	select {
	case err := <-done:
		t.Fatalf("CT2-2: Run() quit on SIGWINCH (err=%v) — a terminal resize must not kill the TUI", err)
	case <-time.After(400 * time.Millisecond):
		// Still running after the resize signal — correct.
	}

	d.Stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Stop()")
	}
}

// TestWave20_CT2_3_Stop_RacesWithCancelAssignment_NoDataRace reproduces
// CT2-3 under `go test -race`: Stop() reads d.cancel under d.mu; Run()
// writes d.cancel very early in its body. The functional PASS/FAIL of this
// test (Run() returning after Stop()) holds either way — the finding is a
// DATA RACE the race detector reports when d.cancel's write in Run() is not
// ALSO published under d.mu.
//
// It deliberately uses ct2BlockingExecutor (not the instant-response
// mockExecutor) so Run()'s first render() call is permanently wedged inside
// collector.Collect() at the moment Stop() is invoked, and NEVER reaches
// render()'s own (unrelated) d.mu critical section first. This matters:
// render() acquiring/releasing d.mu to read sortBy/sortOrder/etc. — AFTER
// the d.cancel write but BEFORE Stop()'s Lock() — would itself establish a
// happens-before edge to Stop()'s Lock() via ordinary mutex release/acquire
// semantics, laundering the very race we're trying to catch and making the
// guard flaky/blind. Keeping render() blocked isolates the race to exactly
// the d.cancel write vs Stop()'s read.
func TestWave20_CT2_3_Stop_RacesWithCancelAssignment_NoDataRace(t *testing.T) {
	exec := &ct2BlockingExecutor{}
	c := NewCollectorWithExecutor("podman", nil, exec)
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background())
	}()

	// d.cancel is assigned within the first few statements of Run(), well
	// before this sleep elapses on any realistic scheduler; Run()'s
	// goroutine is then stuck inside the wedged Collect() call, never
	// touching d.mu again until Stop() cancels it. Stop() reads d.cancel
	// here. `go test -race` flags the unsynchronized write vs the
	// mutex-guarded read as a data race unless Run() ALSO publishes
	// d.cancel under d.mu (CT2-3 fix).
	time.Sleep(50 * time.Millisecond)
	d.Stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after Stop()")
	}
}

// TestWave20_CT2_4_ZeroRefreshRate_DoesNotPanic reproduces CT2-4 exactly as
// audited: `NewDisplayWithWriter(collector, DisplayConfig{RefreshRate: 0},
// &buf).Run(ctx)`. Pre-fix this panics inside `time.NewTicker(0)`. Post-fix,
// sanitizeRefreshRate() clamps the zero value to DefaultDisplayConfig()'s
// rate (both at construction time and defensively inside Run()), so Run()
// runs normally and returns cleanly once ctx is cancelled.
func TestWave20_CT2_4_ZeroRefreshRate_DoesNotPanic(t *testing.T) {
	exec := &mockExecutor{responses: map[string][]byte{"podman ps": []byte(`[]`)}}
	c := NewCollectorWithExecutor("podman", nil, exec)
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DisplayConfig{RefreshRate: 0}, &buf)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	// Called synchronously (not via `go`) so an unfixed panic surfaces as a
	// normal, attributable test failure rather than an untraceable process
	// crash from a goroutine.
	err := d.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestWave20_CT2_5_RemoteHostFailure_SurfacesInStats reproduces CT2-5
// exactly as audited: a host is registered with no sshExecutor configured,
// so every collectFromHost call errors (a total remote wipeout). Pre-fix,
// collectRemote() silently discarded each per-host error and always
// returned a nil error, so CollectorStats.Errors stayed 0. Post-fix, the
// per-host failure count is surfaced into CollectorStats.Errors.
func TestWave20_CT2_5_RemoteHostFailure_SurfacesInStats(t *testing.T) {
	exec := &mockExecutor{responses: map[string][]byte{"podman ps": []byte(`[]`)}}

	hm := &ctopMockHostManager{
		hosts: map[string]remote.RemoteHost{
			"unreachable-1": {Name: "unreachable-1", Address: "10.0.0.99", User: "u"},
		},
	}

	// NewCollectorWithExecutor leaves sshExecutor nil, so collectFromHost
	// returns "SSH executor not configured" for every registered host — a
	// total remote wipeout, per the audit's exact repro.
	c := NewCollectorWithExecutor("podman", hm, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)

	stats := c.GetStats()
	assert.Greater(t, stats.Errors, 0,
		"CT2-5: a total remote-host wipeout (every host's collectFromHost failing) must surface as CollectorStats.Errors > 0, not silently vanish")
}

// TestWave20_CT2_5_RemoteHostFailure_MultiHost_CountsEachFailure strengthens
// CT2-5's guard: with N unreachable hosts, Errors must reflect (at least)
// that more than one host failed — not merely "some error happened
// somewhere" (which the old single collectRemote-level error already gave
// partial, ambiguous credit for).
func TestWave20_CT2_5_RemoteHostFailure_MultiHost_CountsEachFailure(t *testing.T) {
	exec := &mockExecutor{responses: map[string][]byte{"podman ps": []byte(`[]`)}}

	hm := &ctopMockHostManager{
		hosts: map[string]remote.RemoteHost{
			"unreachable-1": {Name: "unreachable-1", Address: "10.0.0.99", User: "u"},
			"unreachable-2": {Name: "unreachable-2", Address: "10.0.0.98", User: "u"},
		},
	}

	c := NewCollectorWithExecutor("podman", hm, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)

	stats := c.GetStats()
	assert.GreaterOrEqual(t, stats.Errors, 2,
		"CT2-5: both unreachable hosts must be counted, not collapsed into a single generic error")
}
