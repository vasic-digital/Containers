package emulator

// Batch EMU-HARD (Wave-20) — §11.4.115 RED→GREEN behavioral guards for
// EMU-1 (HIGH, GENY-1/CF-1 class ctx-bounding) + EMU-4 (MED/HIGH,
// authorizeADB temp-dir leak + secret hygiene) over this package.
//
// Each guard is GREEN against the fixed code and, per §11.4.115, was
// proven RED by a surgical revert of the corresponding fix (captured
// `--- FAIL` line recorded in the Bluff-Audit blocks below). Guards
// drive the injectable CommandExecutor seam — no real adb/emulator
// (§11.4.27).
//
// Bluff-Audit:
//
//	Mutation (EMU-1, WaitForBoot): reverted `cctx, cancel :=
//	          context.WithDeadline(ctx, deadline); defer cancel()` in
//	          AndroidEmulator.WaitForBoot, restoring the raw caller `ctx`
//	          for both the `adb connect` + `adb shell getprop` Execute
//	          calls and the select's `case <-ctx.Done()`.
//	Observed (actual captured output, surgical revert ACTUALLY applied +
//	          reverted per §11.4.115):
//	            === RUN   TestWaitForBoot_EMU1_deadlineBoundsWedgedAdb
//	                wave20_emuhard_test.go:123: EMU-1: WaitForBoot HUNG
//	                past 3s on a wedged adb call — timeout not enforced
//	                against caller context.Background() (pre-fix behavior)
//	            --- FAIL: TestWaitForBoot_EMU1_deadlineBoundsWedgedAdb (3.00s)
//	          (a context.Background() caller ctx never fires Done, so the
//	          emu1BlockingExecutor's `<-ctx.Done()` in the first `adb
//	          connect` call never unblocks, and the 500ms `timeout`
//	          argument is silently not honored.)
//	Reverted: yes — restored, re-ran, GREEN again (0.51s).
//
//	Mutation (EMU-1, Teardown): reverted the first poll loop's
//	          `cctx, cancel := context.WithDeadline(ctx, deadline);
//	          defer cancel()` in AndroidEmulator.Teardown (plus the two
//	          leftover comment lines that referenced it), restoring the
//	          raw caller `ctx` for the `adb devices` poll Execute call and
//	          the select's `case <-ctx.Done()`.
//	Observed (actual captured output, surgical revert ACTUALLY applied +
//	          reverted per §11.4.115):
//	            === RUN   TestTeardown_EMU1_deadlineBoundsWedgedAdbDevicesPoll
//	                wave20_emuhard_test.go:174: EMU-1c: Teardown HUNG past
//	                3s on a wedged `adb devices` poll — timeout not
//	                enforced against caller context.Background()
//	                (pre-fix behavior)
//	            --- FAIL: TestTeardown_EMU1_deadlineBoundsWedgedAdbDevicesPoll (3.00s)
//	          (the `adb emu kill` call succeeds immediately as designed by
//	          emu1TeardownExecutor, but the SUBSEQUENT `adb devices` poll
//	          call blocks forever on the never-firing raw ctx.)
//	Reverted: yes — restored, re-ran, GREEN again (0.50s).
//
//	Mutation (EMU-4): removed the `c.removeADBKeyTmpDir()` call from the
//	          non-empty-containerName path of Containerized.Teardown (the
//	          production defect this fixes: authorizeADB creates + tracks
//	          the temp dir, but nothing ever reaps it).
//	Observed (actual captured output, surgical revert ACTUALLY applied +
//	          reverted per §11.4.115):
//	            === RUN   TestContainerized_EMU4_authorizeADBTempDirReapedByTeardown
//	                wave20_emuhard_test.go:228: cycle 0: expected
//	                c.adbKeyTmpDir cleared after Teardown, got
//	                "/tmp/.private/milos/lava-emu-adbkey-564720074"
//	            --- FAIL: TestContainerized_EMU4_authorizeADBTempDirReapedByTeardown
//	          (WaitForBoot->authorizeADB created + tracked a temp dir
//	          holding a copy of the guest's private adb key; with the
//	          cleanup call removed, Teardown never cleared c.adbKeyTmpDir
//	          nor removed the directory from disk — an unbounded
//	          per-cycle leak of private key material.)
//	Reverted: yes — restored, re-ran, GREEN again (0.00s); the orphaned
//	          temp dir the RED run created was removed as part of restoring
//	          a clean working tree.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// emu1BlockingExecutor is a CommandExecutor whose Execute call blocks
// until the ctx it is ACTUALLY INVOKED WITH is Done, then returns
// ctx.Err(). It never unblocks on its own — the only way it returns is
// cancellation/deadline of the specific ctx passed to Execute. This is
// exactly what EMU-1 tests: whether a wait's `timeout` argument
// propagates into the underlying Execute call, or whether the wait
// invokes Execute with the caller's raw (possibly non-cancelling) ctx.
//
// Mirrors pkg/cuttlefish's CF-1 blockingExecutor pattern.
type emu1BlockingExecutor struct{}

