package distribution

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// mockScheduler for distribution tests.
type mockScheduler struct {
	batchFunc func(ctx context.Context, reqs []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error)
}

func (m *mockScheduler) Schedule(
	ctx context.Context, req scheduler.ContainerRequirements,
) (*scheduler.PlacementDecision, error) {
	return &scheduler.PlacementDecision{
		Requirement: req, HostName: "local", Score: 0.8,
	}, nil
}

func (m *mockScheduler) ScheduleBatch(
	ctx context.Context, reqs []scheduler.ContainerRequirements,
) (*scheduler.PlacementPlan, error) {
	if m.batchFunc != nil {
		return m.batchFunc(ctx, reqs)
	}
	decisions := make([]scheduler.PlacementDecision, len(reqs))
	for i, req := range reqs {
		decisions[i] = scheduler.PlacementDecision{
			Requirement: req,
			HostName:    "local",
			Score:       0.8,
			Reason:      "mock",
		}
	}
	return &scheduler.PlacementPlan{
		Decisions:     decisions,
		HostSnapshots: map[string]*remote.HostResources{},
	}, nil
}

func (m *mockScheduler) Rebalance(
	ctx context.Context,
) (*scheduler.PlacementPlan, error) {
	return &scheduler.PlacementPlan{}, nil
}

// mockExecutor for distribution tests.
type mockExecutor struct {
	executeFunc   func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
	reachableFunc func(ctx context.Context, host remote.RemoteHost) bool
}

func (m *mockExecutor) Execute(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (*remote.CommandResult, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, host, cmd)
	}
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (m *mockExecutor) ExecuteStream(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *mockExecutor) CopyFile(
	ctx context.Context, host remote.RemoteHost, l, r string,
) error {
	return nil
}

func (m *mockExecutor) CopyDir(
	ctx context.Context, host remote.RemoteHost, l, r string,
) error {
	return nil
}

func (m *mockExecutor) IsReachable(
	ctx context.Context, host remote.RemoteHost,
) bool {
	if m.reachableFunc != nil {
		return m.reachableFunc(ctx, host)
	}
	return true
}

// mockHostManager for distribution tests.
type mockHostManager struct {
	hosts map[string]remote.RemoteHost
}

func (m *mockHostManager) AddHost(h remote.RemoteHost) error {
	m.hosts[h.Name] = h
	return nil
}

func (m *mockHostManager) RemoveHost(name string) error {
	delete(m.hosts, name)
	return nil
}

func (m *mockHostManager) GetHost(
	name string,
) (*remote.RemoteHost, error) {
	h, ok := m.hosts[name]
	if !ok {
		return nil, nil
	}
	return &h, nil
}

func (m *mockHostManager) ListHosts() []remote.RemoteHost {
	hosts := make([]remote.RemoteHost, 0)
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

func (m *mockHostManager) ProbeHost(
	ctx context.Context, name string,
) (*remote.HostResources, error) {
	return &remote.HostResources{Host: name}, nil
}

func (m *mockHostManager) ProbeAll(
	ctx context.Context,
) map[string]*remote.HostResources {
	return nil
}

func (m *mockHostManager) HostState(
	name string,
) remote.HostState {
	return remote.HostOnline
}

func TestDefaultDistributor_Distribute_Local(t *testing.T) {
	// §11.4.120 RECONCILED for CT-HARDEN-DIST-HARD DIST-3. This test previously
	// asserted LocalContainers==2 / FailedContainers==0 with NO LocalRuntime —
	// enshrining the deployLocal false-success bluff (nil runtime `return nil`
	// counted as a deployed container though nothing ran). The fix makes a nil
	// LocalRuntime a deploy FAILURE, so the correct-behaviour version supplies a
	// real (fake) runtime seam: the 2 local placements genuinely reach Start()
	// and THEN count as deployed. Asserting Local==2/Failed==0 is now backed by
	// an actual runtime call, not a silent skip.
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLocalRuntime(&deployLocalRuntime{}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1", Image: "nginx:latest"},
		{Name: "app-2", Image: "redis:latest"},
	}

	summary, err := dist.Distribute(
		context.Background(), reqs,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalContainers)
	assert.Equal(t, 2, summary.LocalContainers)
	assert.Equal(t, 0, summary.RemoteContainers)
	assert.Equal(t, 0, summary.FailedContainers)
}

func TestDefaultDistributor_Distribute_Mixed(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{
			batchFunc: func(
				ctx context.Context,
				reqs []scheduler.ContainerRequirements,
			) (*scheduler.PlacementPlan, error) {
				decisions := make(
					[]scheduler.PlacementDecision, len(reqs),
				)
				for i, req := range reqs {
					host := "local"
					if i%2 == 1 {
						host = "remote-1"
					}
					decisions[i] = scheduler.PlacementDecision{
						Requirement: req,
						HostName:    host,
						Score:       0.7,
						Reason:      "test",
					}
				}
				return &scheduler.PlacementPlan{
					Decisions:     decisions,
					HostSnapshots: map[string]*remote.HostResources{},
				}, nil
			},
		}),
		WithExecutor(&mockExecutor{}),
		// §11.4.120 RECONCILED for DIST-3: the local half of this mixed batch
		// now needs a real (fake) runtime seam to count as deployed — a nil
		// LocalRuntime is a deploy FAILURE post-fix, not a silent success, so
		// without this the LocalContainers==1 assertion would be a bluff. The
		// remote half is unchanged.
		WithLocalRuntime(&deployLocalRuntime{}),
		WithHostManager(&mockHostManager{
			hosts: map[string]remote.RemoteHost{
				"remote-1": {
					Name: "remote-1", Address: "10.0.0.1",
					User: "u", Runtime: "docker",
				},
			},
		}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1", Image: "nginx"},
		{Name: "app-2", Image: "redis"},
	}

	summary, err := dist.Distribute(
		context.Background(), reqs,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalContainers)
	assert.Equal(t, 1, summary.LocalContainers)
	assert.Equal(t, 1, summary.RemoteContainers)
}

