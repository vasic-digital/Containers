package cache

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWave17_Cache_SHA256_ValidatedAndNormalized proves the Store validates
// and normalizes entry.SHA256 BEFORE splicing it into the content-addressed
// blob/lock filesystem path.
//
// Two coupled defects on the pre-fix code:
//  1. Path-traversal + integrity-verify bypass — a Manifest that did not pass
//     through LoadManifest (built in code / json.Unmarshalled by a consumer)
//     can carry a SHA256 like "../../poison.img"; blobPath filepath.Joins it
//     out of the blob dir, and Get's os.Stat fast-path returns that arbitrary
//     file as a "verified" image with ZERO SHA check, while Refresh os.Removes
//     it (arbitrary file deletion).
//  2. Uppercase-hex SHA never caches — LoadManifest permits uppercase hex, but
//     the recomputed digest is always lowercase (hex.EncodeToString), so the
//     compare and the blob path never match for an uppercase-SHA manifest.
//
// A single normalize-to-lowercase + validate-64-hex boundary closes both.
// RED on the pre-fix source; the polarity harness reverts each site.
func TestWave17_Cache_SHA256_ValidatedAndNormalized(t *testing.T) {
	const traversal = "../../poison.img"
	ctx := context.Background()

	// Places a decoy file at <root>/poison.img, the location the traversal
	// SHA resolves to: blobPath = <root>/blobs/sha256, Join(.., "../../poison.img")
	// == <root>/poison.img.
	newPoisoned := func(t *testing.T) (*FilesystemStore, string) {
		t.Helper()
		root := t.TempDir()
		s := NewFilesystemStore(root)
		poison := root + "/poison.img"
		require.NoError(t, os.WriteFile(poison, []byte("not an image"), 0o644))
		require.Equal(t, poison, s.blobPath(traversal),
			"sanity: the traversal SHA must resolve to the decoy outside the blob dir")
		return s, poison
	}
	manifestWith := func(sha string) *Manifest {
		return &Manifest{Version: 1, Images: []ImageEntry{
			{ID: "img", URL: "http://never.invalid/x", SHA256: sha, Size: 12, Format: "qcow2"},
		}}
	}

	t.Run("TraversalRejected_Get_NoBypass", func(t *testing.T) {
		s, poison := newPoisoned(t)
		path, err := s.Get(ctx, manifestWith(traversal), "img")
		require.Error(t, err, "Get must reject a non-64-hex SHA, not return an out-of-blobdir file as verified")
		assert.Empty(t, path)
		assert.NotEqual(t, poison, path)
	})

	t.Run("TraversalRejected_Refresh_NoArbitraryDelete", func(t *testing.T) {
		s, poison := newPoisoned(t)
		_ = s.Refresh(ctx, manifestWith(traversal), "img")
		assert.FileExists(t, poison,
			"Refresh must reject the traversal SHA BEFORE os.Remove — pre-fix deletes an arbitrary file")
	})

	t.Run("TraversalRejected_Verify", func(t *testing.T) {
		s, _ := newPoisoned(t)
		err := s.Verify(ctx, manifestWith(traversal), "img")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid SHA256",
			"Verify must reject the malformed SHA at the boundary, not hash the traversal target")
	})

	t.Run("UppercaseSHA_Normalizes_Verify", func(t *testing.T) {
		body := []byte("upper-sha image bytes")
		lower := sha256Hex(body)
		upper := strings.ToUpper(lower)
		require.NotEqual(t, lower, upper)

		root := t.TempDir()
		s := NewFilesystemStore(root)
		require.NoError(t, os.MkdirAll(s.blobsDir(), 0o755))
		// Blob stored at the canonical lowercase content path.
		require.NoError(t, os.WriteFile(s.blobPath(lower), body, 0o644))

		m := &Manifest{Version: 1, Images: []ImageEntry{
			{ID: "up", URL: "http://x", SHA256: upper, Size: int64(len(body)), Format: "qcow2"},
		}}
		require.NoError(t, s.Verify(ctx, m, "up"),
			"an uppercase-hex manifest SHA must verify against the lowercase blob (normalize case)")
	})

	// Positive control: a valid lowercase SHA still fetches + caches correctly
	// (the fix must not over-restrict legitimate input).
	t.Run("ValidLowercase_StillCaches", func(t *testing.T) {
		body := []byte("fake qcow2 bytes for control")
		var hits int64
		srv := newCountingHTTPServer(body, &hits)
		defer srv.Close()

		s := NewFilesystemStore(t.TempDir())
		m, hash := newSingleImageManifest(t, "ctl", srv.URL, body)
		path, err := s.Get(ctx, m, "ctl")
		require.NoError(t, err)
		assert.Equal(t, s.blobPath(hash), path)
		got, _ := os.ReadFile(path)
		assert.Equal(t, body, got)
	})
}
