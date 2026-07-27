package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCTHardenCache1_Refresh_DestroysValidBlobOnFailedRefetch is the §11.4.115
// guard for CT-HARDEN-CACHE-1: pre-fix, Refresh() unconditionally os.Remove'd
// the current, already-SHA-verified blob BEFORE attempting the re-fetch, so any
// transient re-fetch failure (network error, non-200, SHA/size mismatch, ctx
// cancellation) left the cache entry permanently destroyed instead of unchanged.
// Fixed: Refresh shares Get's fetch→verify→atomic-rename path (fetchAndPlace/
// fetchToTemp) and never removes the existing blob until a verified replacement
// is ready to atomically swap in. Fully sequential — no goroutine/scheduler luck.
func TestCTHardenCache1_Refresh_DestroysValidBlobOnFailedRefetch(t *testing.T) {
	body := []byte("real, valid, previously-verified qcow2 bytes")
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
			return
		}
		// 2nd request (the Refresh-triggered re-fetch): simulate a
		// transient upstream failure.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cacheRoot := t.TempDir()
	m, hash := newSingleImageManifest(t, "x", srv.URL, body)
	s := NewFilesystemStore(cacheRoot)
	ctx := context.Background()

	path, err := s.Get(ctx, m, "x")
	require.NoError(t, err)
	require.FileExists(t, path)

	refreshErr := s.Refresh(ctx, m, "x")
	require.Error(t, refreshErr, "Refresh must surface the re-fetch failure")

	// THE ASSERTION THAT CATCHES THE BUG: a failed Refresh must leave the
	// previously-verified blob intact, not destroy it.
	_, statErr := os.Stat(path)
	require.NoErrorf(t, statErr,
		"BUG CT-HARDEN-CACHE-1: the valid blob at %s was destroyed by a failed Refresh", path)

	got2, readErr2 := os.ReadFile(path)
	require.NoError(t, readErr2)
	assert.Equal(t, body, got2)

	// The blob still serves from cache (no 3rd HTTP fetch needed).
	path2, err := s.Get(ctx, m, "x")
	require.NoError(t, err)
	assert.Equal(t, path, path2)
	assert.Equal(t, int64(2), atomic.LoadInt64(&hits), "no 3rd HTTP fetch")
	assert.Equal(t, hash, filepath.Base(path))
}

// TestCTHardenCache2_NilManifest_CleanErrorNotPanic is the §11.4.115 guard for
// CT-HARDEN-CACHE-2: Manifest.FindByID (the single choke point Get/Verify/
// Refresh all funnel through) dereferenced a possibly-nil receiver with no
// guard, so a nil *Manifest passed to any of the three exported Store methods
// panicked instead of returning a clean, handleable error. Fixed with one
// nil-receiver guard at the funnel point.
func TestCTHardenCache2_NilManifest_CleanErrorNotPanic(t *testing.T) {
	s := NewFilesystemStore(t.TempDir())
	ctx := context.Background()

	assertCleanError := func(name string, call func() error) {
		t.Helper()
		var panicked any
		err := func() (err error) {
			defer func() { panicked = recover() }()
			return call()
		}()
		if panicked != nil {
			t.Fatalf("%s: PANICKED on nil manifest instead of returning a clean error: %v", name, panicked)
		}
		if err == nil {
			t.Fatalf("%s: expected an error for a nil manifest, got nil", name)
		}
		if !strings.Contains(err.Error(), "manifest") {
			t.Fatalf("%s: error should mention the manifest problem, got: %v", name, err)
		}
	}

	assertCleanError("Get", func() error {
		_, err := s.Get(ctx, nil, "x")
		return err
	})
	assertCleanError("Verify", func() error {
		return s.Verify(ctx, nil, "x")
	})
	assertCleanError("Refresh", func() error {
		return s.Refresh(ctx, nil, "x")
	})
}