func (emu1BlockingExecutor) Execute(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (emu1BlockingExecutor) Start(ctx context.Context, _ string, _ ...string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestWaitForBoot_EMU1_deadlineBoundsWedgedAdb — AndroidEmulator.WaitForBoot
// MUST bound a wedged adb call (both the per-iteration `adb connect` and
// `adb shell getprop`) by its own `timeout`, even when the caller passes
// context.Background() (the real callers' actual argument).
func TestWaitForBoot_EMU1_deadlineBoundsWedgedAdb(t *testing.T) {
	a := NewAndroidEmulatorWithExecutor("/sdk", emu1BlockingExecutor{})

	type res struct {
		d   time.Duration
		err error
	}
	done := make(chan res, 1)
	started := time.Now()
	go func() {
		d, werr := a.WaitForBoot(context.Background(), 5555, 500*time.Millisecond)
		done <- res{d, werr}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("EMU-1: expected a timeout error, got nil (elapsed %s)", r.d)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("EMU-1: returned but took %s (>2s) for a 500ms timeout — deadline not enforced against a wedged adb call", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EMU-1: WaitForBoot HUNG past 3s on a wedged adb call — timeout not enforced against caller context.Background() (pre-fix behavior)")
	}
}

// emu1TeardownExecutor lets the initial `adb emu kill` succeed
// immediately (matching real adb's near-instant "OK: killing emulator,
// bye bye" response), then blocks the SUBSEQUENT `adb devices` poll
// calls on the ctx ACTUALLY PASSED to Execute — isolating the guard to
// exactly the poll loop under test (EMU-1c) rather than hanging before
// the loop is ever reached.
type emu1TeardownExecutor struct{}

func (emu1TeardownExecutor) Execute(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[len(args)-1] == "kill" {
		return []byte("OK: killing emulator, bye bye\n"), nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (emu1TeardownExecutor) Start(ctx context.Context, _ string, _ ...string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestTeardown_EMU1_deadlineBoundsWedgedAdbDevicesPoll — AndroidEmulator.
// Teardown's first `adb devices` poll loop MUST bound a wedged `adb
// devices` call by teardownGracePeriod, even when the caller passes
// context.Background().
func TestTeardown_EMU1_deadlineBoundsWedgedAdbDevicesPoll(t *testing.T) {
	// teardownGracePeriod is a documented package-level test seam (see the
	// NOTE block at the top of android.go); per that contract this test
	// MUST NOT call t.Parallel().
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 500 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	a := NewAndroidEmulatorWithExecutor("/sdk", emu1TeardownExecutor{})

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- a.Teardown(context.Background(), 5555)
	}()

	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("EMU-1c: Teardown returned but took %s (>2s) for a %s teardownGracePeriod — deadline not enforced against a wedged `adb devices` poll", elapsed, teardownGracePeriod)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EMU-1c: Teardown HUNG past 3s on a wedged `adb devices` poll — timeout not enforced against caller context.Background() (pre-fix behavior)")
	}
}

// TestContainerized_EMU4_authorizeADBTempDirReapedByTeardown — every
// WaitForBoot->authorizeADB call creates a host temp dir holding a copy
// of the container's PRIVATE adb key; Teardown MUST remove it so N
// boot/teardown cycles leave 0 leaked lava-emu-adbkey-* directories.
func TestContainerized_EMU4_authorizeADBTempDirReapedByTeardown(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555":                             {Out: []byte("connected\n")},
			"adb -s localhost:5555 shell getprop sys.boot_completed": {Out: []byte("1\n")},
			// LVA-014 fix #2: WaitForBoot's container-liveness check runs
			// before the first getprop poll once containerName is set;
			// answer "running" for each cycle's container so the poll
			// proceeds to the (successful) getprop.
			livenessInspectKey("lava-emu-emu4-test-0"): {Out: []byte("true 0\n")},
			livenessInspectKey("lava-emu-emu4-test-1"): {Out: []byte("true 0\n")},
			livenessInspectKey("lava-emu-emu4-test-2"): {Out: []byte("true 0\n")},
		},
	}
	c, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	require.NoError(t, err)

	// Precise per-cycle path tracking (rather than a global os.TempDir()
	// glob) is deliberate: a sibling test in this package
	// (TestContainerized_authorizeADB_CopiesBakedKeyAndSetsVendorKeys)
	// calls authorizeADB directly and never Teardown, so it leaks its own
	// lava-emu-adbkey-* dir independently of this fix — a pre-existing,
	// out-of-scope test-hygiene gap unrelated to EMU-4. A global glob
	// would make this guard flaky against that unrelated leak (and
	// against any other concurrent `go test` process using the same
	// shared os.TempDir()); asserting on the EXACT path this test itself
	// created avoids that cross-test interference entirely.
	const cycles = 3
	createdDirs := make([]string, 0, cycles)
	for i := 0; i < cycles; i++ {
		// Simulate Boot having set the container name (authorizeADB
		// requires it); Boot itself isn't under test here.
		c.containerName = fmt.Sprintf("lava-emu-emu4-test-%d", i)
		if _, err := c.WaitForBoot(context.Background(), 5555, 2*time.Second); err != nil {
			t.Fatalf("cycle %d: WaitForBoot: %v", i, err)
		}
		tmpDir := c.adbKeyTmpDir
		if tmpDir == "" {
			t.Fatalf("cycle %d: expected authorizeADB to have created + tracked a temp dir, c.adbKeyTmpDir is empty", i)
		}
		if _, statErr := os.Stat(tmpDir); statErr != nil {
			t.Fatalf("cycle %d: c.adbKeyTmpDir %q does not exist on disk right after WaitForBoot: %v", i, tmpDir, statErr)
		}
		createdDirs = append(createdDirs, tmpDir)

		if err := c.Teardown(context.Background(), 0); err != nil {
			t.Fatalf("cycle %d: Teardown: %v", i, err)
		}
		if got := c.adbKeyTmpDir; got != "" {
			t.Fatalf("cycle %d: expected c.adbKeyTmpDir cleared after Teardown, got %q", i, got)
		}
	}

	for i, dir := range createdDirs {
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("EMU-4: cycle %d's temp dir %q still exists after Teardown (stat err=%v) — authorizeADB's private-key temp dir was not reaped", i, dir, statErr)
		}
	}
}
