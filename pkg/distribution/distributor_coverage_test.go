package distribution

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/network"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// TestDefaultDistributor_Rebalance_NoScheduler verifies Rebalance
// fails when no scheduler is configured.
func TestDefaultDistributor_Rebalance_NoScheduler(t *testing.T) {
	dist := NewDistributor(WithLogger(logging.NopLogger{}))
	_, err := dist.Rebalance(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scheduler not configured")
}

// TestDefaultDistributor_Rebalance_WithContainers verifies Rebalance
// redistributes currently tracked containers.
func TestDefaultDistributor_Rebalance_WithContainers(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	// Pre-populate containers
	dist.containers = []DistributedContainer{
		{Requirement: scheduler.ContainerRequirements{Name: "app-1", Image: "nginx"}, HostName: "local", State: StateRunning},
		{Requirement: scheduler.ContainerRequirements{Name: "app-2", Image: "redis"}, HostName: "local", State: StateRunning},
	}

	summary, err := dist.Rebalance(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalContainers)
}

// TestDefaultDistributor_Rebalance_EmptyContainers verifies Rebalance
// with no tracked containers.
func TestDefaultDistributor_Rebalance_EmptyContainers(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	summary, err := dist.Rebalance(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, summary.TotalContainers)
}

// TestDefaultDistributor_DistributeEndpoints_Success verifies
// DistributeEndpoints converts names to requirements and distributes.
func TestDefaultDistributor_DistributeEndpoints_Success(t *testing.T) {
	// §11.4.120 RECONCILED for CT-HARDEN-DIST-HARD DIST-3: DistributeEndpoints
	// returns LocalContainers+RemoteContainers, so the count==3 assertion is a
	// bluff without a runtime — post-fix a nil LocalRuntime FAILS every local
	// deploy (count would be 0). Supply a real (fake) runtime seam so the 3
	// endpoints genuinely deploy locally and the count is honestly 3.
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLocalRuntime(&deployLocalRuntime{}),
		WithLogger(logging.NopLogger{}),
	)

	count, err := dist.DistributeEndpoints(
		context.Background(),
		[]string{"postgresql", "redis", "chromadb"},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

// TestDefaultDistributor_DistributeEndpoints_NoScheduler verifies
// DistributeEndpoints returns an error when no scheduler is configured.
func TestDefaultDistributor_DistributeEndpoints_NoScheduler(t *testing.T) {
	dist := NewDistributor(WithLogger(logging.NopLogger{}))

	_, err := dist.DistributeEndpoints(
		context.Background(),
		[]string{"service1"},
	)
	assert.Error(t, err)
}

// TestDefaultDistributor_DistributeEndpoints_Empty verifies
// DistributeEndpoints with an empty name list.
func TestDefaultDistributor_DistributeEndpoints_Empty(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	count, err := dist.DistributeEndpoints(context.Background(), []string{})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// TestDefaultDistributor_HealthCheckAll_RemoteUnreachable verifies
// that an unreachable remote host is reported as an error.
func TestDefaultDistributor_HealthCheckAll_RemoteUnreachable(t *testing.T) {
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u"},
		},
	}
	exec := &mockExecutor{
		reachableFunc: func(ctx context.Context, host remote.RemoteHost) bool {
			return false
		},
	}

	dist := NewDistributor(
		WithHostManager(hm),
		WithExecutor(exec),
		WithLogger(logging.NopLogger{}),
	)
	dist.containers = []DistributedContainer{
		{
			Requirement: scheduler.ContainerRequirements{Name: "app-1"},
			HostName:    "remote-1",
			State:       StateRunning,
		},
	}

	errors := dist.HealthCheckAll(context.Background())
	assert.Len(t, errors, 1)
	assert.Contains(t, errors["app-1"].Error(), "unreachable")
}

// TestDefaultDistributor_HealthCheckAll_RemoteHostNotFound verifies
// that a missing host is reported as an error.
func TestDefaultDistributor_HealthCheckAll_RemoteHostNotFound(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	exec := &mockExecutor{}

	dist := NewDistributor(
		WithHostManager(hm),
		WithExecutor(exec),
		WithLogger(logging.NopLogger{}),
	)
	dist.containers = []DistributedContainer{
		{
			Requirement: scheduler.ContainerRequirements{Name: "app-1"},
			HostName:    "missing-host",
			State:       StateRunning,
		},
	}

	errors := dist.HealthCheckAll(context.Background())
	assert.Len(t, errors, 1)
	assert.Contains(t, errors["app-1"].Error(), "not found")
}

// TestDefaultDistributor_HealthCheckAll_SkipsNonRunning verifies
// containers not in StateRunning are skipped.
func TestDefaultDistributor_HealthCheckAll_SkipsNonRunning(t *testing.T) {
	dist := NewDistributor(
		WithLogger(logging.NopLogger{}),
	)
	dist.containers = []DistributedContainer{
		{
			Requirement: scheduler.ContainerRequirements{Name: "app-1"},
			HostName:    "remote-1",
			State:       StateStopped,
		},
		{
			Requirement: scheduler.ContainerRequirements{Name: "app-2"},
			HostName:    "remote-2",
			State:       StateFailed,
		},
	}

	errors := dist.HealthCheckAll(context.Background())
	assert.Empty(t, errors)
}

// TestDefaultDistributor_Undistribute_WithTunnelManager verifies
// Undistribute calls TunnelManager.CloseAll.
func TestDefaultDistributor_Undistribute_WithTunnelManager(t *testing.T) {
	closeAllCalled := false
	tm := &mockTunnelManager{
		closeAllFunc: func() error {
			closeAllCalled = true
			return nil
		},
	}

	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithTunnelManager(tm),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{{Name: "app-1"}}
	_, _ = dist.Distribute(context.Background(), reqs)

	err := dist.Undistribute(context.Background())
	assert.NoError(t, err)
	assert.True(t, closeAllCalled)
}

// mockTunnelManager implements network.TunnelManager for testing.
type mockTunnelManager struct {
	closeAllFunc func() error
}

func (m *mockTunnelManager) CreateTunnel(
	ctx context.Context, hostName string, spec network.TunnelSpec,
) (*network.TunnelInfo, error) {
	return nil, nil
}

func (m *mockTunnelManager) CloseTunnel(localPort string) error {
	return nil
}

func (m *mockTunnelManager) ListTunnels() []network.TunnelInfo {
	return nil
}

func (m *mockTunnelManager) CloseAllForHost(hostName string) error {
	return nil
}

func (m *mockTunnelManager) CloseAll() error {
	if m.closeAllFunc != nil {
		return m.closeAllFunc()
	}
	return nil
}
