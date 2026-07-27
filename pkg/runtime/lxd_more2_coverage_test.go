//go:build !integration

package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLXD_Stop_WithTimeout exercises the `if o.Timeout > 0` branch in
// LXD Stop by passing a non-default timeout. Because the default timeout
// is already > 0, this test also verifies the arguments include
// "--timeout".
func TestLXD_Stop_WithTimeout(t *testing.T) {
	var capturedArgs []string
	exec := &mockExecutor{
		executeFunc: func(
			ctx context.Context, name string, args ...string,
		) ([]byte, error) {
			capturedArgs = args
			return []byte(""), nil
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	err := l.Stop(context.Background(), "my-container",
		WithStopTimeout(5*time.Second))
	assert.NoError(t, err)
	assert.Contains(t, capturedArgs, "--timeout")
	assert.Contains(t, capturedArgs, "5")
}

// TestLXD_Stop_ZeroTimeout exercises the false branch of `if o.Timeout > 0`
// by passing a zero timeout, which skips appending "--timeout".
func TestLXD_Stop_ZeroTimeout(t *testing.T) {
	var capturedArgs []string
	exec := &mockExecutor{
		executeFunc: func(
			ctx context.Context, name string, args ...string,
		) ([]byte, error) {
			capturedArgs = args
			return []byte(""), nil
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	err := l.Stop(context.Background(), "my-container",
		WithStopTimeout(0))
	assert.NoError(t, err)
	assert.NotContains(t, capturedArgs, "--timeout")
}

// TestLXD_Remove_WithForce exercises the `if o.Force` branch in LXD Remove
// by calling Remove with WithForceRemove.
func TestLXD_Remove_WithForce(t *testing.T) {
	var capturedArgs []string
	exec := &mockExecutor{
		executeFunc: func(
			ctx context.Context, name string, args ...string,
		) ([]byte, error) {
			capturedArgs = args
			return []byte(""), nil
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	err := l.Remove(context.Background(), "my-container",
		WithForceRemove(true))
	assert.NoError(t, err)
	assert.Contains(t, capturedArgs, "--force")
}

// TestLXD_Remove_Error exercises the error branch in LXD Remove when
// the executor returns an error.
func TestLXD_Remove_Error(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			ctx context.Context, name string, args ...string,
		) ([]byte, error) {
			return nil, fmt.Errorf("lxc delete failed")
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	err := l.Remove(context.Background(), "my-container")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "lxc delete my-container")
}

// TestParseLXDStatus_UnmarshalError exercises the JSON unmarshal error
// branch of parseLXDStatus.
func TestParseLXDStatus_UnmarshalError(t *testing.T) {
	_, err := parseLXDStatus([]byte("invalid json"), "any")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing lxc list output")
}

// TestParseLXDStatus_EmptyList exercises the "no container found" branch
// of parseLXDStatus when the JSON array is empty.
func TestParseLXDStatus_EmptyList(t *testing.T) {
	_, err := parseLXDStatus([]byte("[]"), "any")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container found")
}

// TestParseLXDStatus_WithNilState exercises the `c.State != nil` branch
// by providing a JSON object where "state" is null. When state is nil the
// StartedAt field is left as the zero time.
func TestParseLXDStatus_WithNilState(t *testing.T) {
	data := `[{"name":"c1","status":"Running","status_code":103,"state":null}]`
	status, err := parseLXDStatus([]byte(data), "c1")
	require.NoError(t, err)
	assert.Equal(t, "c1", status.Name)
	assert.True(t, status.StartedAt.IsZero(),
		"StartedAt should be zero when state is nil")
}

// TestParseLXDStatus_WithNonNilState exercises the `c.State != nil`
// branch by providing a JSON object with a real state, which causes
// StartedAt to be set from LastUsedAt.
func TestParseLXDStatus_WithNonNilState(t *testing.T) {
	data := `[{"name":"c2","status":"Running","status_code":103,` +
		`"last_used_at":"2024-06-01T12:00:00Z",` +
		`"state":{"status":"Running","cpu":{"usage":0},"memory":{"usage":0,"limit":0},"network":{}}}]`
	status, err := parseLXDStatus([]byte(data), "c2")
	require.NoError(t, err)
	assert.Equal(t, "c2", status.Name)
	assert.False(t, status.StartedAt.IsZero(),
		"StartedAt should be set from LastUsedAt when state is non-nil")
}

// TestLXD_Stop_Error exercises the `if err != nil` error branch in LXD
// Stop when the executor returns an error.
func TestLXD_Stop_Error(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(
			ctx context.Context, name string, args ...string,
		) ([]byte, error) {
			return nil, fmt.Errorf("lxc stop failed: container busy")
		},
	}
	l := NewLXDRuntimeWithExecutor(exec)

	err := l.Stop(context.Background(), "busy-container")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lxc stop busy-container")
}
