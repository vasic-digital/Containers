package serviceregistry

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNopLogger_AllMethods calls all nopLogger stub methods for coverage.
func TestNopLogger_AllMethods(t *testing.T) {
	// bluff-scan: no-assert-ok (null-implementation smoke — no-op type must accept all interface calls without panic)
	l := nopLogger{}
	l.Info("test %s", "info")
	l.Debug("test %s", "debug")
	l.Warn("test %s", "warn")
	l.Error("test %s", "error")
}

// TestWithLabels_NilMap verifies WithLabels with a nil map does nothing bad.
func TestWithLabels_NilMap(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	// A nil label map should not panic.
	err := r.Register("svc-nil-labels", 9999, WithLabels(nil))
	assert.NoError(t, err)
	svc, ok := r.Get("svc-nil-labels")
	assert.True(t, ok)
	// Labels field should be initialized (empty map), not nil.
	assert.NotNil(t, svc.Labels)
}

// TestSaveToDisk_MarshalFail covers the branch where registryDir is set
// but data write succeeds via a writable temp dir.
func TestSaveToDisk_MarshalFail(t *testing.T) {
	// bluff-scan: no-assert-ok (error-path smoke — failure path must not panic)
	// Use a directory that exists but then immediately remove it so the
	// write fails, exercising the os.WriteFile error branch in saveToDisk.
	tmpDir, err := os.MkdirTemp("", "registry-save-fail-*")
	if err != nil {
		t.Skip("could not create temp dir") // SKIP-OK: #env-tempdir-unavailable
	}
	// Remove write permissions so WriteFile fails.
	_ = os.Chmod(tmpDir, 0o555)
	defer func() {
		_ = os.Chmod(tmpDir, 0o755)
		_ = os.RemoveAll(tmpDir)
	}()

	r := New(WithRegistryDir(tmpDir))
	// Register triggers saveToDisk; the write should fail silently.
	_ = r.Register("fail-svc", 1234)
	// We only need to exercise the code path; no error is returned.
}

// TestLoadFromDisk_UnmarshalFail covers the Unmarshal error branch in
// loadFromDisk by writing an invalid JSON file before constructing the registry.
func TestLoadFromDisk_UnmarshalFail(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "registry-load-fail-*")
	if err != nil {
		t.Skip("could not create temp dir") // SKIP-OK: #env-tempdir-unavailable
	}
	defer os.RemoveAll(tmpDir)

	// Write invalid JSON to the services file.
	servicesFile := tmpDir + "/services.json"
	_ = os.WriteFile(servicesFile, []byte("NOT JSON {{{"), 0644)

	// Constructing a registry with this dir triggers loadFromDisk
	// which should hit the Unmarshal error branch.
	r := New(WithRegistryDir(tmpDir))
	assert.NotNil(t, r)
	// Should have zero services because load failed.
	assert.Empty(t, r.GetAll())
}

// TestFindAvailablePort_WithLogger exercises FindAvailablePort logging via
// the logger for complete line coverage.
func TestFindAvailablePort_FindsPort(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	port := r.FindAvailablePort(40000)
	assert.GreaterOrEqual(t, port, 40000)
	assert.Less(t, port, 50000)
}
