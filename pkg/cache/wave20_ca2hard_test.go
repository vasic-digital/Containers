package cache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWave20CA1_VerifyParticipatesInSHALock_BlocksConcurrentRefresh is the
// §11.4.115 GREEN-polarity guard for Wave-20 CA-1: pre-fix, Verify's
// open→hash→(if drift) os.Rename(path, quarantine) sequence took NEITHER the
// in-process s.keymu(imageID) NOR the cross-process per-SHA flock that
// fetchAndPlace (Get/Refresh) uses — so a concurrent Refresh publishing a
// fresh GOOD blob for the SAME content address between Verify's hash and
// Verify's quarantine rename could have its good bytes yanked out from under
// it and quarantined based on Verify's STALE (pre-replacement) read.
//
// This uses TWO DIFFERENT imageIDs that resolve to the SAME sha256 content
// address (a dedup scenario), so the in-process per-imageID keymu ALONE
// cannot serialize Verify against Refresh — only the shared per-SHA flock
// can. verifyPostHashHook (a test-only, default-no-op seam, same convention
// as the package's existing syncFile/renameFile/syncDir seams) pauses Verify
// immediately after it has computed the hash and decided drift, but BEFORE
// it acts — giving this test a controlled window to prove a concurrent
// Refresh for the SAME SHA genuinely BLOCKS (rather than racing to
// completion) until Verify's critical section ends.
//
// HONEST BOUNDARY (§11.4.107/§11.4.123): the flock block is a real OS-level
// mutual-exclusion primitive, so "Refresh's HTTP fetch has not fired within
// the wait window" is not a scheduler-luck assertion — Refresh categorically
// CANNOT proceed past its own flock acquisition until Verify releases it.
// Overrides a package-level var, so NO t.Parallel().
func TestWave20CA1_VerifyParticipatesInSHALock_BlocksConcurrentRefresh(t *testing.T) {
	good := []byte("good, previously-verified content shared by two image IDs")
	var hits int64
	srv := newCountingHTTPServer(good, &hits)
	defer srv.Close()

	sha := sha256Hex(good)
	m := &Manifest{
		Version: 1,
		Images: []ImageEntry{
			{ID: "img-verify", URL: srv.URL, SHA256: sha, Size: int64(len(good)), Format: "qcow2"},
			{ID: "img-refresh", URL: srv.URL, SHA256: sha, Size: int64(len(good)), Format: "qcow2"},
		},
	}
	s := NewFilesystemStore(t.TempDir())
	ctx := context.Background()

	// 1) Prime the shared blob (1 HTTP fetch) then corrupt it on disk.
	path, err := s.Get(ctx, m, "img-verify")
	require.NoError(t, err)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))
	require.NoError(t, os.WriteFile(path, []byte("CORRUPT bit-rotted bytes"), 0o644))

	// 2) Wire the test-only hook to pause Verify mid-critical-section.
	hookEntered := make(chan struct{})
	proceed := make(chan struct{})
	var hookFired int32
	prevHook := verifyPostHashHook
	verifyPostHashHook = func() {
		if atomic.CompareAndSwapInt32(&hookFired, 0, 1) {
			close(hookEntered)
			<-proceed
		}
	}
	defer func() { verifyPostHashHook = prevHook }()

	// 3) Launch Verify (will detect drift, then block inside the hook).
	verifyDone := make(chan error, 1)
	go func() {
		verifyDone <- s.Verify(ctx, m, "img-verify")
	}()

	select {
	case <-hookEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Verify never reached the post-hash hook — test setup broken")
	}

	// 4) While Verify holds its critical section, launch Refresh for the
	//    OTHER imageID sharing the SAME sha. If CA-1 is fixed, Refresh must
	//    BLOCK on the shared per-SHA flock — its HTTP fetch must NOT fire.
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- s.Refresh(ctx, m, "img-refresh")
	}()

	select {
	case err := <-refreshDone:
		t.Fatalf("BUG CACHE-HARD CA-1: Refresh completed (err=%v) WHILE Verify was still mid-critical-section "+
			"for the same content address — Verify does not participate in the per-SHA lock", err)
	case <-time.After(300 * time.Millisecond):
		// Expected: Refresh is genuinely blocked on the flock.
	}
	require.Equal(t, int64(1), atomic.LoadInt64(&hits),
		"BUG CACHE-HARD CA-1: Refresh's HTTP re-fetch fired before Verify released its lock")

	// 5) Let Verify finish (quarantines the stale-read corrupt bytes), then
	//    let Refresh proceed — it must now succeed, publishing the fresh
	//    good blob undisturbed.
	close(proceed)

	select {
	case verr := <-verifyDone:
		require.Error(t, verr)
		assert.Contains(t, verr.Error(), "drift")
	case <-time.After(5 * time.Second):
		t.Fatal("Verify never returned after being unblocked")
	}

	select {
	case rerr := <-refreshDone:
		require.NoError(t, rerr, "Refresh must succeed once Verify releases the shared per-SHA lock")
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh never returned after Verify released the lock")
	}
	require.Equal(t, int64(2), atomic.LoadInt64(&hits), "Refresh must have performed exactly one re-fetch")

	// 6) The fresh good blob published by Refresh must be intact — NOT
	//    quarantined based on Verify's stale pre-replacement read.
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "BUG CACHE-HARD CA-1: the fresh GOOD blob was removed/quarantined")
	assert.Equal(t, good, got, "BUG CACHE-HARD CA-1: the fresh GOOD blob's bytes were disturbed")
}

