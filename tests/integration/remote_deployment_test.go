package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/envconfig"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Integration tests for remote deployment scenarios.
// These tests require:
// 1. A remote host configured in tests/configs/.env.remote-single
// 2. SSH access to the remote host (key-based auth)
// 3. Podman or Docker installed on the remote host

// skipIfNoRemoteHost skips the test if no remote host is configured.
func skipIfNoRemoteHost(t *testing.T) *envconfig.DistributionConfig {
	t.Helper()

	cfg := envconfig.LoadFromEnv()
	if !cfg.Enabled || len(cfg.ToRemoteHosts()) == 0 {
		t.Skip("No remote host configured - set CONTAINERS_REMOTE_ENABLED=true and CONTAINERS_REMOTE_HOST_1_*") // SKIP-OK: #env-remote-disabled
	}

	return cfg
}

// TestRemoteDeployment_SSHConnection tests SSH connection to remote host.
func TestRemoteDeployment_SSHConnection(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	require.NotEmpty(t, hosts)

	host := hosts[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := executor.Execute(ctx, host, "echo hello")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "hello")
}

// TestRemoteDeployment_ComposeDetection tests compose command detection on remote host.
func TestRemoteDeployment_ComposeDetection(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	detector := remote.NewComposeDetector(executor, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, err := detector.Detect(ctx, host)
	require.NoError(t, err)
	assert.NotEmpty(t, cmd.Name)
	assert.NotEmpty(t, cmd.Binary)
	t.Logf("Detected compose command: %s (version: %s)", cmd.Name, cmd.Version)
}

// TestRemoteDeployment_ComposeDetection_PodmanComposePreferred tests that podman-compose
// is preferred over "podman compose" when available.
func TestRemoteDeployment_ComposeDetection_PodmanComposePreferred(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	// Check if podman-compose exists
	ctx := context.Background()
	result, _ := executor.Execute(ctx, host, "which podman-compose")

	detector := remote.NewComposeDetector(executor, logger)
	cmd, err := detector.Detect(ctx, host)
	require.NoError(t, err)

	// If podman-compose exists, it should be detected
	if result != nil && result.ExitCode == 0 {
		assert.Equal(t, "podman-compose", cmd.Name, "podman-compose should be preferred when available")
	}
}

// TestRemoteDeployment_ComposeUp tests deploying a compose file to remote host.
func TestRemoteDeployment_ComposeUp(t *testing.T) {
	if os.Getenv("CONTAINERS_INTEGRATION_TEST") == "" {
		t.Skip("Set CONTAINERS_INTEGRATION_TEST=1 to run integration tests") // SKIP-OK: #integration-mode-only
	}

	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	orch := remote.NewRemoteComposeOrchestrator(host, executor, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Create a simple test compose content
	testCompose := `
services:
  test-nginx:
    image: docker.io/nginx:alpine
    ports:
      - "8888:80"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/"]
      interval: 5s
      timeout: 3s
      retries: 3
`

	// Write test compose to temp file
	tmpFile, err := os.CreateTemp("", "test-compose-*.yml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	_, err = tmpFile.WriteString(testCompose)
	require.NoError(t, err)
	tmpFile.Close()

	// Copy compose file to remote host
	remotePath := "/tmp/test-compose-" + time.Now().Format("20060102-150405") + ".yml"
	err = executor.CopyFile(ctx, host, tmpFile.Name(), remotePath)
	require.NoError(t, err)
	defer executor.Execute(ctx, host, "rm -f "+remotePath)

	// Deploy
	project := compose.ComposeProject{
		File: remotePath,
		Name: "test-integration",
	}

	err = orch.Up(ctx, project)
	require.NoError(t, err)

	// Verify container is running
	time.Sleep(5 * time.Second)
	statuses, err := orch.Status(ctx, project)
	require.NoError(t, err)
	assert.NotEmpty(t, statuses)

	// Cleanup
	_ = orch.Down(ctx, project)
}

// TestRemoteDeployment_HealthCheck tests remote health checking.
func TestRemoteDeployment_HealthCheck(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	ctx := context.Background()

	// Test TCP health check (SSH port should be open)
	result, err := executor.Execute(ctx, host, "nc -z localhost 22 && echo OK")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "OK")
}

// TestRemoteDeployment_HostResources tests resource probing on remote host.
func TestRemoteDeployment_HostResources(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	ctx := context.Background()

	// Probe CPU
	cpuResult, err := executor.Execute(ctx, host, "nproc")
	require.NoError(t, err)
	t.Logf("Remote host CPU cores: %s", cpuResult.Stdout)

	// Probe Memory
	memResult, err := executor.Execute(ctx, host, "free -m | awk '/^Mem:/{print $2}'")
	require.NoError(t, err)
	t.Logf("Remote host total memory (MB): %s", memResult.Stdout)

	// Probe Disk
	diskResult, err := executor.Execute(ctx, host, "df -BG / | awk 'NR==2{print $4}'")
	require.NoError(t, err)
	t.Logf("Remote host available disk (GB): %s", diskResult.Stdout)
}

// TestRemoteDeployment_ControlMaster tests SSH ControlMaster optimization.
func TestRemoteDeployment_ControlMaster(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(
		logger,
		remote.WithControlMaster(true),
		remote.WithControlPersist(60*time.Second),
	)
	require.NoError(t, err)
	defer executor.Close()

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	ctx := context.Background()

	// Execute multiple commands - should reuse connection
	start := time.Now()
	for i := 0; i < 5; i++ {
		_, err := executor.Execute(ctx, host, "echo test")
		require.NoError(t, err)
	}
	elapsed := time.Since(start)

	t.Logf("5 commands with ControlMaster: %v", elapsed)
	assert.Less(t, elapsed.Seconds(), 5.0, "Commands should be fast with ControlMaster")
}

// TestRemoteDeployment_RuntimeInfo tests getting runtime info from remote host.
func TestRemoteDeployment_RuntimeInfo(t *testing.T) {
	cfg := skipIfNoRemoteHost(t)

	logger := logging.NopLogger{}
	executor, err := remote.NewSSHExecutor(logger)
	require.NoError(t, err)

	hosts := cfg.ToRemoteHosts()
	host := hosts[0]

	ctx := context.Background()

	// Check if Podman is available
	podmanResult, _ := executor.Execute(ctx, host, "podman --version")
	if podmanResult != nil && podmanResult.ExitCode == 0 {
		t.Logf("Podman available: %s", podmanResult.Stdout)
	}

	// Check if Docker is available
	dockerResult, _ := executor.Execute(ctx, host, "docker --version")
	if dockerResult != nil && dockerResult.ExitCode == 0 {
		t.Logf("Docker available: %s", dockerResult.Stdout)
	}
}
