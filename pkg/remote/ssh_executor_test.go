package remote

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
)

// unreachableHost returns a RemoteHost pointing at TEST-NET-1
// (RFC 5737) which is guaranteed to be unreachable.
func unreachableHost() RemoteHost {
	return RemoteHost{
		Name:    "unreachable",
		Address: "192.0.2.1",
		Port:    22,
		User:    "testuser",
		Auth:    AuthSSHKey,
	}
}

// newTestSSHExecutor creates an SSHExecutor with short timeouts
// and ControlMaster disabled so tests complete quickly.
func newTestSSHExecutor(t *testing.T) *SSHExecutor {
	t.Helper()
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithConnectTimeout(1*time.Second),
		WithCommandTimeout(2*time.Second),
		WithControlMaster(false),
	)
	require.NoError(t, err)
	return exec
}

func TestSSHExecutor_Execute_InvalidHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name    string
		host    RemoteHost
		command string
	}{
		{
			name:    "unreachable host with simple command",
			host:    unreachableHost(),
			command: "echo hello",
		},
		{
			name: "unreachable host with different port",
			host: RemoteHost{
				Name:    "bad-port",
				Address: "192.0.2.1",
				Port:    2222,
				User:    "deploy",
				Auth:    AuthSSHKey,
			},
			command: "hostname",
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, err := exec.Execute(ctx, tc.host, tc.command)
			assert.Error(t, err,
				"Execute should fail for unreachable host")
		})
	}
}

func TestSSHExecutor_Execute_EmptyCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name    string
		host    RemoteHost
		command string
	}{
		{
			name:    "empty command string",
			host:    unreachableHost(),
			command: "",
		},
		{
			name:    "whitespace-only command",
			host:    unreachableHost(),
			command: "   ",
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, err := exec.Execute(ctx, tc.host, tc.command)
			// With an unreachable host, any command (even empty)
			// should fail due to connection timeout.
			assert.Error(t, err,
				"Execute should fail for unreachable host "+
					"regardless of command content")
		})
	}
}

func TestSSHExecutor_CopyFile_InvalidHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name       string
		host       RemoteHost
		localPath  string
		remotePath string
	}{
		{
			name:       "copy to unreachable host",
			host:       unreachableHost(),
			localPath:  "/tmp/nonexistent.txt",
			remotePath: "/remote/file.txt",
		},
		{
			name: "copy with custom port",
			host: RemoteHost{
				Name:    "bad-host",
				Address: "192.0.2.1",
				Port:    2222,
				User:    "deploy",
			},
			localPath:  "/tmp/test.txt",
			remotePath: "/remote/test.txt",
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			defer cancel()

			err := exec.CopyFile(
				ctx, tc.host, tc.localPath, tc.remotePath,
			)
			assert.Error(t, err,
				"CopyFile should fail for unreachable host")
			assert.Contains(t, err.Error(), "scp to")
		})
	}
}

func TestSSHExecutor_CopyDir_InvalidHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name      string
		host      RemoteHost
		localDir  string
		remoteDir string
	}{
		{
			name:      "copy dir to unreachable host",
			host:      unreachableHost(),
			localDir:  "/tmp",
			remoteDir: "/remote/dir",
		},
		{
			name: "copy dir with key path",
			host: RemoteHost{
				Name:    "key-host",
				Address: "192.0.2.1",
				Port:    22,
				User:    "admin",
				KeyPath: "/nonexistent/key",
			},
			localDir:  "/tmp",
			remoteDir: "/remote/data",
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			defer cancel()

			err := exec.CopyDir(
				ctx, tc.host, tc.localDir, tc.remoteDir,
			)
			assert.Error(t, err,
				"CopyDir should fail for unreachable host")
			assert.Contains(t, err.Error(), "scp dir to")
		})
	}
}

func TestSSHExecutor_IsReachable_Unreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name string
		host RemoteHost
	}{
		{
			name: "TEST-NET-1 address",
			host: RemoteHost{
				Name:    "test-net-1",
				Address: "192.0.2.1",
				Port:    22,
				User:    "user",
			},
		},
		{
			name: "TEST-NET-1 with different port",
			host: RemoteHost{
				Name:    "test-net-1-alt",
				Address: "192.0.2.1",
				Port:    2222,
				User:    "deploy",
			},
		},
		{
			name: "TEST-NET-1 with key",
			host: RemoteHost{
				Name:    "test-net-1-key",
				Address: "192.0.2.1",
				Port:    22,
				User:    "admin",
				KeyPath: "/nonexistent/id_rsa",
			},
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			reachable := exec.IsReachable(ctx, tc.host)
			assert.False(t, reachable,
				"IsReachable should return false for "+
					"TEST-NET-1 (192.0.2.1)")
		})
	}
}