// TestWave20CA2_VerifyQuarantineUsesRenameFileSeamThenSyncsDir is the
// §11.4.14/§11.4.115 guard for Wave-20 CA-2: pre-fix, Verify's quarantine
// rename used a raw os.Rename call (bypassing the package's renameFile
// indirection seam entirely) and was never followed by a syncDir(blobsDir)
// call — unlike fetchAndPlace's publish rename, which fsyncs the containing
// directory so the rename's directory-entry metadata survives a crash.
// os.Rename gives atomicity, not durability: a crash between the quarantine
// rename and a missing dir-fsync can leave the corrupt blob back at `final`
// on recovery, silently defeating the Wave-20 C-2 quarantine fix. Fully
// sequential — no goroutine/scheduler luck. Overrides package-level vars, so
// NO t.Parallel().
func TestWave20CA2_VerifyQuarantineUsesRenameFileSeamThenSyncsDir(t *testing.T) {
	body := []byte("good bytes for CA-2 durability check")
	var hits int64
	srv := newCountingHTTPServer(body, &hits)
	defer srv.Close()

	s := NewFilesystemStore(t.TempDir())
	m, _ := newSingleImageManifest(t, "ca2", srv.URL, body)
	ctx := context.Background()

	path, err := s.Get(ctx, m, "ca2")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("corrupt bytes for CA-2"), 0o644))

	var (
		mu     sync.Mutex
		events []string
	)
	record := func(name string) {
		mu.Lock()
		events = append(events, name)
		mu.Unlock()
	}
	prevRename, prevSyncDir := renameFile, syncDir
	renameFile = func(oldpath, newpath string) error { record("rename"); return os.Rename(oldpath, newpath) }
	syncDir = func(dir string) error { record("dir_sync"); return fsyncDir(dir) }
	defer func() { renameFile, syncDir = prevRename, prevSyncDir }()

	verr := s.Verify(ctx, m, "ca2")
	require.Error(t, verr, "Verify must surface the SHA drift")
	assert.Contains(t, verr.Error(), "drift")

	mu.Lock()
	defer mu.Unlock()
	idx := func(name string) int {
		for i, e := range events {
			if e == name {
				return i
			}
		}
		return -1
	}
	ri, di := idx("rename"), idx("dir_sync")
	require.GreaterOrEqualf(t, ri, 0,
		"BUG CACHE-HARD CA-2: Verify's quarantine did NOT go through the renameFile seam (raw os.Rename used instead); recorded ops=%v", events)
	require.GreaterOrEqualf(t, di, 0,
		"BUG CACHE-HARD CA-2: Verify's quarantine rename was NOT followed by a blobsDir fsync — the quarantine is not durable; recorded ops=%v", events)
	assert.Lessf(t, ri, di,
		"the blobs dir must be fsync'd AFTER the quarantine rename publishes the sidecar; recorded ops=%v", events)

	assert.FileExists(t, path+".corrupt")
}

