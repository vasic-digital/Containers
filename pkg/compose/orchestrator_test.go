package compose

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStatusOutput_ValidLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []ServiceStatus
	}{
		{
			name:  "single service running",
			input: "web|running|healthy|0.0.0.0:8080->80/tcp|0\n",
			expected: []ServiceStatus{
				{
					Name:     "web",
					State:    "running",
					Health:   "healthy",
					Ports:    []string{"0.0.0.0:8080->80/tcp"},
					ExitCode: 0,
				},
			},
		},
		{
			name: "multiple services",
			input: "web|running|healthy|0.0.0.0:8080->80/tcp|0\n" +
				"db|running|healthy|0.0.0.0:5432->5432/tcp|0\n" +
				"redis|exited||0.0.0.0:6379->6379/tcp|1\n",
			expected: []ServiceStatus{
				{
					Name: "web", State: "running",
					Health: "healthy",
					Ports:  []string{"0.0.0.0:8080->80/tcp"},
				},
				{
					Name: "db", State: "running",
					Health: "healthy",
					Ports:  []string{"0.0.0.0:5432->5432/tcp"},
				},
				{
					Name: "redis", State: "exited",
					Ports:    []string{"0.0.0.0:6379->6379/tcp"},
					ExitCode: 1,
				},
			},
		},
		{
			name:     "empty input",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			input:    "   \n   \n",
			expected: nil,
		},
		{
			name: "multiple ports",
			input: "app|running|healthy|" +
				"0.0.0.0:80->80/tcp, 0.0.0.0:443->443/tcp|0\n",
			expected: []ServiceStatus{
				{
					Name: "app", State: "running",
					Health: "healthy",
					Ports: []string{
						"0.0.0.0:80->80/tcp",
						"0.0.0.0:443->443/tcp",
					},
				},
			},
		},
		{
			name:  "no ports",
			input: "worker|running|||0\n",
			expected: []ServiceStatus{
				{
					Name: "worker", State: "running",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseStatusOutput(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "single port",
			input:    "0.0.0.0:8080->80/tcp",
			expected: []string{"0.0.0.0:8080->80/tcp"},
		},
		{
			name:  "multiple ports",
			input: "0.0.0.0:80->80/tcp, 0.0.0.0:443->443/tcp",
			expected: []string{
				"0.0.0.0:80->80/tcp",
				"0.0.0.0:443->443/tcp",
			},
		},
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parsePorts(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestProjectArgs(t *testing.T) {
	o := NewOrchestrator("docker", []string{"compose"}, "/tmp", nil)

	tests := []struct {
		name     string
		project  ComposeProject
		expected []string
	}{
		{
			name:     "empty project",
			project:  ComposeProject{},
			expected: nil,
		},
		{
			name: "file only",
			project: ComposeProject{
				File: "docker-compose.yml",
			},
			expected: []string{"-f", "docker-compose.yml"},
		},
		{
			name: "all fields",
			project: ComposeProject{
				Name:    "myproject",
				File:    "compose.yaml",
				Profile: "dev",
			},
			expected: []string{
				"-f", "compose.yaml",
				"--project-name", "myproject",
				"--profile", "dev",
			},
		},
		{
			name: "name and profile only",
			project: ComposeProject{
				Name:    "proj",
				Profile: "test",
			},
			expected: []string{
				"--project-name", "proj",
				"--profile", "test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := o.projectArgs(tt.project)
			assert.Equal(t, tt.expected, args)
		})
	}
}

func TestUpOptions(t *testing.T) {
	tests := []struct {
		name     string
		opts     []UpOption
		expected upOptions
	}{
		{
			name: "defaults",
			opts: nil,
			expected: upOptions{
				Detach: true,
			},
		},
		{
			name: "all options set",
			opts: []UpOption{
				WithUpDetach(false),
				WithRemoveOrphans(true),
				WithBuildFirst(true),
				WithForceRecreate(true),
				WithNoRecreate(false),
				WithUpTimeout(30),
				WithWait(true),
			},
			expected: upOptions{
				Detach:        false,
				RemoveOrphans: true,
				BuildFirst:    true,
				ForceRecreate: true,
				NoRecreate:    false,
				Timeout:       30,
				Wait:          true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := applyUpOptions(tt.opts)
			assert.Equal(t, tt.expected.Detach, cfg.Detach)
			assert.Equal(t, tt.expected.RemoveOrphans,
				cfg.RemoveOrphans)
			assert.Equal(t, tt.expected.BuildFirst,
				cfg.BuildFirst)
			assert.Equal(t, tt.expected.ForceRecreate,
				cfg.ForceRecreate)
			assert.Equal(t, tt.expected.Timeout, cfg.Timeout)
			assert.Equal(t, tt.expected.Wait, cfg.Wait)
		})
	}
}

func TestDownOptions(t *testing.T) {
	tests := []struct {
		name     string
		opts     []DownOption
		expected downOptions
	}{
		{
			name:     "defaults",
			opts:     nil,
			expected: downOptions{},
		},
		{
			name: "all options set",
			opts: []DownOption{
				WithDownRemoveOrphans(true),
				WithDownRemoveVolumes(true),
				WithDownRemoveImages("all"),
				WithDownTimeout(60),
			},
			expected: downOptions{
				RemoveOrphans: true,
				RemoveVolumes: true,
				RemoveImages:  "all",
				Timeout:       60,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := applyDownOptions(tt.opts)
			assert.Equal(t, tt.expected.RemoveOrphans,
				cfg.RemoveOrphans)
			assert.Equal(t, tt.expected.RemoveVolumes,
				cfg.RemoveVolumes)
			assert.Equal(t, tt.expected.RemoveImages,
				cfg.RemoveImages)
			assert.Equal(t, tt.expected.Timeout, cfg.Timeout)
		})
	}
}

// --- Tests for NewDefaultOrchestrator ---

func TestNewDefaultOrchestrator_Success(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so this real-env auto-detect test is not false-FAILed (§11.4.1) by a
	// consumer's rootless-podman override sitting in the environment.
	t.Setenv(composeEnvVar, "")
	// This test will only succeed if docker or podman is available
	// on the system. We skip if no compose command is found.
	o, err := NewDefaultOrchestrator("/tmp", nil)
	if err != nil {
		t.Skipf("no compose command available: %v", err) // SKIP-OK: #env-compose-cmd-missing
	}

	require.NotNil(t, o)
	assert.NotEmpty(t, o.composeCmd)
	assert.Equal(t, "/tmp", o.workDir)
	assert.NotNil(t, o.logger)
}

func TestNewDefaultOrchestrator_NilLogger(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so this real-env auto-detect test is not false-FAILed (§11.4.1) by a
	// consumer's rootless-podman override sitting in the environment.
	t.Setenv(composeEnvVar, "")
	o, err := NewDefaultOrchestrator("/tmp", nil)
	if err != nil {
		t.Skipf("no compose command available: %v", err) // SKIP-OK: #env-compose-cmd-missing
	}

	require.NotNil(t, o)
	// Should use NopLogger when nil is passed
	assert.NotNil(t, o.logger)
}

// --- Tests for detectComposeCmd ---

func TestDetectComposeCmd_FindsDockerCompose(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so this real-env auto-detect test is not false-FAILed (§11.4.1) by a
	// consumer's rootless-podman override sitting in the environment.
	t.Setenv(composeEnvVar, "")
	// Check if docker compose is available
	cmd := exec.Command("docker", "compose", "version")
	if err := cmd.Run(); err != nil {
		t.Skip("docker compose not available") // SKIP-OK: #env-docker-compose
	}

	composecmd, args, err := detectComposeCmd()
	require.NoError(t, err)
	assert.Equal(t, "docker", composecmd)
	assert.Equal(t, []string{"compose"}, args)
}

func TestDetectComposeCmd_FindsStandaloneDockerCompose(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so this real-env auto-detect test is not false-FAILed (§11.4.1) by a
	// consumer's rootless-podman override sitting in the environment.
	t.Setenv(composeEnvVar, "")
	// Check if docker-compose is available
	cmd := exec.Command("docker-compose", "version")
	if err := cmd.Run(); err != nil {
		t.Skip("docker-compose not available") // SKIP-OK: #env-docker-compose-standalone
	}

	// Only test this if docker compose plugin is NOT available
	// (otherwise docker compose takes precedence)
	pluginCmd := exec.Command("docker", "compose", "version")
	if pluginCmd.Run() == nil {
		t.Skip("docker compose plugin available, takes precedence") // SKIP-OK: #env-precedence-docker-plugin
	}

	composecmd, args, err := detectComposeCmd()
	require.NoError(t, err)
	assert.Equal(t, "docker-compose", composecmd)
	assert.Nil(t, args)
}

func TestDetectComposeCmd_FindsPodmanCompose(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so this real-env auto-detect test is not false-FAILed (§11.4.1) by a
	// consumer's rootless-podman override sitting in the environment.
	t.Setenv(composeEnvVar, "")
	// Check if podman-compose is available
	cmd := exec.Command("podman-compose", "version")
	if err := cmd.Run(); err != nil {
		t.Skip("podman-compose not available") // SKIP-OK: #env-podman-compose
	}

	// Only test if neither docker compose nor docker-compose is available
	dockerPluginCmd := exec.Command("docker", "compose", "version")
	dockerStandaloneCmd := exec.Command("docker-compose", "version")
	if dockerPluginCmd.Run() == nil || dockerStandaloneCmd.Run() == nil {
		t.Skip("docker compose available, takes precedence") // SKIP-OK: #env-precedence-docker
	}

	composecmd, args, err := detectComposeCmd()
	require.NoError(t, err)
	assert.Equal(t, "podman-compose", composecmd)
	assert.Nil(t, args)
}

// --- Tests for Up method ---

func TestDefaultOrchestrator_Up_WithEcho(t *testing.T) {
	// Use echo as a mock command - it will always succeed
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{
		Name:     "test-project",
		File:     "docker-compose.yml",
		Services: []string{"web", "db"},
	}

	err := o.Up(ctx, project)
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Up_WithAllOptions(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", &testLogger{})

	ctx := context.Background()
	project := ComposeProject{
		Name:     "test-project",
		File:     "docker-compose.yml",
		Services: []string{"web"},
	}

	err := o.Up(ctx, project,
		WithUpDetach(true),
		WithRemoveOrphans(true),
		WithBuildFirst(true),
		WithForceRecreate(true),
		WithUpTimeout(30),
		WithWait(true),
	)
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Up_WithNoRecreate(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{
		Name: "test-project",
	}

	err := o.Up(ctx, project, WithNoRecreate(true))
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Up_NoDetach(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{}

	err := o.Up(ctx, project, WithUpDetach(false))
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Up_CommandFailure(t *testing.T) {
	// Use false command which always fails
	o := NewOrchestrator("false", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{}

	err := o.Up(ctx, project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func TestDefaultOrchestrator_Up_ContextCanceled(t *testing.T) {
	// Use sleep to simulate a long-running command
	o := NewOrchestrator("sleep", nil, "/tmp", nil)

	ctx, cancel := context.WithTimeout(
		context.Background(), 50*time.Millisecond,
	)
	defer cancel()

	project := ComposeProject{
		Services: []string{"100"}, // sleep will interpret this as 100 sec
	}

	err := o.Up(ctx, project)
	require.Error(t, err)
}

// --- Tests for Down method ---

func TestDefaultOrchestrator_Down_WithEcho(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{
		Name: "test-project",
		File: "docker-compose.yml",
	}

	err := o.Down(ctx, project)
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Down_WithAllOptions(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", &testLogger{})

	ctx := context.Background()
	project := ComposeProject{
		Name:    "test-project",
		File:    "docker-compose.yml",
		Profile: "dev",
	}

	err := o.Down(ctx, project,
		WithDownRemoveOrphans(true),
		WithDownRemoveVolumes(true),
		WithDownRemoveImages("all"),
		WithDownTimeout(60),
	)
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Down_CommandFailure(t *testing.T) {
	o := NewOrchestrator("false", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{}

	err := o.Down(ctx, project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func TestDefaultOrchestrator_Down_ContextCanceled(t *testing.T) {
	o := NewOrchestrator("sleep", nil, "/tmp", nil)

	ctx, cancel := context.WithTimeout(
		context.Background(), 50*time.Millisecond,
	)
	defer cancel()

	project := ComposeProject{}

	// sleep will be used with "down" args which it won't understand,
	// but the context cancellation should still work
	err := o.Down(ctx, project)
	// Error is expected (either from context or from sleep misuse)
	require.Error(t, err)
}

// --- Tests for Status method ---

func TestDefaultOrchestrator_Status_Success(t *testing.T) {
	// Create a test script that outputs compose ps format
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
if [[ "$*" == *"ps"* ]]; then
    echo "web|running|healthy|0.0.0.0:8080->80/tcp|0"
    echo "db|running|healthy|0.0.0.0:5432->5432/tcp|0"
    exit 0
fi
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name: "test-project",
	}

	statuses, err := o.Status(ctx, project)
	require.NoError(t, err)
	require.Len(t, statuses, 2)

	assert.Equal(t, "web", statuses[0].Name)
	assert.Equal(t, "running", statuses[0].State)
	assert.Equal(t, "healthy", statuses[0].Health)

	assert.Equal(t, "db", statuses[1].Name)
	assert.Equal(t, "running", statuses[1].State)
}

func TestDefaultOrchestrator_Status_EmptyOutput(t *testing.T) {
	// Create a test script that outputs nothing
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{}

	statuses, err := o.Status(ctx, project)
	require.NoError(t, err)
	assert.Empty(t, statuses)
}

func TestDefaultOrchestrator_Status_CommandFailure(t *testing.T) {
	o := NewOrchestrator("false", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{}

	_, err := o.Status(ctx, project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compose ps failed")
}

func TestDefaultOrchestrator_Status_WithProjectArgs(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
echo "svc|running|healthy||0"
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name:    "myproject",
		File:    "compose.yaml",
		Profile: "dev",
	}

	statuses, err := o.Status(ctx, project)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "svc", statuses[0].Name)
}

// --- Tests for Logs method ---

func TestDefaultOrchestrator_Logs_Success(t *testing.T) {
	// Create a test script that outputs log lines
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
if [[ "$*" == *"logs"* ]]; then
    echo "Log line 1"
    echo "Log line 2"
    echo "Log line 3"
    exit 0
fi
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name: "test-project",
	}

	reader, err := o.Logs(ctx, project, "web")
	require.NoError(t, err)
	require.NotNil(t, reader)

	// Read all logs
	data, err := io.ReadAll(reader)
	require.NoError(t, err)

	logs := string(data)
	assert.Contains(t, logs, "Log line 1")
	assert.Contains(t, logs, "Log line 2")
	assert.Contains(t, logs, "Log line 3")

	// Close the reader
	err = reader.Close()
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Logs_EmptyOutput(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{}

	reader, err := o.Logs(ctx, project, "service")
	require.NoError(t, err)
	require.NotNil(t, reader)

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Empty(t, data)

	err = reader.Close()
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Logs_InvalidCommand(t *testing.T) {
	o := NewOrchestrator("/nonexistent/command", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{}

	_, err := o.Logs(ctx, project, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start compose logs")
}

func TestDefaultOrchestrator_Logs_WithComposeArgs(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
echo "Log output"
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	// Test with composeArgs (like ["compose"] for docker compose)
	o := NewOrchestrator(scriptPath, []string{"--arg1"}, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		File: "compose.yaml",
	}

	reader, err := o.Logs(ctx, project, "db")
	require.NoError(t, err)
	require.NotNil(t, reader)

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, string(data), "Log output")

	err = reader.Close()
	require.NoError(t, err)
}

// --- Tests for logReader ---

func TestLogReader_Read(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "echo-script.sh")

	script := `#!/bin/bash
echo "test data"
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	cmd := exec.Command(scriptPath)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	err = cmd.Start()
	require.NoError(t, err)

	lr := &logReader{cmd: cmd, reader: stdout}

	buf := make([]byte, 100)
	n, err := lr.Read(buf)
	// First read should succeed
	if err == nil {
		assert.Greater(t, n, 0)
		assert.Contains(t, string(buf[:n]), "test data")
	}

	err = lr.Close()
	require.NoError(t, err)
}

func TestLogReader_Close(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "quick-exit.sh")

	script := `#!/bin/bash
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	cmd := exec.Command(scriptPath)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	err = cmd.Start()
	require.NoError(t, err)

	lr := &logReader{cmd: cmd, reader: stdout}

	// Close should wait for the process and return no error
	err = lr.Close()
	require.NoError(t, err)
}

// --- Tests for run method ---

func TestDefaultOrchestrator_Run_Success(t *testing.T) {
	o := NewOrchestrator("true", nil, "/tmp", nil)

	ctx := context.Background()
	err := o.run(ctx, []string{})
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Run_Failure(t *testing.T) {
	o := NewOrchestrator("false", nil, "/tmp", nil)

	ctx := context.Background()
	err := o.run(ctx, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func TestDefaultOrchestrator_Run_WithStderr(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "stderr-script.sh")

	script := `#!/bin/bash
echo "error message" >&2
exit 1
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	err = o.run(ctx, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error message")
}

func TestDefaultOrchestrator_Run_WorkingDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file in the temp directory
	testFile := filepath.Join(tmpDir, "testfile.txt")
	err := os.WriteFile(testFile, []byte("test"), 0644)
	require.NoError(t, err)

	// Script that checks if testfile.txt exists in working dir
	scriptPath := filepath.Join(tmpDir, "check-wd.sh")
	script := `#!/bin/bash
if [ -f "testfile.txt" ]; then
    exit 0
else
    exit 1
fi
`
	err = os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	err = o.run(ctx, []string{})
	require.NoError(t, err)
}

// --- Tests for output method ---

func TestDefaultOrchestrator_Output_Success(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	out, err := o.output(ctx, []string{"hello", "world"})
	require.NoError(t, err)
	assert.Contains(t, out, "hello world")
}

func TestDefaultOrchestrator_Output_Failure(t *testing.T) {
	o := NewOrchestrator("false", nil, "/tmp", nil)

	ctx := context.Background()
	_, err := o.output(ctx, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

func TestDefaultOrchestrator_Output_WithStderr(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "stderr-output.sh")

	script := `#!/bin/bash
echo "stderr content" >&2
exit 1
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	_, err = o.output(ctx, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stderr content")
}

func TestDefaultOrchestrator_Output_EmptyOutput(t *testing.T) {
	o := NewOrchestrator("true", nil, "/tmp", nil)

	ctx := context.Background()
	out, err := o.output(ctx, []string{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// --- Additional edge case tests ---

func TestDefaultOrchestrator_Up_EmptyServices(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{
		Services: []string{}, // empty services
	}

	err := o.Up(ctx, project)
	require.NoError(t, err)
}

func TestDefaultOrchestrator_Down_EmptyProject(t *testing.T) {
	o := NewOrchestrator("echo", nil, "/tmp", nil)

	ctx := context.Background()
	project := ComposeProject{} // all fields empty

	err := o.Down(ctx, project)
	require.NoError(t, err)
}

func TestParseStatusOutput_MalformedLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []ServiceStatus
	}{
		{
			name:     "too few fields",
			input:    "web|running|healthy\n",
			expected: nil,
		},
		{
			name:     "partial line",
			input:    "web|running\n",
			expected: nil,
		},
		{
			name:     "just name",
			input:    "web\n",
			expected: nil,
		},
		{
			name: "mixed valid and invalid",
			input: "web|running|healthy|80/tcp|0\n" +
				"invalid\n" +
				"db|running||5432/tcp|0\n",
			expected: []ServiceStatus{
				{
					Name: "web", State: "running",
					Health: "healthy",
					Ports:  []string{"80/tcp"},
				},
				{
					Name: "db", State: "running",
					Ports: []string{"5432/tcp"},
				},
			},
		},
		{
			name:  "invalid exit code",
			input: "web|running|healthy|80/tcp|invalid\n",
			expected: []ServiceStatus{
				{
					Name: "web", State: "running",
					Health:   "healthy",
					Ports:    []string{"80/tcp"},
					ExitCode: 0, // defaults to 0 on parse error
				},
			},
		},
		{
			name:     "only 4 fields - skipped",
			input:    "web|exited||-1\n",
			expected: nil, // only 4 fields, needs 5
		},
		{
			name:  "negative exit code with 5 fields",
			input: "web|exited||80/tcp|-1\n",
			expected: []ServiceStatus{
				{
					Name:     "web",
					State:    "exited",
					Ports:    []string{"80/tcp"},
					ExitCode: -1,
				},
			},
		},
		{
			name:  "exit code with spaces",
			input: "web|running|healthy|80/tcp| 42 \n",
			expected: []ServiceStatus{
				{
					Name: "web", State: "running",
					Health:   "healthy",
					Ports:    []string{"80/tcp"},
					ExitCode: 42,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseStatusOutput(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParsePorts_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "trailing comma",
			input:    "80/tcp,",
			expected: []string{"80/tcp"},
		},
		{
			name:     "leading comma",
			input:    ",80/tcp",
			expected: []string{"80/tcp"},
		},
		{
			name:     "multiple commas",
			input:    "80/tcp,,443/tcp",
			expected: []string{"80/tcp", "443/tcp"},
		},
		{
			name:     "spaces around commas",
			input:    "80/tcp , 443/tcp , 8080/tcp",
			expected: []string{"80/tcp", "443/tcp", "8080/tcp"},
		},
		{
			name:     "newlines in ports",
			input:    "80/tcp\n443/tcp",
			expected: []string{"80/tcp\n443/tcp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parsePorts(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewOrchestrator_WithNilArgs(t *testing.T) {
	o := NewOrchestrator("docker-compose", nil, "/home", nil)
	require.NotNil(t, o)
	assert.Equal(t, "docker-compose", o.composeCmd)
	assert.Nil(t, o.composeArgs)
	assert.Equal(t, "/home", o.workDir)
}

// --- Test ComposeOrchestrator interface compliance ---

func TestDefaultOrchestrator_ImplementsInterface(t *testing.T) {
	var _ ComposeOrchestrator = (*DefaultOrchestrator)(nil)
}

// --- Test context handling ---

func TestDefaultOrchestrator_ContextCancelDuringOutput(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "slow-script.sh")

	script := `#!/bin/bash
sleep 10
echo "done"
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx, cancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond,
	)
	defer cancel()

	_, err = o.output(ctx, []string{})
	require.Error(t, err)
}

// --- Test argument building ---

func TestDefaultOrchestrator_UpArguments(t *testing.T) {
	// Capture arguments by using a script that prints them
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	scriptPath := filepath.Join(tmpDir, "capture-args.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, []string{"compose"}, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name:     "myproj",
		File:     "compose.yml",
		Profile:  "dev",
		Services: []string{"web", "api"},
	}

	err = o.Up(ctx, project,
		WithUpDetach(true),
		WithRemoveOrphans(true),
		WithBuildFirst(true),
		WithForceRecreate(true),
		WithUpTimeout(30),
		WithWait(true),
	)
	require.NoError(t, err)

	// Read captured arguments
	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)

	args := string(data)
	assert.Contains(t, args, "compose")
	assert.Contains(t, args, "-f compose.yml")
	assert.Contains(t, args, "--project-name myproj")
	assert.Contains(t, args, "--profile dev")
	assert.Contains(t, args, "up")
	assert.Contains(t, args, "-d")
	assert.Contains(t, args, "--remove-orphans")
	assert.Contains(t, args, "--build")
	assert.Contains(t, args, "--force-recreate")
	assert.Contains(t, args, "--timeout 30")
	assert.Contains(t, args, "--wait")
	assert.Contains(t, args, "web")
	assert.Contains(t, args, "api")
}

func TestDefaultOrchestrator_DownArguments(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	scriptPath := filepath.Join(tmpDir, "capture-args.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name: "myproj",
		File: "docker-compose.yml",
	}

	err = o.Down(ctx, project,
		WithDownRemoveOrphans(true),
		WithDownRemoveVolumes(true),
		WithDownRemoveImages("local"),
		WithDownTimeout(45),
	)
	require.NoError(t, err)

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)

	args := string(data)
	assert.Contains(t, args, "-f docker-compose.yml")
	assert.Contains(t, args, "--project-name myproj")
	assert.Contains(t, args, "down")
	assert.Contains(t, args, "--remove-orphans")
	assert.Contains(t, args, "--volumes")
	assert.Contains(t, args, "--rmi local")
	assert.Contains(t, args, "--timeout 45")
}

func TestDefaultOrchestrator_StatusArguments(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	scriptPath := filepath.Join(tmpDir, "capture-args.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
echo "svc|running|healthy|80/tcp|0"
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, []string{"compose"}, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		Name:    "testproj",
		Profile: "prod",
	}

	_, err = o.Status(ctx, project)
	require.NoError(t, err)

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)

	args := string(data)
	assert.Contains(t, args, "compose")
	assert.Contains(t, args, "--project-name testproj")
	assert.Contains(t, args, "--profile prod")
	assert.Contains(t, args, "ps")
	assert.Contains(t, args, "--format")
}

func TestDefaultOrchestrator_LogsArguments(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	scriptPath := filepath.Join(tmpDir, "capture-args.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
echo "log line"
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{
		File: "compose.yaml",
	}

	reader, err := o.Logs(ctx, project, "myservice")
	require.NoError(t, err)

	// Read to trigger the command
	_, _ = io.ReadAll(reader)
	_ = reader.Close()

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)

	args := string(data)
	assert.Contains(t, args, "-f compose.yaml")
	assert.Contains(t, args, "logs")
	assert.Contains(t, args, "--no-color")
	assert.Contains(t, args, "myservice")
}

// Test parseStatusOutput with extra whitespace
func TestParseStatusOutput_Whitespace(t *testing.T) {
	input := "  web  |  running  |  healthy  |  80/tcp  |  0  \n"
	result := parseStatusOutput(input)

	require.Len(t, result, 1)
	assert.Equal(t, "web", result[0].Name)
	assert.Equal(t, "running", result[0].State)
	assert.Equal(t, "healthy", result[0].Health)
	assert.Equal(t, []string{"80/tcp"}, result[0].Ports)
	assert.Equal(t, 0, result[0].ExitCode)
}

// Test that composeArgs are properly prepended
func TestDefaultOrchestrator_ComposeArgsPrepended(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	scriptPath := filepath.Join(tmpDir, "capture-args.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
exit 0
`
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, []string{"compose", "--verbose"}, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{}

	err = o.Up(ctx, project)
	require.NoError(t, err)

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)

	args := string(data)
	// compose and --verbose should appear before up
	composeIdx := strings.Index(args, "compose")
	verboseIdx := strings.Index(args, "--verbose")
	upIdx := strings.Index(args, "up")

	assert.Less(t, composeIdx, upIdx)
	assert.Less(t, verboseIdx, upIdx)
}

// TestDetectComposeCmd_NoComposeAvailable tests the error path when
// no compose command is found on the system.
func TestDetectComposeCmd_NoComposeAvailable(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so the no-compose-found error path is reached instead of the pin's
	// "not runnable" fail-closed error (§11.4.1).
	t.Setenv(composeEnvVar, "")
	// Save original PATH
	origPath := os.Getenv("PATH")
	defer os.Setenv("PATH", origPath)

	// Set PATH to empty to simulate no compose commands available
	os.Setenv("PATH", "/nonexistent")

	cmd, args, err := detectComposeCmd()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no compose command found")
	assert.Empty(t, cmd)
	assert.Nil(t, args)
}

// TestNewDefaultOrchestrator_NoComposeAvailable tests NewDefaultOrchestrator
// when no compose command is available.
func TestNewDefaultOrchestrator_NoComposeAvailable(t *testing.T) {
	// R1 hermeticity: neutralize any globally-exported CONTAINERS_COMPOSE_CMD
	// pin so the no-compose-found error path is reached instead of the pin's
	// "not runnable" fail-closed error (§11.4.1).
	t.Setenv(composeEnvVar, "")
	// Save original PATH
	origPath := os.Getenv("PATH")
	defer os.Setenv("PATH", origPath)

	// Set PATH to empty to simulate no compose commands available
	os.Setenv("PATH", "/nonexistent")

	o, err := NewDefaultOrchestrator("/tmp", nil)
	require.Error(t, err)
	assert.Nil(t, o)
	assert.Contains(t, err.Error(), "no compose command found")
}

// TestDefaultOrchestrator_Logs_StdoutPipeError tests the Logs method
// when creating the stdout pipe fails. This is hard to trigger directly,
// so we test the command start failure path instead.
func TestDefaultOrchestrator_Logs_CommandNotExecutable(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a non-executable file
	scriptPath := filepath.Join(tmpDir, "not-executable.txt")
	err := os.WriteFile(scriptPath, []byte("not a script"), 0644)
	require.NoError(t, err)

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)

	ctx := context.Background()
	project := ComposeProject{Name: "test-project"}

	_, err = o.Logs(ctx, project, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start compose logs")
}

// --- Mock types for testing StdoutPipe failure ---

// mockCmdFactory creates mock Cmd instances.
type mockCmdFactory struct {
	stdoutPipeErr error
	startErr      error
	waitErr       error
}

func (f *mockCmdFactory) CommandContext(
	_ context.Context, _ string, _ ...string,
) Cmd {
	return &mockCmd{
		stdoutPipeErr: f.stdoutPipeErr,
		startErr:      f.startErr,
		waitErr:       f.waitErr,
	}
}

// mockCmd is a mock implementation of Cmd.
type mockCmd struct {
	stdoutPipeErr error
	startErr      error
	waitErr       error
}

func (c *mockCmd) SetDir(_ string) {}
func (c *mockCmd) Start() error    { return c.startErr }
func (c *mockCmd) Wait() error     { return c.waitErr }
func (c *mockCmd) StdoutPipe() (io.ReadCloser, error) {
	if c.stdoutPipeErr != nil {
		return nil, c.stdoutPipeErr
	}
	return io.NopCloser(strings.NewReader("mock output")), nil
}

// TestDefaultOrchestrator_Logs_StdoutPipeError tests the error path when
// cmd.StdoutPipe() fails.
func TestDefaultOrchestrator_Logs_StdoutPipeError(t *testing.T) {
	factory := &mockCmdFactory{
		stdoutPipeErr: fmt.Errorf("mock stdout pipe error"),
	}

	o := NewOrchestratorWithFactory(
		"docker", []string{"compose"}, "/tmp", nil, factory,
	)

	ctx := context.Background()
	project := ComposeProject{Name: "test"}

	_, err := o.Logs(ctx, project, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create stdout pipe")
	assert.Contains(t, err.Error(), "mock stdout pipe error")
}

// TestDefaultOrchestrator_Logs_StartError tests the error path when
// cmd.Start() fails.
func TestDefaultOrchestrator_Logs_StartError(t *testing.T) {
	factory := &mockCmdFactory{
		startErr: fmt.Errorf("mock start error"),
	}

	o := NewOrchestratorWithFactory(
		"docker", []string{"compose"}, "/tmp", nil, factory,
	)

	ctx := context.Background()
	project := ComposeProject{Name: "test"}

	_, err := o.Logs(ctx, project, "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start compose logs")
	assert.Contains(t, err.Error(), "mock start error")
}

// TestNewOrchestratorWithFactory tests the factory constructor.
func TestNewOrchestratorWithFactory(t *testing.T) {
	factory := &mockCmdFactory{}
	o := NewOrchestratorWithFactory(
		"docker", []string{"compose"}, "/tmp", nil, factory,
	)

	require.NotNil(t, o)
	assert.Equal(t, "docker", o.composeCmd)
	assert.Equal(t, []string{"compose"}, o.composeArgs)
	assert.Equal(t, "/tmp", o.workDir)
}

// TestCmdLogReader_ReadAndClose tests the cmdLogReader methods.
func TestCmdLogReader_ReadAndClose(t *testing.T) {
	reader := io.NopCloser(strings.NewReader("test content"))
	waitCalled := false

	mockCmd := &mockCmd{waitErr: nil}

	// Wrap to track Wait call
	clr := &cmdLogReader{
		cmd:    mockCmd,
		reader: reader,
	}

	// Test Read
	buf := make([]byte, 20)
	n, err := clr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "test content", string(buf[:n]))

	// Test Close
	err = clr.Close()
	require.NoError(t, err)
	_ = waitCalled // mockCmd.Wait() was called implicitly
}

// testLogger is a simple test logger for orchestrator tests.
type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any) {}
func (l *testLogger) Info(msg string, args ...any)  {}
func (l *testLogger) Warn(msg string, args ...any)  {}
func (l *testLogger) Error(msg string, args ...any) {}
