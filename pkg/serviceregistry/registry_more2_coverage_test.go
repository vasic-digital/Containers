package serviceregistry

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiscover_ContextCancelled verifies that Discover returns ctx.Err()
// when the context is already cancelled before the port-scan loop runs.
// This exercises the `select { case <-ctx.Done(): return nil, ctx.Err() }`
// branch inside the loop.
func TestDiscover_ContextCancelled(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Discover

	// Use a port range that spans several ports so the loop would run
	// if the context were not cancelled.
	svc, err := r.Discover(ctx, "test-svc-cancelled", 19200, 19200, 19210)
	assert.Error(t, err)
	assert.Nil(t, svc)
	// The error should be the context cancellation error.
	assert.ErrorIs(t, err, context.Canceled)
}

// TestLoadFromDisk_ValidFile covers the success path of loadFromDisk by
// pre-writing a valid services JSON file and then constructing a registry
// that reads it. This exercises the `r.logger.Info(...)` call at the end
// of loadFromDisk that is otherwise missed.
func TestLoadFromDisk_ValidFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-load-valid-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Write a valid services JSON that loadFromDisk can parse.
	servicesJSON := `{
		"my-service": {
			"name": "my-service",
			"host": "localhost",
			"port": 8080,
			"protocol": "tcp",
			"healthy": true,
			"labels": {}
		}
	}`
	servicesFile := tmpDir + "/services.json"
	require.NoError(t, os.WriteFile(servicesFile, []byte(servicesJSON), 0644))

	// New() calls loadFromDisk internally; providing the dir with a
	// valid JSON triggers the logger.Info branch at the end.
	r := New(WithRegistryDir(tmpDir))
	require.NotNil(t, r)

	// Verify the service was actually loaded.
	svc, ok := r.Get("my-service")
	assert.True(t, ok, "my-service should have been loaded from disk")
	assert.Equal(t, 8080, svc.Port)
}

// TestSaveToDisk_WriteFileError covers persist()'s write-failure branch: a
// directory planted at the services.json path makes the atomic rename fail.
//
// SR-HARD-3 reconciliation (§11.4.120 fix-breaks-its-own-gate): this test used
// to assert "Register should succeed even if saveToDisk WriteFile fails" — i.e.
// it encoded the exact silent-persistence-failure defect SR-HARD-3 removes.
// persist() now RETURNS its failure and Register PROPAGATES it, so the assertion
// is rewritten to the NEW contract (Register surfaces the error). This is a
// reconciliation, not a fake-pass and not a revert: reverting the SR-HARD-3 fix
// (Register returning nil again) makes this test FAIL, so it remains a genuine
// guard rather than a tautology.
func TestSaveToDisk_WriteFileError(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-writefile-fail-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a directory at the location where services.json would be
	// written; this causes the atomic rename in persist() to fail.
	blockerPath := tmpDir + "/services.json"
	require.NoError(t, os.MkdirAll(blockerPath, 0755))

	r := New(WithRegistryDir(tmpDir))
	// Trigger persist(); the write failure MUST be surfaced, not swallowed.
	err = r.Register("write-fail-svc", 7654)
	require.Error(t, err,
		"SR-HARD-3: Register must propagate persist() failure, not silently return nil")

	// The in-memory registration still stands — only the on-disk write failed.
	_, ok := r.Get("write-fail-svc")
	assert.True(t, ok, "in-memory registration should stand even when persistence fails")
}
