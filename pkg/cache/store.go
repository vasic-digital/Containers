package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Store is the cache contract.
//
// Get returns the local path of the image's bytes. On cache miss, the
// image is fetched from the manifest's URL, SHA-256 verified, then
// written atomically. Verify recomputes the SHA and compares against
// the manifest. Refresh forces a fresh fetch and atomically replaces the
// cached blob — it never removes the existing blob until a verified
// replacement is ready, so a failed re-fetch leaves the cache untouched.
type Store interface {
	Get(ctx context.Context, m *Manifest, imageID string) (path string, err error)
	Verify(ctx context.Context, m *Manifest, imageID string) error
	Refresh(ctx context.Context, m *Manifest, imageID string) error
}

// FilesystemStore is the default Store impl. Cache root layout:
//
//	<root>/blobs/sha256/<full-hash>     image bytes
//	<root>/lockfiles/<sha-prefix>.lock  flock per-image (concurrent-fetch safety)
type FilesystemStore struct {
	root string
	// httpClient injection seam for tests; production uses http.DefaultClient.
	httpClient *http.Client
	// in-process mutex map: serializes Get for the same imageID across
	// goroutines in the same process. Cross-process safety comes from
	// the flock on disk; in-process callers don't need to acquire flock
	// twice.
	mu     sync.Mutex
	keymus map[string]*sync.Mutex
}

// NewFilesystemStore constructs a Store rooted at cacheRoot.
// $XDG_CACHE_HOME/vasic-digital/containers-images/ is the production path;
// tests pass t.TempDir() to isolate.
func NewFilesystemStore(cacheRoot string) *FilesystemStore {
	s := &FilesystemStore{
		root:       cacheRoot,
		httpClient: http.DefaultClient,
		keymus:     make(map[string]*sync.Mutex),
	}
	s.sweepStaleIncoming()
	return s
}

// sweepStaleIncoming removes leftover "incoming-*" partial-download temp
// files orphaned by a previous process that crashed (SIGKILL/OOM) between
// os.CreateTemp and fetchToTemp's terminal rename-or-remove (CA-3, §11.4.14
// durability). fetchToTemp ALWAYS either renames an "incoming-*" file into
// place (fetchAndPlace) or os.Removes it on any failure path, so anything
// still matching "incoming-*" in blobsDir at construction time is, by
// construction, prior-run debris — never a live in-flight download — for
// multi-GB qcow2/Android images an unswept orphan is an unbounded disk leak
// across restarts.
//
// This runs exactly once, synchronously, INSIDE NewFilesystemStore, strictly
// BEFORE the constructed *FilesystemStore is ever handed to a caller — so no
// concurrent fetchToTemp using THIS store instance can possibly be creating a
// NEW incoming-* file yet. Best-effort: blobsDir may not exist on a fresh
// cache root, and a permission error here must not fail construction —
// fetchAndPlace's own os.MkdirAll(s.blobsDir()) still runs on first fetch
// either way.
func (s *FilesystemStore) sweepStaleIncoming() {
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "incoming-") {
			_ = os.Remove(filepath.Join(s.blobsDir(), e.Name()))
		}
	}
}

func (s *FilesystemStore) blobsDir() string     { return filepath.Join(s.root, "blobs", "sha256") }
func (s *FilesystemStore) lockfilesDir() string { return filepath.Join(s.root, "lockfiles") }

func (s *FilesystemStore) blobPath(sha string) string {
	return filepath.Join(s.blobsDir(), sha)
}

func (s *FilesystemStore) lockPath(sha string) string {
	prefix := sha
	if len(sha) > 8 {
		prefix = sha[:8]
	}
	return filepath.Join(s.lockfilesDir(), prefix+".lock")
}

