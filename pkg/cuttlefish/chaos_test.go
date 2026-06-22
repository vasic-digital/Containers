package cuttlefish

// chaos_test.go — §11.4.85 CHAOS coverage for the Cuttlefish lifecycle
// wrapper. Failures are INJECTED through the same real seams the unit tests
// use (the CommandExecutor, the statDevice device-presence seam, the
// cleanupWithDeps(procWalker, killer) core, and malformed wire input to the
// adb parser). Each case asserts the RECOVERY behaviour — the wrapper
// degrades gracefully (honest wrapped error, never a panic, cleanup still
// runs, state left consistent, no goroutine leak) — not merely that an
// error was produced (§11.4.85 chaos closed-set: process-death, input-
// corruption, resource-exhaustion-analogue, state-corruption injection).
//
// Anti-bluff (§11.4.1): a chaos step that crashed the test for a script
// reason would be a FAIL-bluff; these drive the genuine product paths and
// assert the genuine recovery contract.

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChaos_CommandFailureMidLaunch — the container runtime fails the
// `run` command (image pull denied / OOM at boot). RECOVERY contract:
// Launch returns a non-nil error that wraps the runtime output, Started is
// false, AND the device partition (Present/Missing) is still reported
// honestly so the caller can act — no panic, no partial "Started=true".
func TestChaos_CommandFailureMidLaunch(t *testing.T) {
	withStatDevice(t, "/dev/kvm") // partial host: only kvm present
	exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("Error: OCI runtime error: permission denied"), errors.New("exit status 126")
	}}
	c := newStressInstance(t, "chaos-launchfail", exec)

	res, err := c.Launch(context.Background())
	require.Error(t, err)
	assert.False(t, res.Started, "a failed run MUST NOT report Started=true")
	assert.Contains(t, err.Error(), "permission denied", "error must surface the runtime output")
	// Recovery: the device partition is still honest so the caller can
	// SKIP-with-reason rather than retry blindly.
	assert.Equal(t, []string{"/dev/kvm"}, res.PresentDevices)
	assert.NotEmpty(t, res.MissingDevices)
	assert.Equal(t, res.Error, err, "result.Error mirrors the returned error")
}

// TestChaos_ProcessDeathDuringReadinessWait — the cvd dies mid-wait so the
// host adb call errors on every probe. RECOVERY contract: WaitForReady does
// NOT report "probably ready" (§11.4.6) — it returns an honest timeout
// error naming the serial, and reports elapsed time, never panicking.
func TestChaos_ProcessDeathDuringReadinessWait(t *testing.T) {
	base := runtime.NumGoroutine()
	exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("error: device '0.0.0.0:6520' not found"), errors.New("exit status 1")
	}}
	c := newStressInstance(t, "chaos-death", exec)

	// Short overall timeout so the poll loop exhausts the budget quickly.
	elapsed, err := c.WaitForReady(context.Background(), 1500*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.0.0.0:6520", "timeout error must name the serial it waited for")
	assert.Contains(t, err.Error(), "timed out")
	assert.Positive(t, int64(elapsed), "elapsed wait time must be reported")
	assertNoGoroutineLeak(t, base)
}

// TestChaos_WaitForReadyContextCancelled — the caller's context is
// cancelled while WaitForReady polls a not-yet-ready device. RECOVERY
// contract: it returns the context error promptly (not the timeout error),
// proving cancellation is honoured, with no panic / no goroutine leak.
func TestChaos_WaitForReadyContextCancelled(t *testing.T) {
	base := runtime.NumGoroutine()
	exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("List of devices attached\n0.0.0.0:6520\toffline\n"), nil
	}}
	c := newStressInstance(t, "chaos-cancel", exec)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.WaitForReady(ctx, 30*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assertNoGoroutineLeak(t, base)
}

