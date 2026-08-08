package cuttlefish

// Batch CF-HARD (Wave-20) — §11.4.115 RED→GREEN behavioral guards for a
// fresh §11.4.118 discovery pass over this package (CF-1 HIGH, CF-2/CF-3
// MED).
//
// Each guard is GREEN against the fixed code and, per §11.4.115, was proven
// RED by a surgical revert of the corresponding fix (captured `--- FAIL`
// line recorded in the conductor evidence block / commit body). Guards
// drive the injectable CommandExecutor seam / the unexported
// cleanupWithDepsGrace core — no real adb/podman/crosvm (§11.4.27).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingExecutor is a CommandExecutor whose Execute call blocks until the
// ctx it is ACTUALLY INVOKED WITH is Done, then returns ctx.Err(). It never
// unblocks on its own — the only way it returns is cancellation/deadline of
// the specific ctx passed to Execute. This is exactly what CF-1 tests:
// whether WaitForReady's `timeout` argument propagates into Status's
// underlying Execute call, or whether Status is invoked with the caller's
// raw (possibly non-cancelling) ctx.
type blockingExecutor struct{}

func (blockingExecutor) Execute(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingExecutor) Start(ctx context.Context, _ string, _ ...string) error {
	<-ctx.Done()
	return ctx.Err()
}

// CF-1 (HIGH) — WaitForReady MUST bound a wedged `adb devices` (Status) by
// its own timeout, even when the caller passes context.Background().
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation: in WaitForReady, revert `cctx, cancel := context.WithDeadline(ctx, deadline)`
//	          back to calling `c.Status(ctx)` directly on the raw caller ctx
//	          (drop the derived cctx entirely, mirroring the pre-fix code).
//	Observed: TestWaitForReady_CF1_deadlineBoundsWedgedStatus's outer 3s
//	          watchdog fires and reports:
//	            --- FAIL: TestWaitForReady_CF1_deadlineBoundsWedgedStatus (3.00s)
//	              wave20_cfhard_test.go:76: CF-1: WaitForReady HUNG past 3s on
//	              a wedged `adb devices` — timeout not enforced against caller
//	              context.Background() (pre-fix behavior)
//	          (a `context.Background()` caller ctx never fires Done, so a
//	          wedged Execute — stalled vsock transport / crashed crosvm / hung
//	          adb-server — blocks WaitForReady forever regardless of the
//	          `timeout` argument.)
//	Reverted: yes.
func TestWaitForReady_CF1_deadlineBoundsWedgedStatus(t *testing.T) {
	t.Parallel()
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, Executor: blockingExecutor{},
	})
	require.NoError(t, err)

	type res struct {
		d   time.Duration
		err error
	}
	done := make(chan res, 1)
	started := time.Now()
	go func() {
		d, werr := c.WaitForReady(context.Background(), 500*time.Millisecond)
		done <- res{d, werr}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("CF-1: expected a deadline error, got nil (elapsed %s)", r.d)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("CF-1: returned but took %s (>2s) for a 500ms timeout — deadline not enforced against a wedged exec", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CF-1: WaitForReady HUNG past 3s on a wedged `adb devices` — timeout not enforced against caller context.Background() (pre-fix behavior)")
	}
}

// CF-2 (MED) — Launch MUST NOT commit c.containerName (the internal
// ownership handle ContainerName() exposes) when the container run command
// fails. LaunchResult.ContainerName still carries the diagnostic name in
// both outcomes (public-contract-preserving, §11.4.122); it is the
// INTERNAL c.containerName — and therefore Stop()'s teardown path — that
// must stay unarmed after a failed Launch.
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation: in Launch, move `c.containerName = name` back to BEFORE the
//	          `c.executor.Execute(...)` call (its pre-fix position),
//	          removing the post-success-only assignment.
//	Observed:
//	  --- FAIL: TestLaunch_CF2_failedRunDoesNotCommitContainerName
//	    wave20_cfhard_test.go:135: CF-2: ContainerName() MUST be empty after
//	    a FAILED Launch — a container that was never created must not be
//	    reported as owned
//	    	Error Trace: wave20_cfhard_test.go:135
//	    	Error:      Should be empty, but was cvd-default
//	  (a second failure on the same run: c.Stop(ctx) then attempts
//	  `podman rm -f cvd-default` against a container that was never
//	  created, and — modelling a real runtime's "no such container"
//	  response — returns that rm error as Stop's own error.)
//	Reverted: yes.
func TestLaunch_CF2_failedRunDoesNotCommitContainerName(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	fe := &fakeExecutor{fn: func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			return []byte("permission denied"), errors.New("exit status 126")
		}
		if len(args) >= 2 && args[0] == "rm" && args[1] == "-f" {
			// Models a REAL container runtime's response when asked to
			// remove a container that was never created: it fails. This is
			// the observable consequence CF-2 fixes — pre-fix, Stop
			// attempts this rm because c.containerName was committed
			// despite the failed Launch.
			return []byte("Error: no such container"), errors.New("exit status 1")
		}
		return []byte("ok"), nil // best-effort stop_cvd exec inside Stop
	}}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true, Executor: fe,
	})
	require.NoError(t, err)

	res, launchErr := c.Launch(context.Background())
	require.Error(t, launchErr, "CF-2 baseline: the run command must fail")
	assert.False(t, res.Started, "CF-2: Launch must report Started=false on a run failure")
	assert.NotEmpty(t, res.ContainerName,
		"CF-2: LaunchResult.ContainerName MUST still carry the diagnostic name regardless of outcome (public contract unchanged)")

	assert.Empty(t, c.ContainerName(),
		"CF-2: ContainerName() MUST be empty after a FAILED Launch — a container that was never created must not be reported as owned")

	assert.NoError(t, c.Stop(context.Background()),
		"CF-2: Stop after a failed Launch MUST be a no-op (ContainerName empty) — it must NOT attempt `rm -f` against a container that was never created")
}

