package cache

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWave20_CACHE2_VerifyRemoveFallbackFsyncsDir is the §11.4.115 GREEN-polarity
// guard for Wave-20 SECOND-PASS defect CACHE2-1 (a durability asymmetry the CA-2
// fix missed): Verify's SHA-drift handler quarantines the corrupt blob via the
// renameFile seam and — on the PRIMARY branch — follows the rename with a
// syncDir(blobsDir) so the quarantine survives a crash (CA-2). But when the
// quarantine rename FAILS (cross-device link / permission / racing target),
// Verify falls back to os.Remove(path) to unlink the corrupt blob — and pre-fix
// that removal was NOT followed by any syncDir. os.Remove gives the unlink, not
// its durability: a crash between the unlink and the (missing) directory fsync
// can resurrect the corrupt blob back at path on recovery, which Get's
// re-hash-free fast path then serves forever as "verified" — the exact
// §11.4.14/§11.4.108 integrity hole CA-2 closed on the rename branch but left
// open on this sibling remove-fallback branch.
//
// This forces the quarantine rename to fail via the renameFile seam, then
// asserts Verify (a) still removes the corrupt blob AND (b) fsyncs the blobs
// directory afterwards so the removal is durable. Pre-fix the syncDir invocation
// count on this path is 0 → RED; post-fix it is ≥1 → GREEN.
//
// HONEST BOUNDARY (§11.4.107/§11.4.123): a unit test cannot power-cut the kernel,
// so this proves the durability BARRIER is INVOKED on the remove-fallback path —
// NOT that the removal survives a real crash. Same honest boundary as C-1/CA-2.
// Overrides package-level seams, so NO t.Parallel().
func TestWave20_CACHE2_VerifyRemoveFallbackFsyncsDir(t *testing.T) {
	body := []byte("good, previously-verified bytes for CACHE2-1 remove-fallback durability")
	var hits int64
	srv := newCountingHTTPServer(body, &hits)
	defer srv.Close()

	s := NewFilesystemStore(t.TempDir())
	m, _ := newSingleImageManifest(t, "cache2-1", srv.URL, body)
	ctx := context.Background()

	// 1) Prime the cache with a good, SHA-verified blob using the REAL seams.
	path, err := s.Get(ctx, m, "cache2-1")
	require.NoError(t, err)
	require.FileExists(t, path)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))

	// 2) Corrupt the on-disk blob (bit-rot / partial write).
	require.NoError(t, os.WriteFile(path, []byte("CORRUPT poisoned bytes for CACHE2-1"), 0o644))

	// 3) Install seams: force the quarantine rename to FAIL (simulating a
	//    cross-device/permission failure so Verify takes the os.Remove fallback),
	//    and record every syncDir invocation from here on. Real seams were used
	//    for the priming Get above, so no publish-time syncDir is miscounted.
	var syncDirCalls int32
	prevRename, prevSyncDir := renameFile, syncDir
	renameFile = func(oldpath, newpath string) error {
		return fmt.Errorf("simulated quarantine rename failure (cross-device)")
	}
	syncDir = func(dir string) error {
		atomic.AddInt32(&syncDirCalls, 1)
		return fsyncDir(dir)
	}
	restore := func() { renameFile, syncDir = prevRename, prevSyncDir }
	defer restore()

	// 4) Verify: detects drift, the quarantine rename fails, so it falls back to
	//    os.Remove of the corrupt blob.
	verr := s.Verify(ctx, m, "cache2-1")
	require.Error(t, verr, "Verify must surface the SHA drift")
	assert.Contains(t, verr.Error(), "drift")

	// (a) The corrupt blob must be gone — the os.Remove fallback ran.
	_, statErr := os.Stat(path)
	require.Truef(t, os.IsNotExist(statErr),
		"BUG CACHE2-1: the corrupt blob at %s was not removed on the rename-failure fallback path", path)

	// (b) THE ASSERTION THAT CATCHES THE BUG: the fallback os.Remove MUST be
	//     followed by a blobs-dir fsync so the removal is durable. Pre-fix this
	//     path performs NO syncDir → 0 calls → RED.
	require.GreaterOrEqualf(t, atomic.LoadInt32(&syncDirCalls), int32(1),
		"BUG CACHE2-1: Verify's os.Remove drift-fallback was NOT followed by a blobsDir fsync — "+
			"the corrupt-blob removal is not durable; a crash can resurrect it for Get's re-hash-free fast path")

	// 5) Restore the real seams and prove the removal actually cleared the fast
	//    path: a subsequent Get misses and re-fetches a correct copy (2nd fetch).
	restore()
	path2, err := s.Get(ctx, m, "cache2-1")
	require.NoError(t, err)
	assert.Equal(t, path, path2)
	assert.Equal(t, int64(2), atomic.LoadInt64(&hits),
		"Get must re-fetch after the corrupt blob was removed on the fallback path")
	got, _ := os.ReadFile(path2)
	assert.Equal(t, body, got, "the re-fetched blob must be the good bytes, not the corrupt ones")
}