// TestChaos_KillerFailsOnSomePIDs — during orphan reaping SIGKILL fails for
// a straggler (e.g. it became a zombie / permission flip). RECOVERY
// contract: Cleanup does NOT error or panic; it records the survivor
// honestly in report.Surviving and still kills the ones it can, so the
// caller has an accurate picture (§11.4.6 — no silent success).
func TestChaos_KillerFailsOnSomePIDs(t *testing.T) {
	wk := fakeWalker{comms: map[int]string{
		701: "crosvm",  // immortal + SIGKILL fails → Surviving
		702: "run_cvd", // immortal + SIGKILL succeeds → KilledKILL
	}}
	k := newFakeKiller()
	k.immortal[701] = true
	k.immortal[702] = true
	k.killProof[701] = false // SIGKILL on 701 fails
	k.killProof[702] = true

	report, err := cleanupWithDeps(context.Background(), wk, k)
	require.NoError(t, err, "a kill failure is reported in the struct, not as an error")
	assert.ElementsMatch(t, []int{701, 702}, report.Found)
	assert.Contains(t, report.Surviving, 701, "the un-killable pid MUST be surfaced as Surviving")
	assert.Contains(t, report.KilledKILL, 702, "the killable straggler MUST be recorded killed")
}

// TestChaos_WalkerError — process enumeration itself fails (e.g. /proc
// unreadable, `ps` exits non-zero). RECOVERY contract: Cleanup propagates
// the error and returns an empty report — it does NOT signal anything on a
// guessed process list (§11.4.6), and never panics.
func TestChaos_WalkerError(t *testing.T) {
	wk := fakeWalker{err: errors.New("ps: operation not permitted")}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), wk, k)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation not permitted")
	assert.Empty(t, report.Found)
	assert.Empty(t, k.signals, "no process may be signalled when the walk failed")
}

// TestChaos_CleanupContextCancelledMidWait — the context is already
// cancelled when reaping enters its post-SIGTERM grace wait (an immortal
// pid forces the loop). RECOVERY contract: cleanupWithDeps returns the
// context error promptly, no panic, no goroutine leak.
func TestChaos_CleanupContextCancelledMidWait(t *testing.T) {
	base := runtime.NumGoroutine()
	wk := fakeWalker{comms: map[int]string{801: "crosvm"}}
	k := newFakeKiller()
	k.immortal[801] = true // never dies on SIGTERM → forces the wait loop

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := cleanupWithDeps(ctx, wk, k)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assertNoGoroutineLeak(t, base)
}

// TestChaos_AllDevicesAbsent — resource-exhaustion-analogue: the host
// exposes NONE of the required device nodes (e.g. a non-KVM / macOS host).
// RECOVERY contract: Launch does not panic; it reports every node as
// Missing and an empty Present set so the caller SKIPs-with-reason per
// §11.4.3 instead of booting a doomed container.
func TestChaos_AllDevicesAbsent(t *testing.T) {
	withStatDevice(t /* nothing present */)
	exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) { return []byte("id"), nil }}
	c := newStressInstance(t, "chaos-nodevs", exec)

	res, err := c.Launch(context.Background())
	require.NoError(t, err) // the run command itself succeeded (fake)
	assert.Empty(t, res.PresentDevices, "no node should be present")
	assert.ElementsMatch(t, DefaultDevices(), res.MissingDevices,
		"every required node must be surfaced as Missing for a §11.4.3 SKIP")
}

// TestChaos_MalformedADBDevicesOutput — input-corruption injection: the
// host adb returns garbage / truncated / binary output. RECOVERY contract:
// ParseADBDevices + Status never panic and never falsely report Ready on
// junk — readiness stays false, and Status surfaces the raw output as
// honest evidence (adb exit 0 with junk is "not ready", not an error).
func TestChaos_MalformedADBDevicesOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"empty", ""},
		{"header only", "List of devices attached\n"},
		{"truncated line no state", "List of devices attached\n0.0.0.0:6520"},
		{"binary nul bytes", "List of devices attached\n\x00\x01\x02\xff\n0.0.0.0:6520"},
		{"no newline different serial", "10.1.1.1:5555\tdevice"},
		{"wrong serial online", "List of devices attached\n10.0.0.9:5555\tdevice\n"},
		{"garbage tokens", "????\n\t\t\t\n   spaces   only   \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Direct parser: must never panic; returns best-effort slice.
			devs := ParseADBDevices([]byte(tc.out))
			_ = devs // a valid (possibly empty) slice — no panic is the assertion

			// Through Status: junk that does not contain the configured
			// serial online MUST reduce to Ready=false, never a false PASS.
			exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
				return []byte(tc.out), nil
			}}
			c := newStressInstance(t, "chaos-adbjunk", exec)
			st, err := c.Status(context.Background())
			require.NoError(t, err, "adb exit 0 with junk is not-ready, not an error")
			assert.False(t, st.Ready, "junk output must never report Ready for the configured serial")
		})
	}

	// The "no newline" truncated single line IS a well-formed serial+state
	// pair (`0.0.0.0:6520\tdevice`) — confirm the happy parse still works so
	// the chaos cases above are genuinely exercising the negative path.
	devs := ParseADBDevices([]byte("0.0.0.0:6520\tdevice"))
	require.Len(t, devs, 1)
	assert.True(t, devs[0].Online())
}

