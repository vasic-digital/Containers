package cache

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWave20CacheHardC1_FsyncOrdering_FileBeforeRenameBeforeDir is the
// §11.4.115 structural guard for Wave-20 CACHE-HARD C-1 (crash-durability gap):
// pre-fix, fetchToTemp wrote the download and Close'd the temp WITHOUT ever
// fsync'ing its data, and fetchAndPlace's os.Rename published the blob WITHOUT
// fsync'ing the containing directory. os.Rename gives ATOMICITY but not
// DURABILITY: on a crash after the rename metadata is journaled but before the
// data pages / directory entry flush, recovery can leave a present-but-garbage
// blob at final that Get's re-hash-free fast path serves forever as "verified".
//
// The fix flushes the temp DATA (tmp.Sync) BEFORE the temp is eligible to be
// renamed, and fsyncs the blobs directory AFTER the rename so the publish is
// durable. This guard records the invocation order of the three durability
// operations during a real Get and asserts file-sync → rename → dir-sync.
//
// HONEST BOUNDARY (§11.4.107 / §11.4.123): a unit test CANNOT power-cut the
// kernel mid-write, so this guard proves the durability BARRIER is INVOKED in
// the correct order — NOT that the blob's bytes survive a real crash. In-process
// crash-survival has no clean software oracle; that gap is stated, never claimed
// closed. Overrides package-level seams, so NO t.Parallel().
func TestWave20CacheHardC1_FsyncOrdering_FileBeforeRenameBeforeDir(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(name string) {
		mu.Lock()
		events = append(events, name)
		mu.Unlock()
	}
	prevSyncFile, prevRename, prevSyncDir := syncFile, renameFile, syncDir
	syncFile = func(f *os.File) error { record("file_sync"); return f.Sync() }
	renameFile = func(oldpath, newpath string) error { record("rename"); return os.Rename(oldpath, newpath) }
	syncDir = func(dir string) error { record("dir_sync"); return fsyncDir(dir) }
	defer func() { syncFile, renameFile, syncDir = prevSyncFile, prevRename, prevSyncDir }()

	body := []byte("durable qcow2 bytes")
	var hits int64
	srv := newCountingHTTPServer(body, &hits)
	defer srv.Close()

	s := NewFilesystemStore(t.TempDir())
	m, _ := newSingleImageManifest(t, "d", srv.URL, body)

	path, err := s.Get(context.Background(), m, "d")
	require.NoError(t, err)
	require.FileExists(t, path)

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
	fi, ri, di := idx("file_sync"), idx("rename"), idx("dir_sync")
	require.GreaterOrEqualf(t, fi, 0,
		"BUG CACHE-HARD C-1: tmp.Sync() (data flush) was never invoked before publishing the blob — the durability barrier is missing; recorded ops=%v", events)
	require.GreaterOrEqualf(t, di, 0,
		"BUG CACHE-HARD C-1: the blobs directory was never fsync'd after the rename — the atomic publish is not durable; recorded ops=%v", events)
	require.GreaterOrEqualf(t, ri, 0, "sanity: the rename seam must fire; recorded ops=%v", events)
	assert.Lessf(t, fi, ri, "file DATA must be fsync'd BEFORE the rename publishes the blob (C-1 ordering); recorded ops=%v", events)
	assert.Lessf(t, ri, di, "the blobs directory must be fsync'd AFTER the rename (C-1 ordering); recorded ops=%v", events)
}

// TestWave20CacheHardC2_VerifyDriftQuarantinesSoNextGetRefetches is the
// §11.4.115 guard for Wave-20 CACHE-HARD C-2 (corrupt blob served after Verify
// flags it): pre-fix, Verify recomputed the SHA and returned a drift error on
// mismatch but LEFT the poisoned blob in place; Get's fast path never re-hashes,
// so the very next Get kept serving the corrupt blob forever. The fix has Verify
// quarantine the drifted blob aside to a .corrupt sidecar so a subsequent Get
// misses the fast path and re-fetches a good copy. Fully sequential — no
// goroutine/scheduler luck.
func TestWave20CacheHardC2_VerifyDriftQuarantinesSoNextGetRefetches(t *testing.T) {
	body := []byte("good, previously-verified qcow2 bytes")
	var hits int64
	srv := newCountingHTTPServer(body, &hits)
	defer srv.Close()

	s := NewFilesystemStore(t.TempDir())
	m, _ := newSingleImageManifest(t, "c2", srv.URL, body)
	ctx := context.Background()

	// 1) Prime the cache with a good, SHA-verified blob (1 HTTP fetch).
	path, err := s.Get(ctx, m, "c2")
	require.NoError(t, err)
	require.FileExists(t, path)
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))

	// 2) Corrupt the on-disk blob bytes directly (bit-rot / partial write).
	require.NoError(t, os.WriteFile(path, []byte("CORRUPT poisoned bytes"), 0o644))

	// 3) Verify detects the drift AND removes/quarantines the poisoned blob.
	verr := s.Verify(ctx, m, "c2")
	require.Error(t, verr, "Verify must surface the SHA drift")
	assert.Contains(t, verr.Error(), "drift")

	_, statErr := os.Stat(path)
	require.Truef(t, os.IsNotExist(statErr),
		"BUG CACHE-HARD C-2: the corrupt blob at %s was NOT removed/quarantined; Get's fast path will serve it forever as 'verified'", path)
	// The corrupt bytes are preserved aside for forensic recovery.
	assert.FileExists(t, path+".corrupt")

	// 4) A subsequent Get misses the now-empty fast path and re-fetches a
	//    correct copy (2nd HTTP fetch) — the whole point of the quarantine.
	path2, err := s.Get(ctx, m, "c2")
	require.NoError(t, err)
	assert.Equal(t, path, path2)
	assert.Equal(t, int64(2), atomic.LoadInt64(&hits),
		"Get must re-fetch after the poisoned blob was quarantined")
	got, _ := os.ReadFile(path2)
	assert.Equal(t, body, got, "the re-fetched blob must be the good bytes, not the corrupt ones")
}