// TestWave20CA3_SweepsStaleIncomingTempFilesOnConstruction is the §11.4.14
// guard for Wave-20 CA-3: crash-orphaned "incoming-*" temp files (left behind
// when a process is SIGKILL'd/OOM-killed between os.CreateTemp and
// fetchToTemp's terminal rename-or-remove) were never swept — for multi-GB
// qcow2/Android images this is an unbounded disk leak across restarts. Fixed
// by sweeping leftover "incoming-*" entries in blobsDir once, synchronously,
// inside NewFilesystemStore — strictly BEFORE the constructed store is ever
// handed to a caller, so no concurrent fetchToTemp can possibly be creating a
// NEW incoming-* file yet.
func TestWave20CA3_SweepsStaleIncomingTempFilesOnConstruction(t *testing.T) {
	body := []byte("fresh bytes for CA-3 sweep check")
	var hits int64
	srv := newCountingHTTPServer(body, &hits)
	defer srv.Close()

	root := t.TempDir()
	blobsDir := filepath.Join(root, "blobs", "sha256")
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))
	strayPath := filepath.Join(blobsDir, "incoming-orphaned12345")
	require.NoError(t, os.WriteFile(strayPath, []byte("orphaned partial download from a crashed process"), 0o644))
	// A non-"incoming-" file in the same dir must NOT be touched by the sweep.
	unrelatedPath := filepath.Join(blobsDir, "not-an-incoming-file")
	require.NoError(t, os.WriteFile(unrelatedPath, []byte("unrelated"), 0o644))

	s := NewFilesystemStore(root)

	_, strayStatErr := os.Stat(strayPath)
	require.Truef(t, os.IsNotExist(strayStatErr),
		"BUG CACHE-HARD CA-3: stray incoming-* temp file %s was NOT swept at construction — unbounded disk leak across restarts", strayPath)
	assert.FileExists(t, unrelatedPath, "the sweep must only remove incoming-* entries, nothing else")

	m, hash := newSingleImageManifest(t, "ca3", srv.URL, body)
	path, err := s.Get(context.Background(), m, "ca3")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(blobsDir, hash), path)
	got, _ := os.ReadFile(path)
	assert.Equal(t, body, got)
	assert.Equal(t, int64(1), atomic.LoadInt64(&hits))
}

// TestWave20CA4_QuarantineVersionsNamesInsteadOfClobbering is the §11.4.14
// guard for Wave-20 CA-4: pre-fix, the quarantine rename target was the fixed
// name path+".corrupt" — rename(2) silently OVERWRITES an existing file at
// that target, so a second corruption/Verify cycle destroyed the first
// forensic sample. Fixed by versioning the quarantine sidecar name (an
// incrementing suffix) so every quarantine event's sample survives. Fully
// sequential — no goroutine/scheduler luck.
func TestWave20CA4_QuarantineVersionsNamesInsteadOfClobbering(t *testing.T) {
	good := []byte("good, previously-verified bytes for CA-4")
	var hits int64
	srv := newCountingHTTPServer(good, &hits)
	defer srv.Close()

	s := NewFilesystemStore(t.TempDir())
	m, _ := newSingleImageManifest(t, "ca4", srv.URL, good)
	ctx := context.Background()

	path, err := s.Get(ctx, m, "ca4")
	require.NoError(t, err)

	// First corruption + Verify → first quarantine sample.
	corrupt1 := []byte("CORRUPT sample #1")
	require.NoError(t, os.WriteFile(path, corrupt1, 0o644))
	verr1 := s.Verify(ctx, m, "ca4")
	require.Error(t, verr1)
	require.FileExists(t, path+".corrupt")
	sample1, _ := os.ReadFile(path + ".corrupt")
	require.Equal(t, corrupt1, sample1)

	// Re-fetch a fresh good copy (2nd HTTP hit) so `path` exists again.
	path2, err := s.Get(ctx, m, "ca4")
	require.NoError(t, err)
	require.Equal(t, path, path2)
	require.Equal(t, int64(2), atomic.LoadInt64(&hits))

	// Second corruption + Verify → MUST NOT clobber the first quarantine
	// sample; a second, distinctly-named sidecar must be created instead.
	corrupt2 := []byte("CORRUPT sample #2 DIFFERENT from #1")
	require.NoError(t, os.WriteFile(path, corrupt2, 0o644))
	verr2 := s.Verify(ctx, m, "ca4")
	require.Error(t, verr2)

	// BUG CA-4 pre-fix: os.Rename(path, path+".corrupt") OVERWRITES the
	// existing sidecar (rename(2) clobbers on conflict), silently losing the
	// first forensic sample.
	sample1Again, readErr1 := os.ReadFile(path + ".corrupt")
	require.NoError(t, readErr1, "the FIRST quarantine sample must survive a second quarantine event")
	assert.Equal(t, corrupt1, sample1Again,
		"BUG CACHE-HARD CA-4: the first .corrupt sidecar was clobbered by the second quarantine rename")

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	var corruptSidecars []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(path)+".corrupt") {
			corruptSidecars = append(corruptSidecars, e.Name())
		}
	}
	require.Lenf(t, corruptSidecars, 2,
		"expected exactly 2 distinct quarantine sidecars after 2 corrupt/Verify cycles, found %v", corruptSidecars)

	foundSecond := false
	for _, name := range corruptSidecars {
		data, _ := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if string(data) == string(corrupt2) {
			foundSecond = true
		}
	}
	assert.True(t, foundSecond, "the second corrupt sample's bytes must be preserved under its own distinctly-named sidecar")
}
