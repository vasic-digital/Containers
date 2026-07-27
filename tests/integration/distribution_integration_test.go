//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/distribution"
	"digital.vasic.containers/pkg/envconfig"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDistributionWorkflow_AllPhases verifies that all 7
// WorkflowPhases are defined and returned by AllPhases.
func TestDistributionWorkflow_AllPhases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode") // SKIP-OK: #short-mode
	}

	phases := distribution.AllPhases()
	require.Len(t, phases, 7,
		"expected exactly 7 workflow phases")

	expected := []distribution.WorkflowPhase{
		distribution.PhaseProbe,
		distribution.PhaseSchedule,
		distribution.PhaseVolumes,
		distribution.PhaseDeploy,
		distribution.PhaseTunnels,
		distribution.PhaseHealth,
		distribution.PhaseEvents,
	}

	for i, phase := range expected {
		assert.Equal(t, phase, phases[i],
			"phase at index %d should match", i)
	}

	// Verify each phase has a non-empty description and that the
	// NoopTranslator fallback (translator=nil) returns the namespaced
	// message ID, not the unknown-phase sentinel. The verbatim ID is
	// positive runtime evidence per CONST-035 / §11.9 — the assertion
	// here proves the i18n pipeline is wired through the integration
	// layer, not just the unit-test sandbox.
	for _, phase := range phases {
		desc := distribution.PhaseDescription(context.Background(), nil, phase)
		assert.NotEmpty(t, desc,
			"phase %q should have a description", phase)
		assert.NotEqual(t, "containers_workflow_phase_unknown", desc,
			"phase %q should not resolve to the unknown msgID", phase)
	}
}

// TestDistributor_WithMockHosts creates a Distributor with
// minimal dependencies and verifies that Distribute returns an
// error when no scheduler is configured.
func TestDistributor_WithMockHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode") // SKIP-OK: #short-mode
	}

	// Create a distributor without a scheduler to test the
	// error path for empty requirements.
	dist := distribution.NewDistributor(
		distribution.WithLogger(logging.NopLogger{}),
	)
	require.NotNil(t, dist)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	// Distribute with empty requirements should fail because
	// no scheduler is configured.
	_, err := dist.Distribute(
		ctx, []scheduler.ContainerRequirements{},
	)
	require.Error(t, err,
		"distribute without scheduler should return error")
	assert.Contains(t, err.Error(), "scheduler",
		"error should mention missing scheduler")

	// Status should return empty when nothing is distributed.
	status := dist.Status(ctx)
	assert.Empty(t, status,
		"status should be empty before any distribution")
}

// TestEnvConfig_LoadFromEnv sets CONTAINERS_REMOTE_ENABLED=true
// and verifies that LoadFromEnv reads the value correctly.
func TestEnvConfig_LoadFromEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode") // SKIP-OK: #short-mode
	}

	// Save and restore environment.
	orig := os.Getenv("CONTAINERS_REMOTE_ENABLED")
	t.Cleanup(func() {
		if orig == "" {
			os.Unsetenv("CONTAINERS_REMOTE_ENABLED")
		} else {
			os.Setenv("CONTAINERS_REMOTE_ENABLED", orig)
		}
	})

	os.Setenv("CONTAINERS_REMOTE_ENABLED", "true")

	cfg := envconfig.LoadFromEnv()
	require.NotNil(t, cfg)
	assert.True(t, cfg.Enabled,
		"config should be enabled when env var is true")

	// Verify defaults are populated.
	assert.Equal(t, "docker", cfg.DefaultRuntime,
		"default runtime should be docker")
	assert.Equal(t, "resource_aware", cfg.Scheduler,
		"default scheduler should be resource_aware")
	assert.Equal(t, 20000, cfg.PortRangeStart,
		"default port range start")
	assert.Equal(t, 30000, cfg.PortRangeEnd,
		"default port range end")
	assert.Equal(t, "sshfs", cfg.VolumeType,
		"default volume type should be sshfs")
}

// TestEnvConfig_GenerateExample verifies that GenerateEnvExample
// produces non-empty output containing expected configuration
// keys.
func TestEnvConfig_GenerateExample(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode") // SKIP-OK: #short-mode
	}

	example := envconfig.GenerateEnvExample()
	require.NotEmpty(t, example,
		"generated env example should not be empty")

	expectedKeys := []string{
		"CONTAINERS_REMOTE_ENABLED",
		"CONTAINERS_REMOTE_DEFAULT_SSH_USER",
		"CONTAINERS_REMOTE_DEFAULT_SSH_KEY",
		"CONTAINERS_REMOTE_DEFAULT_RUNTIME",
		"CONTAINERS_REMOTE_SCHEDULER",
		"CONTAINERS_REMOTE_PORT_RANGE_START",
		"CONTAINERS_REMOTE_PORT_RANGE_END",
		"CONTAINERS_REMOTE_VOLUME_TYPE",
		"CONTAINERS_REMOTE_HOST_1_NAME",
		"CONTAINERS_REMOTE_HOST_1_ADDRESS",
	}

	for _, key := range expectedKeys {
		assert.True(t, strings.Contains(example, key),
			"env example should contain key %q", key)
	}
}

// TestScheduler_ScheduleBatch_Empty verifies that scheduling an
// empty batch of requirements returns an empty placement plan.
func TestScheduler_ScheduleBatch_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode") // SKIP-OK: #short-mode
	}

	// Create a host manager with no hosts to keep the test
	// self-contained.
	executor, err := remote.NewSSHExecutor(
		logging.NopLogger{},
		remote.WithControlMaster(false),
	)
	require.NoError(t, err)
	defer executor.Close()

	hm := remote.NewHostManager(executor, logging.NopLogger{})
	require.NotNil(t, hm)

	sched := scheduler.NewScheduler(
		hm, logging.NopLogger{},
	)
	require.NotNil(t, sched)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	plan, err := sched.ScheduleBatch(
		ctx, []scheduler.ContainerRequirements{},
	)
	require.NoError(t, err,
		"scheduling empty batch should not error")
	require.NotNil(t, plan,
		"plan should not be nil for empty batch")
	assert.Empty(t, plan.Decisions,
		"decisions should be empty for empty batch")
}
