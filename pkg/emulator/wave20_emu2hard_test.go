package emulator

// Batch EMU-HARD (Wave-20) — §11.4.115 RED→GREEN behavioral guard for
// EMU-2 (HIGH, -race correctness): AndroidMatrixRunner.RunMatrix under
// MatrixConfig.Concurrent>1 shared ONE r.emulator across N goroutines, so
// AVD-B's Boot() overwrote the shared per-Boot state (Containerized's
// c.containerName) between AVD-A's Boot() and AVD-A's Teardown(), and
// AVD-A's `rm -f` then force-removed AVD-B's container (cross-attribution).
//
// The fix: NewAndroidMatrixRunnerWithFactory hands each runOne invocation a
// FRESH Emulator instance, so every concurrent goroutine owns its own
// per-Boot state and a per-AVD Teardown always acts on the container its
// own Boot created. Constructing a fresh instance per invocation also fixes
// EMU-3 (gradleModule cross-write) by construction — each instance carries
// its own gradleModule field.
//
// The guard below drives the injectable Emulator seam — NO real
// adb/emulator/podman (§11.4.27). emu2FakeEmulator mirrors Containerized's
// EXACT field-mutation shape: Boot writes an instance-local containerName
// (unlocked, like Containerized.Boot mutating c.containerName), Teardown
// reads it and records the name it would `rm -f` (like
// Containerized.Teardown acting on c.containerName, discarding its port
// arg). A boot barrier forces BOTH boots to complete before either
// teardown, making the shared-instance cross-attribution DETERMINISTIC
// (both teardowns then read whichever Boot wrote last) rather than
// scheduling-dependent. The containerName field is deliberately NOT
// mutex-guarded so `go test -race` also flags the shared-instance defect
// as a DATA RACE; with a fresh instance per invocation that field is only
// ever touched by a single goroutine, so the GREEN guard is race-clean.
//
// Bluff-Audit (§11.4.115 — surgical revert ACTUALLY applied + captured +
// reverted; the runner is factory-constructed so the revert is a genuine
// re-introduction of the shared-instance defect, not a nil deref):
//
//	Mutation (EMU-2): in AndroidMatrixRunner.RunMatrix concurrent path,
//	          HOISTED `em := r.emulatorForInvocation()` OUT of the
//	          `for avd := range queue` worker loop to BEFORE the worker
//	          goroutines are spawned, so a SINGLE factory-produced Emulator
//	          instance is shared across both workers — the exact
//	          shared-instance / unlocked-mutation shape of the EMU-2 defect.
//	Observed (actual captured output, surgical revert ACTUALLY applied +
//	          reverted; `go test ./pkg/emulator/... -race -count=1 -run
//	          TestRunMatrix_EMU2_ConcurrentTeardownActsOnOwnContainer`):
//	            ==================
//	            WARNING: DATA RACE
//	            Write at 0x00c0000161e8 by goroutine 11:
//	              ...(*emu2FakeEmulator).Boot()  wave20_emu2hard_test.go:153
//	              ...(*AndroidMatrixRunner).runOne()          matrix.go:216
//	              ...(*AndroidMatrixRunner).RunMatrix.func1()  matrix.go:561
//	            Previous write at 0x00c0000161e8 by goroutine 10:
//	              ...(*emu2FakeEmulator).Boot()  wave20_emu2hard_test.go:153
//	            ==================
//	            WARNING: DATA RACE
//	            Read at 0x00c0000161e8 by goroutine 10:
//	              ...(*emu2FakeEmulator).Teardown()  wave20_emu2hard_test.go:174
//	            Previous write at 0x00c0000161e8 by goroutine 11:
//	              ...(*emu2FakeEmulator).Teardown()  wave20_emu2hard_test.go:175
//	            ==================
//	            --- FAIL: TestRunMatrix_EMU2_ConcurrentTeardownActsOnOwnContainer (0.05s)
//	                Error: Not equal:
//	                  expected: map[string]int{"lava-emu-emu2avda":1, "lava-emu-emu2avdb":1}
//	                  actual  : map[string]int{"":1, "lava-emu-emu2avda":1}
//	                Messages: EMU-2: each AVD's Teardown MUST rm -f ITS OWN
//	                  container exactly once; cross-attribution (shared
//	                  instance) collapses these to the last AVD's container.
//	                  got=map[:1 lava-emu-emu2avda:1]
//	            FAIL	digital.vasic.containers/pkg/emulator	0.063s
//	          (both signals fired: the shared containerName field raced on the
//	          concurrent Boot writes AND on the concurrent Teardown read/clear,
//	          AND the teardowns cross-attributed — emu2avdb's container was
//	          NEVER torn down, and one teardown acted on an empty name because
//	          the sibling goroutine's Teardown had already cleared the shared
//	          field.)
//	Reverted: yes — moved `em := r.emulatorForInvocation()` back INSIDE the
//	          `for avd := range queue` loop; re-ran, GREEN + race-clean.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// emu2Recorder is the shared, mutex/channel-guarded sink the fake
// emulators coordinate through. Its own state is fully synchronized so it
// never itself produces a race in the GREEN (fresh-instance) path — the
// only field whose sharing is the defect-under-test is
// emu2FakeEmulator.containerName, which lives on the (per-invocation)
// instance, not here.
type emu2Recorder struct {
	mu        sync.Mutex
	teardowns []string // container names actually torn down (the `rm -f` arg)
	portSeq   int

	// boot barrier: blocks each Boot until all n boots have written their
	// containerName, so the shared-instance cross-attribution is
	// deterministic (both teardowns then read the last write).
	n       int
	arrived int
	release chan struct{}
}