// quarantinePath returns a fresh, not-yet-used quarantine sidecar path for
// path (CA-4, Wave-20): rename(2) silently OVERWRITES an existing file at its
// target, so a fixed "path+\".corrupt\"" name would let a second
// corruption/Verify cycle destroy the first forensic sample. This versions
// the sidecar name with an incrementing suffix (".corrupt", ".corrupt.1",
// ".corrupt.2", ...) so every quarantine event's sample survives
// independently. Called with s's per-SHA flock already held (from Verify),
// so no two callers can race to pick the same candidate name for this path.
func (s *FilesystemStore) quarantinePath(path string) string {
	base := path + ".corrupt"
	if _, err := os.Stat(base); err != nil {
		return base
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
}

// Durability seams (§11.4.115 structural guard for Wave-20 CACHE-HARD C-1).
// The fsync/rename operations that make a fetched blob durable are indirected
// through package-level vars so the crash-durability guard can record their
// invocation ORDER: syncFile (flush the temp's data pages) BEFORE renameFile
// (atomically publish the blob at its final path) BEFORE syncDir (fsync the
// containing directory so the rename itself survives a crash). Production
// points them at the real syscalls; tests that override them MUST NOT use
// t.Parallel() — the swap-and-restore pattern on a package-level var is not
// concurrency-safe (same convention as pkg/vm's killByPortHook).
var (
	syncFile   = (*os.File).Sync
	renameFile = os.Rename
	syncDir    = fsyncDir
)

// verifyPostHashHook is a test-only synchronization seam (default no-op),
// invoked by Verify immediately after it has computed the on-disk hash of
// the blob and decided drift-vs-match, but BEFORE it acts on that decision
// (quarantine rename or plain return). Production never overrides this.
// Wave-20 CA-1's regression guard uses it to hold Verify inside its locked
// critical section for a controlled window, proving a concurrent Refresh for
// the SAME content address genuinely blocks on the shared per-SHA flock
// instead of racing Verify's read-decide-quarantine sequence to completion.
var verifyPostHashHook = func() {}

// fsyncDir opens dir and fsyncs it so a preceding os.Rename INTO that directory
// becomes durable (the rename's directory-entry metadata is flushed to stable
// storage). Returns the first error encountered.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if serr := d.Sync(); serr != nil {
		_ = d.Close()
		return serr
	}
	return d.Close()
}

// normalizeSHA256 is the security boundary for the content-addressed blob
// path. It lowercases the manifest SHA and requires exactly 64 hexadecimal
// characters, returning the canonical lowercase form or an error.
//
// Two properties depend on this:
//   - Path safety: a 64-char all-hex string cannot contain a path separator
//     or "..", so it can never traverse out of blobsDir when filepath.Join'd.
//     A value that fails this check (e.g. "../../poison.img") MUST never reach
//     blobPath/lockPath — otherwise a Manifest that did not pass through
//     LoadManifest could make Get return an arbitrary file as a "verified"
//     image (integrity-verify bypass) or make Refresh os.Remove an arbitrary
//     file (arbitrary deletion).
//   - Case normalization: LoadManifest permits uppercase hex, but the
//     recomputed digest is always lowercase (hex.EncodeToString), so without
//     normalizing an uppercase-hex manifest SHA would never match the blob
//     path nor the compare, and the image could never cache.
func normalizeSHA256(sha string) (string, error) {
	s := strings.ToLower(sha)
	if len(s) != 64 {
		return "", fmt.Errorf("invalid SHA256 %q: must be 64 hex characters (got %d)", sha, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("invalid SHA256 %q: not hexadecimal: %w", sha, err)
	}
	return s, nil
}

func (s *FilesystemStore) keymu(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	mu, ok := s.keymus[id]
	if !ok {
		mu = &sync.Mutex{}
		s.keymus[id] = mu
	}
	return mu
}

// acquireSHALock opens (creating if needed) the per-SHA cross-process lock
// file at s.lockPath(sha) and takes an exclusive flock on it, returning a
// release func the caller MUST invoke exactly once (typically via defer)
// when done. This is the SAME lock fetchAndPlace (Get/Refresh) uses to
// serialize a fetch/publish for a given content address against any other
// process, or any other in-process imageID that happens to resolve to the
// SAME sha256 — content-addressing means two different manifest entries can
// share one blob path. Verify (CA-1, Wave-20) takes this same lock too, so
// its read-decide-quarantine sequence can never straddle a concurrent
// fetchAndPlace publishing a fresh blob for the identical content address.
func (s *FilesystemStore) acquireSHALock(sha string) (release func(), err error) {
	if err := os.MkdirAll(s.lockfilesDir(), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lockfiles: %w", err)
	}
	lockPath := s.lockPath(sha)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// Get returns the local path of the image's bytes. On cache miss, the
// image is fetched from URL, SHA-256 verified, then written atomically
// to the blob path. SHA mismatch removes the partial file and returns
// an error.
//
// Concurrency: in-process callers serialize via a per-id sync.Mutex;
// cross-process callers serialize via a syscall.Flock on a sidecar
// lock file. Both paths re-check the cache fast-path after acquiring
// the lock so the second waiter discovers the first waiter's blob and
// skips the fetch.
func (s *FilesystemStore) Get(ctx context.Context, m *Manifest, imageID string) (string, error) {
	entry, err := m.FindByID(imageID)
	if err != nil {
		return "", err
	}
	sha, err := normalizeSHA256(entry.SHA256)
	if err != nil {
		return "", fmt.Errorf("image %q: %w", entry.ID, err)
	}
	final := s.blobPath(sha)

	// Fast path — already cached.
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}

	// In-process serialization first.
	mu := s.keymu(imageID)
	mu.Lock()
	defer mu.Unlock()

	// After winning the in-process lock, re-check the fast path.
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}

	return s.fetchAndPlace(ctx, entry, sha, final, true)
}

