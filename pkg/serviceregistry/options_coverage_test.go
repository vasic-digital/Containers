package serviceregistry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// mockLogger implements the Logger interface.
type mockLogger struct {
	lastMsg string
}

func (m *mockLogger) Info(msg string, args ...any)  { m.lastMsg = msg }
func (m *mockLogger) Debug(msg string, args ...any) { m.lastMsg = msg }
func (m *mockLogger) Warn(msg string, args ...any)  { m.lastMsg = msg }
func (m *mockLogger) Error(msg string, args ...any) { m.lastMsg = msg }

func TestWithLogger(t *testing.T) {
	ml := &mockLogger{}
	r := New(WithLogger(ml))
	assert.NotNil(t, r)
	// Confirm logger is set by registering a service (triggers Info log)
	_ = r.Register("test-svc", 9999)
	assert.NotEmpty(t, ml.lastMsg)
}

func TestWithDefaultHost_RegistryOption(t *testing.T) {
	r := New(WithDefaultHost("192.168.1.100"))
	assert.Equal(t, "192.168.1.100", r.defaultHost)
}

func TestWithRegistryDir(t *testing.T) {
	r := New(WithRegistryDir("/tmp/test-registry"))
	assert.Equal(t, "/tmp/test-registry", r.registryDir)
}

func TestWithHost_ServiceOption(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	_ = r.Register("svc", 8080, WithHost("10.0.0.1"))
	svc, ok := r.Get("svc")
	assert.True(t, ok)
	assert.Equal(t, "10.0.0.1", svc.Host)
}

func TestWithHealthPath_ServiceOption(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	_ = r.Register("svc", 8080, WithHealthPath("/health"))
	svc, ok := r.Get("svc")
	assert.True(t, ok)
	assert.Equal(t, "/health", svc.HealthPath)
}

func TestWithHealthType_ServiceOption(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	_ = r.Register("svc", 8080, WithHealthType("http"))
	svc, ok := r.Get("svc")
	assert.True(t, ok)
	assert.Equal(t, "http", svc.HealthType)
}

func TestWithProtocol_ServiceOption(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	_ = r.Register("svc", 8080, WithProtocol("https"))
	svc, ok := r.Get("svc")
	assert.True(t, ok)
	assert.Equal(t, "https", svc.Protocol)
}

func TestWithLabels_ServiceOption(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	labels := map[string]string{"env": "prod", "tier": "web"}
	_ = r.Register("svc", 8080, WithLabels(labels))
	svc, ok := r.Get("svc")
	assert.True(t, ok)
	assert.Equal(t, "prod", svc.Labels["env"])
	assert.Equal(t, "web", svc.Labels["tier"])
}

func TestGetURL_HTTPS(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	_ = r.Register("svc", 8443, WithProtocol("https"))
	url := r.GetURL("svc")
	assert.Equal(t, "https://localhost:8443", url)
}

func TestGetURL_NotFound(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	url := r.GetURL("nonexistent")
	assert.Empty(t, url)
}

func TestGetEndpoint_NotFound(t *testing.T) {
	r := New(WithRegistryDir(t.TempDir()))
	ep := r.GetEndpoint("nonexistent")
	assert.Empty(t, ep)
}