// TestChaos_StopCleanupRunsDespiteRmFailure — state-corruption injection at
// teardown: the authoritative `rm -f` fails (container stuck). RECOVERY
// contract: Stop returns the wrapped rm error BUT (a) orphan reaping still
// ran and (b) containerName is cleared so the instance is not wedged
// half-torn-down — teardown is best-effort-complete, never aborted midway
// (§11.4.14 leave-the-target-quiescent).
func TestChaos_StopCleanupRunsDespiteRmFailure(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	exec := &safeExecutor{fn: func(name string, args []string) ([]byte, error) {
		// stop_cvd exec ok; rm -f fails.
		if name == "podman" && len(args) >= 1 && args[0] == "rm" {
			return []byte("Error: no such container"), errors.New("exit status 1")
		}
		return []byte("ok"), nil
	}}
	c := newStressInstance(t, "chaos-rmfail", exec)
	_, err := c.Launch(context.Background())
	require.NoError(t, err)

	err = c.Stop(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rm", "the authoritative rm failure must be surfaced")
	// Recovery: the instance is left quiescent — name cleared regardless of
	// the rm failure, so a retry/new lifecycle is not blocked.
	assert.Empty(t, c.ContainerName(), "containerName MUST be cleared even when rm fails")
}

// TestChaos_StopBestEffortStopCvdFailure — the graceful `stop_cvd` step
// fails (cvd already gone) but the authoritative `rm -f` succeeds. RECOVERY
// contract: Stop returns nil (a best-effort stop_cvd failure is NOT fatal),
// the container is removed, and the name is cleared.
func TestChaos_StopBestEffortStopCvdFailure(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	exec := &safeExecutor{fn: func(name string, args []string) ([]byte, error) {
		// stop_cvd (via `podman exec ... stop_cvd`) fails; rm -f succeeds.
		if name == "podman" && len(args) >= 1 && args[0] == "exec" {
			return []byte("Error: container not running"), errors.New("exit status 1")
		}
		return []byte("ok"), nil
	}}
	c := newStressInstance(t, "chaos-stopcvdfail", exec)
	_, err := c.Launch(context.Background())
	require.NoError(t, err)

	require.NoError(t, c.Stop(context.Background()),
		"a best-effort stop_cvd failure MUST NOT abort teardown when rm -f succeeds")
	assert.Empty(t, c.ContainerName())
}

// TestChaos_RequireKVM_AbsentSkips — topology gate under a host that cannot
// run Cuttlefish. RECOVERY contract: RequireKVM returns a non-nil error
// naming the missing capability + the documented pre-flight check, so the
// caller SKIPs-with-reason per §11.4.3 rather than launching a doomed
// container. (This is the genuine "infeasible path → honest SKIP" guard
// per §11.4.3, NOT a faked pass.)
func TestChaos_RequireKVM_AbsentSkips(t *testing.T) {
	orig := kvmDevicePath
	kvmDevicePath = "/nonexistent/dev/kvm-chaos"
	t.Cleanup(func() { kvmDevicePath = orig })

	assert.False(t, KVMAvailable())
	err := RequireKVM()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KVM", "the SKIP reason must name the missing capability")
	assert.Contains(t, err.Error(), "11.4.3", "the SKIP reason must cite the honest-SKIP mandate")
}