// fetchAndPlace performs the cross-process-locked fetch → verify → atomic
// place sequence shared by Get (fetch-if-missing) and Refresh (forced
// re-fetch). The caller MUST already hold s.keymu for the relevant
// imageID before calling this.
//
// It fetches entry.URL into a brand-new temp file and verifies the
// SHA-256 (and declared size) BEFORE final is ever touched — see
// fetchToTemp. The only write to final is the terminal os.Rename, which
// atomically replaces whatever is currently there (if anything) in a
// single filesystem operation. This is what makes Refresh safe to call
// against an imageID that already has a valid cached blob: a failed or
// corrupted re-fetch never destroys the existing good blob, because
// nothing at final is ever removed or truncated until the replacement
// has already been fetched and verified.
//
// dedupeIfPresent, when true (the Get path), re-checks final for
// existence immediately after the flock is acquired and returns it
// without fetching if present — this collapses a redundant fetch when a
// DIFFERENT imageID resolving to the SAME SHA (or a different process)
// populated the blob while the caller was blocked on the flock. Refresh
// passes false: its entire purpose is to force a fresh fetch even though
// final already exists, so it must not short-circuit on the very file it
// is being asked to replace.
func (s *FilesystemStore) fetchAndPlace(ctx context.Context, entry *ImageEntry, sha, final string, dedupeIfPresent bool) (string, error) {
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		return "", fmt.Errorf("mkdir blobs: %w", err)
	}
	release, err := s.acquireSHALock(sha)
	if err != nil {
		return "", err
	}
	defer release()

	// Re-check fast path AFTER acquiring flock — another process (or,
	// in-process, a different imageID sharing this same SHA and thus a
	// different keymu) may have populated it while we waited.
	if dedupeIfPresent {
		if _, err := os.Stat(final); err == nil {
			return final, nil
		}
	}

	tmpPath, err := s.fetchToTemp(ctx, entry, sha)
	if err != nil {
		return "", err
	}
	if err := renameFile(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("atomic rename %s → %s: %w", tmpPath, final, err)
	}
	// Durability barrier: os.Rename atomically publishes the blob, but the
	// rename itself is only durable once the CONTAINING directory's metadata
	// is fsync'd. Without this, a crash after the rename returns but before
	// the directory entry flushes can lose the publish on recovery (final
	// missing, or a stale entry surviving). fsync the blobs dir so the atomic
	// publish survives a power-loss. The blob at final is already SHA-verified
	// (fetchToTemp verifies before returning tmpPath), so a dir-fsync failure
	// surfaces as an error while the verified bytes remain safely in place for
	// the next fast-path Get.
	if err := syncDir(s.blobsDir()); err != nil {
		return "", fmt.Errorf("fsync blobs dir: %w", err)
	}
	return final, nil
}

