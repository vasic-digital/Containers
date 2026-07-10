package serviceregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// srHard2Barrier bounds the SR-HARD-2 guard's block-vs-complete discrimination.
// It is pure margin: a reverted Clear() completes an os.Remove in microseconds,
// while a fixed Clear() cannot complete until the parked persist releases
// persistMu — so any value many orders of magnitude above a syscall separates
// the two categorically (never flaky). On the fixed (GREEN) path the barrier
// always elapses, which is why it is kept small.
var srHard2Barrier = 200 * time.Millisecond

// Wave-20 SR-HARD cluster guards.
//
// Each guard's verdict is DETERMINISTIC and never flaky (no goroutine-count-
// and-hope — §11.4.50). Guards use package-level test seams (nil in production)
// to force the exact interleaving that distinguishes the fixed code from the
// pre-fix code. The committed form defaults to a GREEN guard (§11.4.115): it
// PASSES on the fixed tree so `go test ./...` stays green, and surgically
// editing the corresponding fix back out reproduces RED.
//
//   - SR-HARD-1 uses persistBeforeLockHook (fires BEFORE persistMu). Because the
//     fix marshals AFTER persistMu, a persist parked here has NOT captured yet
//     and marshals the FRESH state on release; the reverted ordering captures a
//     STALE snapshot before the seam and writes it last. Pure timing-free:
//     the parked persist holds no lock, so a second persist runs to completion.
//   - SR-HARD-2 uses persistBeforeRenameHook (fires with persistMu held, a
//     non-empty snapshot already written to a temp file, just before the
//     rename). Clear()'s coordination is read via a bounded, categorical barrier
//     (see srHard2Barrier) — a fixed Clear BLOCKS on persistMu, a reverted Clear
//     completes immediately; the verdict is deterministic, never scheduling luck.

type warnCounterLogger struct {
	mu    sync.Mutex
	warns int
}

func (l *warnCounterLogger) Info(string, ...any)  {}
func (l *warnCounterLogger) Debug(string, ...any) {}
func (l *warnCounterLogger) Warn(string, ...any)  { l.mu.Lock(); l.warns++; l.mu.Unlock() }
func (l *warnCounterLogger) Error(string, ...any) {}
func (l *warnCounterLogger) count() int           { l.mu.Lock(); defer l.mu.Unlock(); return l.warns }

// readDiskServices reads and unmarshals the on-disk registry snapshot. A missing
// file yields an empty map (semantically: nothing persisted).
func readDiskServices(t *testing.T, dir string) map[string]*Service {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "services.json"))
	if os.IsNotExist(err) {
		return map[string]*Service{}
	}
	require.NoError(t, err)
	var m map[string]*Service
	require.NoError(t, json.Unmarshal(data, &m), "on-disk registry snapshot must be valid JSON")
	return m
}

// installPauseSeam wires persistBeforeLockHook so the FIRST persist() to reach
// the seam signals inHook and blocks until proceed is closed; every later
// persist() passes straight through. Returns a restore func the caller defers.
// MUST NOT be used with t.Parallel() (package-level var swap).
func installPauseSeam(inHook, proceed chan struct{}) func() {
	prev := persistBeforeLockHook
	var fired atomic.Bool
	persistBeforeLockHook = func() {
		if fired.CompareAndSwap(false, true) {
			close(inHook)
			<-proceed
		}
	}
	return func() { persistBeforeLockHook = prev }
}

// TestWave20_SRHARD1_StaleSnapshotOrdering_FreshestWins proves the SR-HARD-1 fix:
// snapshot-capture happens INSIDE persistMu, so two concurrent persists write in
// capture order and the freshest snapshot always lands last.
//
// Deterministic interleaving:
//  1. B (goroutine) enters persist(), parks at the seam (before persistMu, before
//     its marshal). Memory is {svc-x, svc-y}.
//  2. Test deletes svc-x from memory (now {svc-y}).
//  3. Test runs A (a full persist()) to completion — persistMu is free, so A
//     marshals {svc-y} and writes it. No deadlock.
//  4. Test releases B.
//     - FIXED  : B marshals AFTER release -> captures {svc-y} -> writes {svc-y}.
//     Final disk == memory {svc-y}.  PASS.
//     - PRE-FIX: B marshaled {svc-x, svc-y} BEFORE the seam -> writes it LAST,
//     clobbering A's fresh {svc-y}. Final disk == {svc-x, svc-y}.  FAIL
//     (svc-x resurrected after it was deleted).
func TestWave20_SRHARD1_StaleSnapshotOrdering_FreshestWins(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-x", 8001))
	require.NoError(t, r.Register("svc-y", 8002))

	inHook := make(chan struct{})
	proceed := make(chan struct{})
	defer installPauseSeam(inHook, proceed)()

	bDone := make(chan struct{})
	go func() { _ = r.persist(); close(bDone) }() // B: parks at the seam
	<-inHook

	// Mutate memory while B is parked (never via Unregister, which would persist).
	r.mu.Lock()
	delete(r.services, "svc-x")
	r.mu.Unlock()

	// A: a full persist of the fresh {svc-y}. persistMu is free (B parked before
	// acquiring it), so this completes with no deadlock on either ordering.
	require.NoError(t, r.persist())

	close(proceed) // release B
	<-bDone

	disk := readDiskServices(t, dir)
	mem := r.GetAll()

	assert.NotContains(t, disk, "svc-x",
		"SR-HARD-1: a stale pre-captured snapshot resurrected svc-x on disk after it was deleted")
	assert.Contains(t, disk, "svc-y", "svc-y must be present on disk (freshest state)")

	diskNames := make([]string, 0, len(disk))
	for k := range disk {
		diskNames = append(diskNames, k)
	}
	memNames := make([]string, 0, len(mem))
	for k := range mem {
		memNames = append(memNames, k)
	}
	assert.ElementsMatch(t, memNames, diskNames,
		"SR-HARD-1: final on-disk snapshot must equal the final in-memory map (freshest wins)")
}

