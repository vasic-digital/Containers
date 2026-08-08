package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// mockHostManager implements remote.HostManager for tests.
type mockHostManager struct {
	hosts     map[string]remote.RemoteHost
	snapshots map[string]*remote.HostResources
}

func newMockHostManager() *mockHostManager {
	return &mockHostManager{
		hosts:     make(map[string]remote.RemoteHost),
		snapshots: make(map[string]*remote.HostResources),
	}
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
	hosts := make([]remote.RemoteHost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

func (m *mockHostManager) ProbeHost(
	ctx context.Context, name string,
) (*remote.HostResources, error) {
	return m.snapshots[name], nil
}

func (m *mockHostManager) ProbeAll(
	ctx context.Context,
) map[string]*remote.HostResources {
	return m.snapshots
}

func (m *mockHostManager) HostState(
	name string,
) remote.HostState {
	return remote.HostOnline
}

func makeSnapshot(
	host string, cpuPct, memPct float64,
	memTotalMB, diskTotalMB uint64, cores int,
) *remote.HostResources {
	return &remote.HostResources{
		Host:          host,
		Timestamp:     time.Now(),
		CPUPercent:    cpuPct,
		MemoryPercent: memPct,
		MemoryTotalMB: memTotalMB,
		MemoryUsedMB:  uint64(float64(memTotalMB) * memPct / 100),
		DiskPercent:   30,
		DiskTotalMB:   diskTotalMB,
		DiskUsedMB:    uint64(float64(diskTotalMB) * 30 / 100),
		CPUCores:      cores,
	}
}

func TestDefaultScheduler_Schedule_ResourceAware(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "gpu-1", Address: "10.0.0.1", User: "u",
		Labels: map[string]string{},
	})
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "gpu-2", Address: "10.0.0.2", User: "u",
		Labels: map[string]string{},
	})
	mgr.snapshots["local"] = makeSnapshot("local", 80, 70, 16384, 500000, 8)
	mgr.snapshots["gpu-1"] = makeSnapshot("gpu-1", 20, 30, 65536, 1000000, 32)
	mgr.snapshots["gpu-2"] = makeSnapshot("gpu-2", 50, 50, 32768, 500000, 16)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyResourceAware),
	)

	req := ContainerRequirements{
		Name:     "test-app",
		MemoryMB: 1024,
	}
	decision, err := sched.Schedule(context.Background(), req)
	require.NoError(t, err)
	assert.NotEmpty(t, decision.HostName)
	assert.Greater(t, decision.Score, 0.0)
	// gpu-1 should win: most available resources.
	assert.Equal(t, "gpu-1", decision.HostName)
}

func TestDefaultScheduler_Schedule_PreferLocal(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "remote-1", Address: "10.0.0.1", User: "u",
	})
	mgr.snapshots["local"] = makeSnapshot("local", 30, 30, 16384, 500000, 8)
	mgr.snapshots["remote-1"] = makeSnapshot("remote-1", 10, 10, 65536, 1000000, 32)

	sched := NewScheduler(mgr, logging.NopLogger{})

	req := ContainerRequirements{
		Name:        "local-app",
		PreferLocal: true,
	}
	decision, err := sched.Schedule(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "local", decision.HostName)
}

func TestDefaultScheduler_ScheduleBatch(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "host-1", Address: "10.0.0.1", User: "u",
	})
	mgr.snapshots["local"] = makeSnapshot("local", 40, 40, 16384, 500000, 8)
	mgr.snapshots["host-1"] = makeSnapshot("host-1", 20, 20, 32768, 1000000, 16)

	sched := NewScheduler(mgr, logging.NopLogger{})

	reqs := []ContainerRequirements{
		{Name: "app-1"},
		{Name: "app-2"},
		{Name: "app-3"},
	}
	plan, err := sched.ScheduleBatch(context.Background(), reqs)
	require.NoError(t, err)
	assert.Len(t, plan.Decisions, 3)
}

func TestDefaultScheduler_ScheduleBatch_Empty(t *testing.T) {
	mgr := newMockHostManager()
	sched := NewScheduler(mgr, logging.NopLogger{})

	plan, err := sched.ScheduleBatch(
		context.Background(), nil,
	)
	require.NoError(t, err)
	assert.Empty(t, plan.Decisions)
}

func TestDefaultScheduler_RoundRobin(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "h1", Address: "10.0.0.1", User: "u",
	})
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "h2", Address: "10.0.0.2", User: "u",
	})
	mgr.snapshots["local"] = makeSnapshot("local", 50, 50, 16384, 500000, 8)
	// Wave-20 CT-HARDEN-SCHED-2 reconciliation (§11.4.120): round_robin now
	// excludes remote hosts absent from ProbeAll's snapshots (probe failed =
	// offline). h1/h2 must be PROBED to be eligible rotation targets — the intent
	// of this test ("distribute across multiple hosts") is preserved; only the
	// fixture is corrected so the hosts it distributes to are actually reachable.
	mgr.snapshots["h1"] = makeSnapshot("h1", 50, 50, 16384, 500000, 8)
	mgr.snapshots["h2"] = makeSnapshot("h2", 50, 50, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyRoundRobin),
	)

	hosts := make(map[string]int)
	for i := 0; i < 6; i++ {
		d, err := sched.Schedule(context.Background(),
			ContainerRequirements{Name: "app"},
		)
		require.NoError(t, err)
		hosts[d.HostName]++
	}
	// Should distribute across available hosts.
	assert.Greater(t, len(hosts), 1)
}

func TestDefaultScheduler_Rebalance(t *testing.T) {
	mgr := newMockHostManager()
	mgr.snapshots["local"] = makeSnapshot("local", 90, 85, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{})
	plan, err := sched.Rebalance(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, plan)
}

func TestDefaultScheduler_Rebalance_NoSnapshots(t *testing.T) {
	mgr := newMockHostManager()
	sched := NewScheduler(mgr, logging.NopLogger{})

	_, err := sched.Rebalance(context.Background())
	assert.Error(t, err)
}

func TestPlacementDecision_IsLocal(t *testing.T) {
	assert.True(t, (&PlacementDecision{HostName: ""}).IsLocal())
	assert.True(t, (&PlacementDecision{HostName: "local"}).IsLocal())
	assert.False(t, (&PlacementDecision{HostName: "remote-1"}).IsLocal())
}

func TestPlacementPlan_ByHost(t *testing.T) {
	plan := &PlacementPlan{
		Decisions: []PlacementDecision{
			{HostName: "local"},
			{HostName: "host-1"},
			{HostName: "local"},
			{HostName: "host-2"},
		},
	}
	byHost := plan.ByHost()
	assert.Len(t, byHost["local"], 2)
	assert.Len(t, byHost["host-1"], 1)
	assert.Len(t, byHost["host-2"], 1)
}

func TestPlacementPlan_LocalRemote(t *testing.T) {
	plan := &PlacementPlan{
		Decisions: []PlacementDecision{
			{HostName: "local"},
			{HostName: "host-1"},
			{HostName: ""},
		},
	}
	assert.Len(t, plan.LocalDecisions(), 2)
	assert.Len(t, plan.RemoteDecisions(), 1)
}
