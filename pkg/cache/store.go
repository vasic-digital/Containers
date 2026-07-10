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
	return &FilesystemStore{
		root:       cacheRoot,
		httpClient: http.DefaultClient,
		keymus:     make(map[string]*sync.Mutex),
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
	if err := os.MkdirAll(s.lockfilesDir(), 0o755); err != nil {
		return "", fmt.Errorf("mkdir lockfiles: %w", err)
	}
	lockPath := s.lockPath(sha)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("flock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

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
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("atomic rename %s → %s: %w", tmpPath, final, err)
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
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write tempfile: %w", err)
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
func (s *FilesystemStore) Verify(ctx context.Context, m *Manifest, imageID string) error {
	entry, err := m.FindByID(imageID)
	if err != nil {
		return err
	}
	sha, err := normalizeSHA256(entry.SHA256)
	if err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}
	path := s.blobPath(sha)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("verify %q: %w", entry.ID, err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != sha {
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