// TestWave20_SRHARD2_ClearNotResurrectedByInFlightPersist proves the SR-HARD-2
// fix: Clear() removes the file under persistMu (and moves the blocking os.Remove
// out of r.mu), so an in-flight persist that already marshaled a non-empty
// snapshot cannot rename it back over the cleared file.
//
// A persist paused BEFORE persistMu (the SR-HARD-1 seam) would not reproduce
// this — the SR-HARD-1 fix makes it re-marshal AFTER Clear emptied memory. So
// this guard parks the persist at the SECOND seam: persistMu held, a non-empty
// snapshot already written to a temp file, immediately before the rename.
//
// Interleaving:
//  1. B enters persist(), marshals {svc-x} to a temp file, parks before the
//     rename (holding persistMu). Disk is still {svc-x} (from setup).
//  2. Clear() runs concurrently.
//     - FIXED  : Clear empties memory, then BLOCKS on persistMu until B's rename
//     commits; it then removes the file. Final disk: svc-x absent.  PASS.
//     - PRE-FIX: Clear removes the file NOW (no persistMu). When B is released its
//     rename recreates {svc-x}. Final disk: svc-x resurrected.  FAIL.
//
// The block-vs-complete distinction is read via srHard2Barrier — a categorical,
// never-flaky margin (a reverted Clear completes in microseconds; a fixed Clear
// cannot complete until B releases persistMu). On the fixed path the barrier
// elapses and B is released first, letting Clear's blocked os.Remove run last.
func TestWave20_SRHARD2_ClearNotResurrectedByInFlightPersist(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-x", 8001))
	require.Contains(t, readDiskServices(t, dir), "svc-x", "precondition: svc-x persisted")

	atRename := make(chan struct{})
	proceed := make(chan struct{})
	var fired atomic.Bool
	prev := persistBeforeRenameHook
	persistBeforeRenameHook = func() {
		if fired.CompareAndSwap(false, true) {
			close(atRename)
			<-proceed
		}
	}
	defer func() { persistBeforeRenameHook = prev }()

	bDone := make(chan struct{})
	go func() { _ = r.persist(); close(bDone) }() // B: parks before the rename
	<-atRename

	clearDone := make(chan struct{})
	go func() { r.Clear(); close(clearDone) }()

	select {
	case <-clearDone: // reverted: Clear removed the file with no persistMu
	case <-time.After(srHard2Barrier): // fixed: Clear is blocked on persistMu
	}

	close(proceed) // release B -> its rename commits
	<-bDone
	<-clearDone

	disk := readDiskServices(t, dir)
	assert.NotContains(t, disk, "svc-x",
		"SR-HARD-2: an in-flight persist resurrected a cleared service on disk")
	assert.Empty(t, r.GetAll(), "in-memory registry must remain cleared")
}

// TestWave20_SRHARD3_PersistFailureSurfaced proves the SR-HARD-3 fix: persist()
// returns its failure and Register propagates it (no silent success), and a
// normal Register still returns nil (negative control).
func TestWave20_SRHARD3_PersistFailureSurfaced(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))

	// Negative control: a healthy Register must NOT report a spurious error.
	require.NoError(t, r.Register("svc-ok", 8001),
		"negative control: a persistable Register must return nil")

	// Force persist() to fail deterministically: point registryDir under a path
	// whose parent is a regular file, so os.MkdirAll fails with ENOTDIR
	// (robust even when the test runs as root, unlike a chmod-based approach).
	badParent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badParent, []byte("x"), 0o644))
	r.mu.Lock()
	r.registryDir = filepath.Join(badParent, "sub")
	r.mu.Unlock()

	err := r.Register("svc-doomed", 8002)
	require.Error(t, err,
		"SR-HARD-3: Register must propagate persist() failure, not silently return nil")

	// The in-memory registration still stands — only the on-disk snapshot failed.
	_, ok := r.Get("svc-doomed")
	assert.True(t, ok, "in-memory registration should stand even when persistence fails")
}

// TestWave20_SRHARD3_EmptyRegistryDirWarnsOnce proves the SR-HARD-3 sub-fix: an
// empty registryDir (persist is a silent in-memory-only no-op) is surfaced at
// Warn — exactly once, no matter how many times persist is called.
func TestWave20_SRHARD3_EmptyRegistryDirWarnsOnce(t *testing.T) {
	lg := &warnCounterLogger{}
	r := New(WithRegistryDir(t.TempDir()), WithLogger(lg))

	// Simulate the os.Getwd()-failed-in-New condition (registryDir == "").
	r.mu.Lock()
	r.registryDir = ""
	r.mu.Unlock()

	require.Equal(t, 0, lg.count(), "precondition: no warn emitted before an empty-dir persist")

	r.persist()
	r.persist()
	r.persist()

	assert.Equal(t, 1, lg.count(),
		"SR-HARD-3: an empty-registryDir persist must warn exactly once (silent no-op made visible)")
}
