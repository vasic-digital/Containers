//go:build security

package security

import (
	"context"
	"testing"
	"time"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSHExecutor_InvalidHost verifies that SSHExecutor.Execute
// fails when the target host is unreachable.
func TestSSHExecutor_InvalidHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping security test in short mode") // SKIP-OK: #short-mode
	}

	executor, err := remote.NewSSHExecutor(
		logging.NopLogger{},
		remote.WithControlMaster(false),
		remote.WithConnectTimeout(2*time.Second),
		remote.WithCommandTimeout(3*time.Second),
	)
	require.NoError(t, err,
		"creating SSH executor should not fail")
	defer executor.Close()

	host := remote.RemoteHost{
		Name:    "unreachable-host",
		Address: "192.0.2.1", // TEST-NET-1: guaranteed unreachable
		Port:    22,
		User:    "testuser",
		Auth:    remote.AuthSSHKey,
		KeyPath: "/nonexistent/key",
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	result, execErr := executor.Execute(ctx, host, "echo hello")
	// We expect either an error or a non-zero exit code.
	if execErr == nil && result != nil {
		assert.NotEqual(t, 0, result.ExitCode,
			"exit code should be non-zero for unreachable host")
	} else {
		assert.Error(t, execErr,
			"execute on unreachable host should return error")
	}
}

// TestSSHExecutor_EmptyCommand verifies that executing an empty
// command on an unreachable host still results in a connection
// error rather than a success.
func TestSSHExecutor_EmptyCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping security test in short mode") // SKIP-OK: #short-mode
	}

	executor, err := remote.NewSSHExecutor(
		logging.NopLogger{},
		remote.WithControlMaster(false),
		remote.WithConnectTimeout(2*time.Second),
		remote.WithCommandTimeout(3*time.Second),
	)
	require.NoError(t, err,
		"creating SSH executor should not fail")
	defer executor.Close()

	host := remote.RemoteHost{
		Name:    "empty-cmd-host",
		Address: "192.0.2.1",
		Port:    22,
		User:    "testuser",
		Auth:    remote.AuthSSHKey,
		KeyPath: "/nonexistent/key",
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	result, execErr := executor.Execute(ctx, host, "")
	// With an unreachable host, the connection itself should
	// fail regardless of the command.
	if execErr == nil && result != nil {
		assert.NotEqual(t, 0, result.ExitCode,
			"empty command on unreachable host should not "+
				"succeed with exit code 0")
	} else {
		assert.Error(t, execErr,
			"empty command on unreachable host should error")
	}
}

// TestConnectionPool_InvalidSocket verifies that
// NewConnectionPool handles an invalid socket directory
// gracefully by either creating it or reporting an error.
func TestConnectionPool_InvalidSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping security test in short mode") // SKIP-OK: #short-mode
	}

	// Use a path under /proc which cannot have directories
	// created inside it.
	opts := remote.ApplyOptions([]remote.Option{
		remote.WithControlMaster(true),
		remote.WithControlSocketDir(
			"/proc/nonexistent-socket-dir-12345",
		),
	})

	pool, err := remote.NewConnectionPool(opts)
	// Creating a pool with an invalid directory should fail.
	assert.Error(t, err,
		"creating connection pool with invalid socket dir "+
			"should fail")
	assert.Nil(t, pool,
		"pool should be nil when creation fails")
}

// TestHostManager_DuplicateHost verifies that adding a host
// with a name that already exists returns an error.
func TestHostManager_DuplicateHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping security test in short mode") // SKIP-OK: #short-mode
	}

	executor, err := remote.NewSSHExecutor(
		logging.NopLogger{},
		remote.WithControlMaster(false),
	)
	require.NoError(t, err)
	defer executor.Close()

	hm := remote.NewHostManager(executor, logging.NopLogger{})
	require.NotNil(t, hm)

	host := remote.RemoteHost{
		Name:    "duplicate-test",
		Address: "10.0.0.1",
		Port:    22,
		User:    "admin",
		Auth:    remote.AuthSSHKey,
	}

	// First addition should succeed.
	err = hm.AddHost(host)
	require.NoError(t, err,
		"first AddHost should succeed")

	// Second addition with the same name should fail.
	err = hm.AddHost(host)
	require.Error(t, err,
		"duplicate AddHost should return error")
	assert.Contains(t, err.Error(), "already registered",
		"error should indicate duplicate host")
}