func TestDefaultDistributor_Distribute_NoScheduler(t *testing.T) {
	dist := NewDistributor(
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(
		context.Background(),
		[]scheduler.ContainerRequirements{{Name: "app"}},
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scheduler not configured")
}

func TestDefaultDistributor_Distribute_FailedPlacement(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{
			batchFunc: func(
				ctx context.Context,
				reqs []scheduler.ContainerRequirements,
			) (*scheduler.PlacementPlan, error) {
				return &scheduler.PlacementPlan{
					Decisions: []scheduler.PlacementDecision{
						{
							Requirement: reqs[0],
							Score:       0,
							Reason:      "no resources",
						},
					},
					HostSnapshots: map[string]*remote.HostResources{},
				}, nil
			},
		}),
		WithLogger(logging.NopLogger{}),
	)

	summary, err := dist.Distribute(
		context.Background(),
		[]scheduler.ContainerRequirements{{Name: "app"}},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.FailedContainers)
}

func TestDefaultDistributor_Undistribute(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1"},
	}
	_, _ = dist.Distribute(context.Background(), reqs)

	err := dist.Undistribute(context.Background())
	assert.NoError(t, err)

	status := dist.Status(context.Background())
	assert.Empty(t, status)
}

func TestDefaultDistributor_Status(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1"},
		{Name: "app-2"},
	}
	_, _ = dist.Distribute(context.Background(), reqs)

	status := dist.Status(context.Background())
	assert.Len(t, status, 2)
}

func TestDefaultDistributor_HealthCheckAll(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{Name: "app-1"},
	}
	_, _ = dist.Distribute(context.Background(), reqs)

	errors := dist.HealthCheckAll(context.Background())
	assert.Empty(t, errors)
}

func TestDefaultDistributor_HostStatus(t *testing.T) {
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"h1": {Name: "h1", Address: "10.0.0.1", User: "u"},
		},
	}

	dist := NewDistributor(
		WithHostManager(hm),
		WithLogger(logging.NopLogger{}),
	)

	res, err := dist.HostStatus(context.Background(), "h1")
	require.NoError(t, err)
	assert.Equal(t, "h1", res.Host)
}

func TestDefaultDistributor_HostStatus_NoManager(t *testing.T) {
	dist := NewDistributor(
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.HostStatus(context.Background(), "h1")
	assert.Error(t, err)
}

func TestDistributionSummary_Fields(t *testing.T) {
	s := &DistributionSummary{
		TotalContainers:  5,
		LocalContainers:  3,
		RemoteContainers: 2,
		FailedContainers: 0,
	}
	assert.Equal(t, 5, s.TotalContainers)
	assert.Equal(t, 3, s.LocalContainers)
}

func TestDistributedContainer_Fields(t *testing.T) {
	dc := DistributedContainer{
		HostName: "gpu-1",
		State:    StateRunning,
	}
	assert.Equal(t, "gpu-1", dc.HostName)
	assert.Equal(t, StateRunning, dc.State)
}