// fetchToTemp downloads entry.URL into a brand-new temp file under
// blobsDir and verifies its SHA-256 (and declared size, if non-zero)
// against sha BEFORE returning. On ANY failure — request construction,
// network error, non-200, write error, SHA mismatch, size mismatch — the
// temp file is removed and an error is returned. fetchToTemp never
// touches (removes, truncates, or overwrites) any existing blob at the
// eventual final path: the caller only learns the returned tmpPath once
// its content is already known-good, so a rejected/failed fetch leaves
// whatever was previously cached completely untouched.
func (s *FilesystemStore) fetchToTemp(ctx context.Context, entry *ImageEntry, sha string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", entry.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d", entry.URL, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(s.blobsDir(), "incoming-*")
	if err != nil {
		return "", fmt.Errorf("create tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write tempfile: %w", err)
	}
	// Durability barrier: flush the blob DATA to stable storage BEFORE the
	// temp is eligible to be renamed into place. os.Rename gives ATOMICITY
	// (no torn file ever visible at final) but NOT DURABILITY — on a crash
	// after the rename's metadata is journaled but before the data pages
	// flush, recovery can leave a present-but-zero/garbage blob at final,
	// which Get's re-hash-free fast path would then serve forever as
	// "verified". A failed Sync fails the fetch and removes the temp; it
	// never publishes an unflushed blob.
	if serr := syncFile(tmp); serr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("fsync tempfile %q: %w", entry.ID, serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close tempfile %q: %w", entry.ID, cerr)
	}

	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if gotSHA != sha {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("image %q: SHA256 mismatch (got %s, want %s)",
			entry.ID, gotSHA, sha)
	}
	if entry.Size != 0 && written != entry.Size {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("image %q: size mismatch (got %d, want %d)",
			entry.ID, written, entry.Size)
	}
	return tmpPath, nil
}