// CF-3 (MED, §11.4.124 investigate-before-remove) — Config.StopGracePeriod
// MUST be genuinely consumed by the teardown-wait mechanism its own doc
// comment describes, not merely validated + defaulted and then read by
// nothing. This drives the grace-period-parameterised core
// (cleanupWithDepsGrace, reached in production via
// CleanupWithGracePeriod — see Cuttlefish.Stop) directly, with an
// "immortal" process that never dies during the SIGTERM-wait window, so the
// measured wall-clock wait is bounded ONLY by the configured grace period.
// A short vs. long configured grace period MUST produce an observably
// different wait; pre-fix, both would take the same fixed 5s regardless of
// the value passed in (the field controlled nothing).
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation: in cleanupWithDepsGrace, revert
//	          `deadline := time.Now().Add(gracePeriod)` to the pre-fix
//	          literal `deadline := time.Now().Add(5 * time.Second)`, and
//	          `case <-time.After(pollInterval):` to the pre-fix literal
//	          `case <-time.After(250 * time.Millisecond):` (both configured-
//	          value parameters become dead — exactly the pre-fix defect).
//	Observed:
//	  --- FAIL: TestCleanupWithGracePeriod_CF3_gracePeriodControlsWait
//	    wave20_cfhard_test.go:206: CF-3: StopGracePeriod not consumed —
//	    short-grace wait (5.000...s) was not shorter than long-grace wait
//	    (5.000...s)
//	  (both the 120ms-configured and the 480ms-configured run took ~5s —
//	  the fixed hardcoded window, proving the parameter was accepted but
//	  ignored.)
//	Reverted: yes.
func TestCleanupWithGracePeriod_CF3_gracePeriodControlsWait(t *testing.T) {
	t.Parallel()
	newImmortalWalker := func(pid int) fakeWalker {
		return fakeWalker{
			comms:    map[int]string{pid: "crosvm"},
			cmdlines: map[int][]string{pid: {"crosvm", "--base_instance_num=cf3"}},
		}
	}

	const short = 120 * time.Millisecond
	const long = 480 * time.Millisecond

	measure := func(grace time.Duration, pid int) time.Duration {
		k := newFakeKiller()
		// Immortal AND never receives a SIGKILL during the loop itself (only
		// after the loop exits, per cleanupWithDepsGrace) — so Exists()
		// reports "still alive" for the entire wait window, forcing the loop
		// to run out the full configured grace period before giving up.
		k.immortal[pid] = true
		w := newImmortalWalker(pid)
		started := time.Now()
		_, err := cleanupWithDepsGrace(context.Background(), "--base_instance_num=cf3", w, k,
			grace, cleanupPollIntervalFor(grace))
		require.NoError(t, err)
		return time.Since(started)
	}

	shortElapsed := measure(short, 601)
	longElapsed := measure(long, 602)

	if shortElapsed >= longElapsed {
		t.Fatalf("CF-3: StopGracePeriod not consumed — short-grace wait (%s) was not shorter than long-grace wait (%s)", shortElapsed, longElapsed)
	}
	// Each measured wait roughly tracks its configured grace period (loose
	// bounds tolerate scheduler jitter/CI slowness) — proving the value
	// flows all the way to the actual deadline, not merely to a discarded
	// parameter.
	if shortElapsed < short/2 || shortElapsed > short*4 {
		t.Errorf("CF-3: short-grace (%s) wait was %s, want roughly proportional to the configured grace period", short, shortElapsed)
	}
	if longElapsed < long/2 || longElapsed > long*4 {
		t.Errorf("CF-3: long-grace (%s) wait was %s, want roughly proportional to the configured grace period", long, longElapsed)
	}
}
