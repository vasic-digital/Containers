//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteHost_SkipWithoutConfig skips when
// CONTAINERS_REMOTE_ENABLED is not set to "true", verifying the
// skip logic itself.
func TestRemoteHost_SkipWithoutConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode") // SKIP-OK: #short-mode
	}

	val := os.Getenv("CONTAINERS_REMOTE_ENABLED")
	if val != "true" {
		t.Skip( // SKIP-OK: #env-remote-disabled
			"skipping remote host test: " +
				"CONTAINERS_REMOTE_ENABLED is not true",
		)
	}

	// If we reach here, CONTAINERS_REMOTE_ENABLED is true.
	assert.Equal(t, "true", val,
		"CONTAINERS_REMOTE_ENABLED should be true to run "+
			"remote tests")
}

// TestLocalRuntime_Detection detects the local container runtime
// and verifies that Name() returns a recognized value.
func TestLocalRuntime_Detection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode") // SKIP-OK: #short-mode
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 15*time.Second,
	)
	defer cancel()

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Skipf( // SKIP-OK: #env-runtime-missing
			"skipping: no container runtime available: %v",
			err,
		)
	}

	require.NotNil(t, rt, "detected runtime should not be nil")

	name := rt.Name()
	validNames := map[string]bool{
		"docker":     true,
		"podman":     true,
		"kubernetes": true,
	}
	assert.True(t, validNames[name],
		"runtime name %q should be docker, podman, or "+
			"kubernetes", name)

	// Verify the runtime reports as available.
	assert.True(t, rt.IsAvailable(ctx),
		"detected runtime %q should be available", name)
}

// TestLocalCompose_StatusOnMissing tries to get compose status
// on a non-existent compose file and expects an error.
func TestLocalCompose_StatusOnMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode") // SKIP-OK: #short-mode
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), 15*time.Second,
	)
	defer cancel()

	orch, err := compose.NewDefaultOrchestrator(
		t.TempDir(), logging.NopLogger{},
	)
	if err != nil {
		t.Skipf( // SKIP-OK: #env-compose-cmd-missing
			"skipping: no compose command available: %v",
			err,
		)
	}
	require.NotNil(t, orch)

	project := compose.ComposeProject{
		Name: "nonexistent-test-project",
		File: "/tmp/nonexistent-compose-file-12345.yml",
	}

	_, statusErr := orch.Status(ctx, project)
	require.Error(t, statusErr,
		"status on non-existent compose file should fail")
}