func newEmu2Recorder(n int) *emu2Recorder {
	return &emu2Recorder{n: n, portSeq: 5554, release: make(chan struct{})}
}

// barrier blocks the calling Boot until all n boots have arrived.
func (r *emu2Recorder) barrier() {
	r.mu.Lock()
	r.arrived++
	if r.arrived == r.n {
		close(r.release)
	}
	r.mu.Unlock()
	<-r.release
}

func (r *emu2Recorder) nextPort() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.portSeq += 2
	return r.portSeq
}

func (r *emu2Recorder) recordTeardown(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.teardowns = append(r.teardowns, name)
}

// teardownCounts returns the multiset of container names passed to
// Teardown: name → count.
func (r *emu2Recorder) teardownCounts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]int, len(r.teardowns))
	for _, n := range r.teardowns {
		m[n]++
	}
	return m
}

// emu2FakeEmulator mirrors Containerized's per-Boot mutable-state shape.
type emu2FakeEmulator struct {
	rec *emu2Recorder

	// containerName mirrors Containerized.containerName: Boot writes it
	// (unlocked), Teardown reads it (unlocked). This is the EXACT field
	// whose cross-goroutine sharing is the EMU-2 defect. It is deliberately
	// NOT synchronized: with a fresh instance per runOne it is touched by a
	// single goroutine (race-clean); with ONE instance shared across the
	// worker pool (the defect) concurrent Boot writes + Teardown reads on it
	// race AND cross-attribute.
	containerName string
}

func (e *emu2FakeEmulator) Boot(_ context.Context, avd AVD, _ bool) (BootResult, error) {
	// Mirror Containerized.Boot mutating c.containerName with no lock.
	// sanitizeContainerName is the same helper Containerized.Boot uses.
	e.containerName = "lava-emu-" + sanitizeContainerName(avd.Name)
	port := e.rec.nextPort()
	// Force both boots to finish writing their containerName before either
	// advances to Teardown → deterministic cross-attribution under sharing.
	e.rec.barrier()
	return BootResult{AVD: avd, Started: true, ConsolePort: port, ADBPort: port + 1}, nil
}

func (e *emu2FakeEmulator) WaitForBoot(_ context.Context, _ int, _ time.Duration) (time.Duration, error) {
	return 0, nil
}

func (e *emu2FakeEmulator) Install(_ context.Context, _ int, _ string) error { return nil }

func (e *emu2FakeEmulator) RunInstrumentation(_ context.Context, _ int, _ string, _ time.Duration) (string, bool, error) {
	return "BUILD SUCCESSFUL", true, nil
}

func (e *emu2FakeEmulator) Teardown(_ context.Context, _ int) error {
	// Mirror Containerized.Teardown acting on c.containerName (NOT the port
	// arg, which Containerized discards): record the name it would `rm -f`.
	e.rec.recordTeardown(e.containerName)
	e.containerName = ""
	return nil
}

// compile-time proof the fake satisfies the seam it substitutes for.
var _ Emulator = (*emu2FakeEmulator)(nil)

// TestRunMatrix_EMU2_ConcurrentTeardownActsOnOwnContainer — under
// Concurrent=2 with two DISTINCT-named AVDs, each AVD's Teardown MUST act
// on the container its OWN Boot created, exactly once. With the EMU-2 fix
// (fresh instance per runOne via NewAndroidMatrixRunnerWithFactory) this is
// GREEN and race-clean; sharing one instance across the worker pool (the
// pre-fix defect) collapses both teardowns onto the last-booted AVD's
// container (see the Bluff-Audit block above for the captured RED/RACE).
func TestRunMatrix_EMU2_ConcurrentTeardownActsOnOwnContainer(t *testing.T) {
	// Distinct AVD names → distinct expected container names.
	avds := []AVD{
		{Name: "emu2avda", APILevel: 30},
		{Name: "emu2avdb", APILevel: 31},
	}
	rec := newEmu2Recorder(len(avds))

	// Factory yields a FRESH emu2FakeEmulator per runOne invocation (the
	// EMU-2 fix). Each concurrent goroutine owns its own containerName.
	runner := NewAndroidMatrixRunnerWithFactory(func() Emulator {
		return &emu2FakeEmulator{rec: rec}
	})

	dir := t.TempDir()
	apkPath := filepath.Join(dir, "app-debug.apk")
	require.NoError(t, os.WriteFile(apkPath, []byte("fake apk bytes"), 0o644))

	res, err := runner.RunMatrix(context.Background(), MatrixConfig{
		AVDs:        avds,
		APKPath:     apkPath,
		TestClass:   "lava.app.X",
		EvidenceDir: dir,
		Concurrent:  2,
	})
	require.NoError(t, err)
	require.Len(t, res.Tests, len(avds), "every AVD must get a row")

	got := rec.teardownCounts()
	want := map[string]int{
		"lava-emu-emu2avda": 1,
		"lava-emu-emu2avdb": 1,
	}
	require.Equalf(t, want, got,
		"EMU-2: each AVD's Teardown MUST rm -f ITS OWN container exactly once; "+
			"cross-attribution (shared instance) collapses these to the last AVD's container. got=%v", got)
}