func TestSSHExecutor_ExecuteStream_InvalidHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH executor test in short mode") // SKIP-OK: #short-mode
	}

	tests := []struct {
		name    string
		host    RemoteHost
		command string
	}{
		{
			name:    "stream on unreachable host",
			host:    unreachableHost(),
			command: "tail -f /var/log/syslog",
		},
		{
			name: "stream with custom user",
			host: RemoteHost{
				Name:    "stream-bad",
				Address: "192.0.2.1",
				Port:    22,
				User:    "root",
			},
			command: "journalctl -f",
		},
	}

	exec := newTestSSHExecutor(t)
	defer exec.Close()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			defer cancel()

			reader, err := exec.ExecuteStream(
				ctx, tc.host, tc.command,
			)
			if err != nil {
				// Connection failed immediately -- expected.
				assert.Error(t, err)
				return
			}

			// If the stream started, reading should eventually
			// fail because the host is unreachable.
			defer reader.Close()
			buf := make([]byte, 1024)
			_, readErr := reader.Read(buf)
			assert.Error(t, readErr,
				"reading from stream on unreachable host "+
					"should fail")
		})
	}
}

func TestSSHExecutor_NewSSHExecutor_NilLogger(t *testing.T) {
	exec, err := NewSSHExecutor(
		nil,
		WithControlMaster(false),
	)
	require.NoError(t, err)
	defer exec.Close()
	assert.NotNil(t, exec)
	assert.NotNil(t, exec.logger,
		"nil logger should be replaced with NopLogger")
}

func TestSSHExecutor_Close_NoPool(t *testing.T) {
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
	)
	require.NoError(t, err)

	err = exec.Close()
	assert.NoError(t, err,
		"Close without pool should succeed")
}

// containsSequence reports whether the needles appear in argv
// in order (not necessarily contiguous). Used to assert that a
// flag and its value appear consecutively.
func containsSequence(argv []string, needle ...string) bool {
	for i := 0; i+len(needle) <= len(argv); i++ {
		match := true
		for j, n := range needle {
			if argv[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSSHExecutor_SSHArgs_KeepAliveDefault verifies that the
// default options produce ssh -o ServerAliveInterval/CountMax
// flags — without these, long compose builds silently drop the
// SSH session when the network stalls (exit code -1).
func TestSSHExecutor_SSHArgs_KeepAliveDefault(t *testing.T) {
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
	)
	require.NoError(t, err)
	defer exec.Close()

	args, err := exec.sshArgs(context.Background(), unreachableHost())
	require.NoError(t, err)

	assert.True(t,
		containsSequence(args, "-o", "ServerAliveInterval=30"),
		"default KeepAlive=30s must emit ServerAliveInterval=30; "+
			"got args: %v", args)
	assert.True(t,
		containsSequence(args, "-o", "ServerAliveCountMax=10"),
		"default KeepAliveCountMax=10 must emit "+
			"ServerAliveCountMax=10; got args: %v", args)
}

// TestSSHExecutor_SSHArgs_KeepAliveDisabled verifies that
// WithKeepAlive(0) disables the probes entirely.
func TestSSHExecutor_SSHArgs_KeepAliveDisabled(t *testing.T) {
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
		WithKeepAlive(0),
	)
	require.NoError(t, err)
	defer exec.Close()

	args, err := exec.sshArgs(context.Background(), unreachableHost())
	require.NoError(t, err)

	for _, a := range args {
		assert.NotContains(t, a, "ServerAliveInterval",
			"KeepAlive=0 must NOT emit ServerAliveInterval")
		assert.NotContains(t, a, "ServerAliveCountMax",
			"KeepAlive=0 must NOT emit ServerAliveCountMax")
	}
}

// TestSSHExecutor_SSHArgs_KeepAliveCustom verifies that non-default
// values are propagated.
func TestSSHExecutor_SSHArgs_KeepAliveCustom(t *testing.T) {
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
		WithKeepAlive(45*time.Second),
		WithKeepAliveCountMax(20),
	)
	require.NoError(t, err)
	defer exec.Close()

	args, err := exec.sshArgs(context.Background(), unreachableHost())
	require.NoError(t, err)

	assert.True(t,
		containsSequence(args, "-o", "ServerAliveInterval=45"),
		"custom 45s interval must be propagated; args: %v", args)
	assert.True(t,
		containsSequence(args, "-o", "ServerAliveCountMax=20"),
		"custom count 20 must be propagated; args: %v", args)
}

// TestSSHExecutor_SCPArgs_KeepAlive verifies scpArgs also
// includes keep-alive (parity with sshArgs for long file copies).
func TestSSHExecutor_SCPArgs_KeepAlive(t *testing.T) {
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
	)
	require.NoError(t, err)
	defer exec.Close()

	args, err := exec.scpArgs(context.Background(), unreachableHost())
	require.NoError(t, err)

	assert.True(t,
		containsSequence(args, "-o", "ServerAliveInterval=30"),
		"scpArgs must emit ServerAliveInterval for default "+
			"KeepAlive=30s; got args: %v", args)
	assert.True(t,
		containsSequence(args, "-o", "ServerAliveCountMax=10"),
		"scpArgs must emit ServerAliveCountMax for default "+
			"KeepAliveCountMax=10; got args: %v", args)
}
