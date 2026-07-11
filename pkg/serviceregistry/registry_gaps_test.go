package serviceregistry

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithLabels_NilServiceLabels exercises the s.Labels == nil branch
// by applying WithLabels to a freshly-created Service that has nil Labels.
func TestWithLabels_NilServiceLabels(t *testing.T) {
	s := &Service{} // Labels is nil by default
	opt := WithLabels(map[string]string{"key": "val"})
	opt(s)
	assert.Equal(t, "val", s.Labels["key"])
}

// TestWithLabels_EmptyMap exercises the range loop with no iterations.
func TestWithLabels_EmptyMap(t *testing.T) {
	s := &Service{}
	opt := WithLabels(map[string]string{})
	opt(s)
	assert.NotNil(t, s.Labels)
	assert.Empty(t, s.Labels)
}

// TestIsPortAvailable_Available verifies isPortAvailable returns true
// for a port that is not in use (exercises the ln.Close() true branch).
func TestIsPortAvailable_Available(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	// FindAvailablePort already exercises the true path; call it here.
	port := r.FindAvailablePort(41000)
	require.Greater(t, port, 0)
	// Directly verify isPortAvailable returns true for the found port.
	assert.True(t, r.isPortAvailable(port))
}

// TestIsPortAvailable_Occupied verifies isPortAvailable returns false
// when a port is already in use (exercises the err != nil branch).
func TestIsPortAvailable_Occupied(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	r := New(WithRegistryDir(t.TempDir()))
	assert.False(t, r.isPortAvailable(addr.Port))
}

// TestFindAvailablePort_ReturnsZero exercises the "no available port"
// return 0 branch by using a range of 1 port that is already in use.
func TestFindAvailablePort_ReturnsZero(t *testing.T) {
	// Occupy a port.
	ln, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port

	r := New(WithRegistryDir(t.TempDir()))
	// Use a range of exactly 1 port that is occupied.
	// startPort + 10000 > port, so we need to override indirectly.
	// Instead, intercept by using defaultHost set to an invalid addr
	// so all isPortAvailable calls fail.
	r.defaultHost = "192.0.2.1" // TEST-NET, won't bind
	result := r.FindAvailablePort(port)
	// All ports should fail to listen -> returns 0.
	assert.Equal(t, 0, result)
	_ = port
}

// TestSaveToDisk_MkdirFail covers the MkdirAll failure branch in saveToDisk
// by pointing the registry at a path that can't be created.
func TestSaveToDisk_MkdirFail(t *testing.T) {
	// Create a file where a directory is expected so MkdirAll fails.
	tmpDir, err := os.MkdirTemp("", "registry-mkdir-fail-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a file at the path that saveToDisk would try to mkdir.
	blockPath := tmpDir + "/subdir"
	require.NoError(t, os.WriteFile(blockPath, []byte("block"), 0644))

	r := New(WithRegistryDir(blockPath + "/nested"))
	_ = r.Register("test-svc", 9090)
	// saveToDisk: MkdirAll will fail because blockPath is a file.
}

// TestLoadFromDisk_EmptyRegistryDir covers the early return in loadFromDisk.
func TestLoadFromDisk_EmptyRegistryDir(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	// Should not panic; registryDir is empty -> early return.
	r.loadFromDisk()
}
