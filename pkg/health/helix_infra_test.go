package health

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHelixServiceHealthChecker(t *testing.T) {
	h := NewHelixServiceHealthChecker("postgres-primary")
	require.NotNil(t, h)
	assert.Equal(t, "postgres-primary", h.ServiceName)
	assert.Equal(t, "tcp", h.CheckType)
	assert.Equal(t, 5432, h.Port)

	h2 := NewHelixServiceHealthChecker("nonexistent")
	assert.Nil(t, h2)
}

func TestHelixServiceHealthChecker_Check_TCP(t *testing.T) {
	// Use a port that's unlikely to be open
	h := &HelixServiceHealthChecker{
		ServiceName: "test-tcp",
		CheckType:   "tcp",
		Host:        "localhost",
		Port:        1,
		Timeout:     1 * time.Second,
		Retries:     0,
	}
	status, err := h.Check(context.Background())
	assert.Error(t, err)
	assert.False(t, status.Healthy)
}

func TestHelixServiceHealthChecker_Check_HTTP(t *testing.T) {
	h := &HelixServiceHealthChecker{
		ServiceName: "test-http",
		CheckType:   "http",
		Host:        "localhost",
		Port:        1,
		Path:        "/",
		Timeout:     1 * time.Second,
		Retries:     0,
	}
	status, err := h.Check(context.Background())
	assert.Error(t, err)
	assert.False(t, status.Healthy)
}

func TestHelixServiceHealthChecker_Nil(t *testing.T) {
	var h *HelixServiceHealthChecker
	status, err := h.Check(context.Background())
	assert.Error(t, err)
	assert.False(t, status.Healthy)
}

func TestAllHelixHealthCheckers(t *testing.T) {
	checkers := AllHelixHealthCheckers()
	assert.Len(t, checkers, 20)
	assert.Contains(t, checkers, "postgres-primary")
	assert.Contains(t, checkers, "vault")
}

// Paired mutation test
func TestHelixServiceHealthChecker_CheckType_Mutation(t *testing.T) {
	h := NewHelixServiceHealthChecker("postgres-primary")
	require.NotNil(t, h)
	assert.Equal(t, "tcp", h.CheckType)
	assert.Equal(t, 5432, h.Port)

	h2 := NewHelixServiceHealthChecker("nats")
	require.NotNil(t, h2)
	assert.Equal(t, "http", h2.CheckType)
	assert.Equal(t, 8222, h2.Port)
	assert.Equal(t, "/healthz", h2.Path)
}