// Verify recomputes the SHA-256 of the cached blob and compares to the
// manifest. Returns nil iff the blob exists AND its bytes hash to the
// declared SHA. Used by tooling that audits cache integrity.
//
// Concurrency (CA-1, Wave-20): Verify takes the SAME per-imageID in-process
// mutex AND the SAME cross-process per-SHA flock that fetchAndPlace
// (Get/Refresh) uses, acquired BEFORE the open→hash→(if drift)quarantine
// sequence below and held across all of it. Without this, a concurrent
// Get/Refresh could publish a fresh, verified blob for the identical content
// address BETWEEN Verify's hash and Verify's quarantine rename — Verify's
// quarantine acts on "whatever is currently at path", so the fresh good blob
// could be quarantined based on Verify's stale (pre-replacement) read, or a
// Get caller mid-use of the fast-path hit could have its file yanked out.
// Holding both locks for the full sequence makes it atomic against any other
// caller touching this exact content address.
func (s *FilesystemStore) Verify(ctx context.Context, m *Manifest, imageID string) error {
	entry, err := m.FindByID(imageID)
	if err != nil {
		return err
	}
	sha, err := normalizeSHA256(entry.SHA256)
	if err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}

	mu := s.keymu(imageID)
	mu.Lock()
	defer mu.Unlock()

	release, err := s.acquireSHALock(sha)
	if err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}
	defer release()

	path := s.blobPath(sha)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, f)
	// Close before any quarantine rename below so we never move a file we
	// still hold open (defensive across platforms; harmless on Linux).
	_ = f.Close()
	if copyErr != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, copyErr)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	verifyPostHashHook()
	if got != sha {
		// SHA drift: the cached blob's bytes no longer hash to the declared
		// SHA (on-disk corruption / bit-rot / partial write). Get's fast path
		// never re-hashes a cached blob, so leaving the poisoned blob in place
		// would serve it forever as "verified" (breaking the package's
		// integrity property). Quarantine it aside to a versioned .corrupt
		// sidecar (CA-4: a fixed name would let rename(2) silently clobber an
		// earlier forensic sample on a second corruption/Verify cycle) so a
		// subsequent Get misses the fast path and re-fetches a good copy,
		// while the corrupt bytes are preserved for forensic recovery. The
		// rename goes through the same renameFile seam fetchAndPlace's
		// publish uses, followed by a syncDir(blobsDir) fsync (CA-2:
		// os.Rename gives atomicity, not durability — without the following
		// dir-fsync a crash between the quarantine rename and the flush can
		// leave the corrupt blob back at path on recovery, silently undoing
		// the quarantine). If the move-aside fails, fall back to removing the
		// blob so a re-fetch can still recover. The drift error is returned
		// either way.
		quarantine := s.quarantinePath(path)
		if renameErr := renameFile(path, quarantine); renameErr != nil {
			// Quarantine rename failed (cross-device link, permission, target
			// races): fall back to REMOVING the corrupt blob so a subsequent Get
			// misses the fast path and re-fetches a good copy. This removal MUST
			// be made durable exactly like the quarantine-rename branch below
			// (CACHE2-1, symmetric with CA-2): os.Remove unlinks the corrupt blob
			// from the directory, but WITHOUT an fsync of the containing directory
			// the unlink's metadata can be lost on a crash — recovery then
			// resurrects the corrupt blob back at path, which Get's re-hash-free
			// fast path serves forever as "verified", silently undoing the
			// removal (the §11.4.14/§11.4.108 durability hole CA-2 closed only on
			// the rename branch, left open on this sibling remove branch). fsync
			// the blobs dir so the removal survives a power-loss.
			_ = os.Remove(path)
			postRemoveSync := syncDir(s.blobsDir()) // CACHE2-1: durably flush the corrupt-blob removal
			if postRemoveSync != nil {
				return fmt.Errorf("verify %q: SHA256 drift (got %s, want %s); post-remove dir-fsync failed: %w",
					entry.ID, got, sha, postRemoveSync)
			}
		} else if syncErr := syncDir(s.blobsDir()); syncErr != nil {
			return fmt.Errorf("verify %q: SHA256 drift (got %s, want %s); quarantine dir-fsync failed: %w",
				entry.ID, got, sha, syncErr)
		}
		return fmt.Errorf("verify %q: SHA256 drift (got %s, want %s)",
			entry.ID, got, sha)
	}
	return nil
}

// Refresh forces a fresh fetch of the image and atomically replaces the
// cached blob. Used by tooling when the operator deliberately bumps a
// manifest entry's SHA, or wants to force a re-fetch of the same content
// (e.g. suspected local corruption) — Refresh's contract does not require
// entry.SHA256 to differ from whatever is currently cached.
//
// Refresh NEVER removes the existing blob before the replacement has been
// fetched AND verified: it shares fetchAndPlace/fetchToTemp with Get, so a
// failed or corrupted re-fetch (network error, non-200, SHA/size
// mismatch, context cancellation) returns an error while leaving whatever
// was previously cached at the blob path completely untouched.
//
// Locking mirrors Get: the same per-imageID in-process mutex, and the
// same per-SHA flock (acquired inside fetchAndPlace), so a Refresh call
// serializes correctly against concurrent Get calls for the same imageID
// instead of racing an unlocked delete against an in-flight fetch.
func (s *FilesystemStore) Refresh(ctx context.Context, m *Manifest, imageID string) error {
	entry, err := m.FindByID(imageID)
	if err != nil {
		return err
	}
	sha, err := normalizeSHA256(entry.SHA256)
	if err != nil {
		return fmt.Errorf("refresh %q: %w", entry.ID, err)
	}
	final := s.blobPath(sha)

	mu := s.keymu(imageID)
	mu.Lock()
	defer mu.Unlock()

	if _, err := s.fetchAndPlace(ctx, entry, sha, final, false); err != nil {
		return fmt.Errorf("refresh %q: %w", entry.ID, err)
	}
	return nil
}
