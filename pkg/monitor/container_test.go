package monitor_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"digital.vasic.containers/pkg/monitor"
	"digital.vasic.containers/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRuntime is a configurable mock runtime for container collector tests.
type mockRuntime struct {
	name       string
	version    string
	versionErr error
	available  bool
	containers []runtime.ContainerInfo
	listErr    error
	stats      map[string]*runtime.ContainerStats
	statsErr   map[string]error
	status     map[string]*runtime.ContainerStatus
	statusErr  map[string]error
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		name:      "mock",
		version:   "1.0.0",
		available: true,
		stats:     make(map[string]*runtime.ContainerStats),
		statsErr:  make(map[string]error),
		status:    make(map[string]*runtime.ContainerStatus),
		statusErr: make(map[string]error),
	}
}

func (m *mockRuntime) Name() string { return m.name }

func (m *mockRuntime) Version(_ context.Context) (string, error) {
	return m.version, m.versionErr
}

func (m *mockRuntime) IsAvailable(_ context.Context) bool {
	return m.available
}

func (m *mockRuntime) Start(
	_ context.Context, _ string, _ ...runtime.StartOption,
) error {
	return nil
}

func (m *mockRuntime) Stop(
	_ context.Context, _ string, _ ...runtime.StopOption,
) error {
	return nil
}

func (m *mockRuntime) Remove(
	_ context.Context, _ string, _ ...runtime.RemoveOption,
) error {
	return nil
}

func (m *mockRuntime) Status(
	_ context.Context, id string,
) (*runtime.ContainerStatus, error) {
	if err, ok := m.statusErr[id]; ok && err != nil {
		return nil, err
	}
	if st, ok := m.status[id]; ok {
		return st, nil
	}
	return &runtime.ContainerStatus{Name: id}, nil
}

func (m *mockRuntime) List(
	_ context.Context, _ runtime.ListFilter,
) ([]runtime.ContainerInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.containers, nil
}

func (m *mockRuntime) Stats(
	_ context.Context, id string,
) (*runtime.ContainerStats, error) {
	if err, ok := m.statsErr[id]; ok && err != nil {
		return nil, err
	}
	if st, ok := m.stats[id]; ok {
		return st, nil
	}
	return &runtime.ContainerStats{}, nil
}

func (m *mockRuntime) Exec(
	_ context.Context, _ string, _ []string,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}

func (m *mockRuntime) Logs(
	_ context.Context, _ string, _ ...runtime.LogOption,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// Test NewContainerCollector
// Test ContainerCollector.CollectAll
func TestContainerCollector_CollectAll(t *testing.T) {
	tests := []struct {
		name           string
		containers     []runtime.ContainerInfo
		stats          map[string]*runtime.ContainerStats
		statsErr       map[string]error
		listErr        error
		expectedLen    int
		expectError    bool
		errorContains  string
		checkContainer string
		checkStats     *runtime.ContainerStats
	}{
		{
			name: "multiple containers with stats",
			containers: []runtime.ContainerInfo{
				{ID: "c1", Name: "redis"},
				{ID: "c2", Name: "postgres"},
				{ID: "c3", Name: "nginx"},
			},
			stats: map[string]*runtime.ContainerStats{
				"c1": {
					CPUPercent:    15.5,
					MemoryPercent: 25.0,
					MemoryUsage:   1024 * 1024 * 100,
					MemoryLimit:   1024 * 1024 * 512,
				},
				"c2": {
					CPUPercent:    8.2,
					MemoryPercent: 45.0,
					MemoryUsage:   1024 * 1024 * 200,
					MemoryLimit:   1024 * 1024 * 1024,
				},
				"c3": {
					CPUPercent:    2.1,
					MemoryPercent: 10.0,
					MemoryUsage:   1024 * 1024 * 50,
					MemoryLimit:   1024 * 1024 * 256,
				},
			},
			statsErr:       make(map[string]error),
			expectedLen:    3,
			expectError:    false,
			checkContainer: "redis",
			checkStats: &runtime.ContainerStats{
				CPUPercent:    15.5,
				MemoryPercent: 25.0,
				MemoryUsage:   1024 * 1024 * 100,
				MemoryLimit:   1024 * 1024 * 512,
			},
		},
		{
			name:        "empty container list",
			containers:  []runtime.ContainerInfo{},
			stats:       make(map[string]*runtime.ContainerStats),
			statsErr:    make(map[string]error),
			expectedLen: 0,
			expectError: false,
		},
		{
			name:          "list error",
			containers:    nil,
			listErr:       errors.New("docker daemon not available"),
			expectedLen:   0,
			expectError:   true,
			errorContains: "container collector: list",
		},
		{
			name: "partial stats failure - some containers fail",
			containers: []runtime.ContainerInfo{
				{ID: "c1", Name: "redis"},
				{ID: "c2", Name: "postgres"},
			},
			stats: map[string]*runtime.ContainerStats{
				"c1": {CPUPercent: 10.0, MemoryPercent: 20.0},
			},
			statsErr: map[string]error{
				"c2": errors.New("container not running"),
			},
			expectedLen:    1, // Only c1 should be included
			expectError:    false,
			checkContainer: "redis",
			checkStats:     &runtime.ContainerStats{CPUPercent: 10.0, MemoryPercent: 20.0},
		},
		{
			name: "all stats fail",
			containers: []runtime.ContainerInfo{
				{ID: "c1", Name: "redis"},
				{ID: "c2", Name: "postgres"},
			},
			stats: make(map[string]*runtime.ContainerStats),
			statsErr: map[string]error{
				"c1": errors.New("stats error 1"),
				"c2": errors.New("stats error 2"),
			},
			expectedLen: 0, // No containers should be included
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newMockRuntime()
			rt.containers = tc.containers
			rt.stats = tc.stats
			rt.statsErr = tc.statsErr
			rt.listErr = tc.listErr

			collector := monitor.NewContainerCollector(rt)
			ctx := context.Background()

			result, err := collector.CollectAll(ctx)

			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorContains)
				return
			}

			require.NoError(t, err)
			assert.Len(t, result, tc.expectedLen)

			if tc.checkContainer != "" && tc.checkStats != nil {
				res, ok := result[tc.checkContainer]
				require.True(t, ok, "expected container %s in result", tc.checkContainer)
				assert.Equal(t, tc.checkStats.CPUPercent, res.CPUPercent)
				assert.Equal(t, tc.checkStats.MemoryPercent, res.MemoryPercent)
				assert.Equal(t, tc.checkStats.MemoryUsage, res.MemoryUsage)
				assert.Equal(t, tc.checkStats.MemoryLimit, res.MemoryLimit)
			}
		})
	}
}

// Test ContainerCollector.Collect (single container)
func TestContainerCollector_Collect(t *testing.T) {
	tests := []struct {
		name          string
		containerID   string
		stats         *runtime.ContainerStats
		statsErr      error
		status        *runtime.ContainerStatus
		statusErr     error
		expectError   bool
		errorContains string
	}{
		{
			name:        "successful collection",
			containerID: "abc123",
			stats: &runtime.ContainerStats{
				CPUPercent:    12.5,
				MemoryPercent: 35.0,
				MemoryUsage:   1024 * 1024 * 150,
				MemoryLimit:   1024 * 1024 * 512,
			},
			status: &runtime.ContainerStatus{
				ID:   "abc123",
				Name: "my-redis",
			},
			expectError: false,
		},
		{
			name:          "stats error",
			containerID:   "abc123",
			statsErr:      errors.New("container not found"),
			expectError:   true,
			errorContains: "container collector: stats abc123",
		},
		{
			name:        "status error",
			containerID: "abc123",
			stats: &runtime.ContainerStats{
				CPUPercent:    10.0,
				MemoryPercent: 20.0,
			},
			statusErr:     errors.New("status unavailable"),
			expectError:   true,
			errorContains: "container collector: status abc123",
		},
		{
			name:        "zero values",
			containerID: "empty-container",
			stats: &runtime.ContainerStats{
				CPUPercent:    0,
				MemoryPercent: 0,
				MemoryUsage:   0,
				MemoryLimit:   0,
			},
			status: &runtime.ContainerStatus{
				ID:   "empty-container",
				Name: "empty",
			},
			expectError: false,
		},
		{
			name:        "high resource usage",
			containerID: "heavy-container",
			stats: &runtime.ContainerStats{
				CPUPercent:    99.9,
				MemoryPercent: 95.5,
				MemoryUsage:   1024 * 1024 * 1024 * 8,  // 8GB
				MemoryLimit:   1024 * 1024 * 1024 * 16, // 16GB
			},
			status: &runtime.ContainerStatus{
				ID:   "heavy-container",
				Name: "heavy-app",
			},
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newMockRuntime()

			if tc.stats != nil {
				rt.stats[tc.containerID] = tc.stats
			}
			if tc.statsErr != nil {
				rt.statsErr[tc.containerID] = tc.statsErr
			}
			if tc.status != nil {
				rt.status[tc.containerID] = tc.status
			}
			if tc.statusErr != nil {
				rt.statusErr[tc.containerID] = tc.statusErr
			}

			collector := monitor.NewContainerCollector(rt)
			ctx := context.Background()

			result, err := collector.Collect(ctx, tc.containerID)

			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)

			assert.Equal(t, tc.status.Name, result.Name)
			assert.Equal(t, tc.stats.CPUPercent, result.CPUPercent)
			assert.Equal(t, tc.stats.MemoryPercent, result.MemoryPercent)
			assert.Equal(t, tc.stats.MemoryUsage, result.MemoryUsage)
			assert.Equal(t, tc.stats.MemoryLimit, result.MemoryLimit)
		})
	}
}

// Test ContainerCollector with context cancellation
func TestContainerCollector_ContextCancellation(t *testing.T) {
	rt := newMockRuntime()
	rt.containers = []runtime.ContainerInfo{
		{ID: "c1", Name: "redis"},
	}
	rt.stats["c1"] = &runtime.ContainerStats{CPUPercent: 10.0}

	collector := monitor.NewContainerCollector(rt)

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The mock doesn't actually check context, but this tests the API
	result, err := collector.CollectAll(ctx)
	// Since mock doesn't implement context checking, should still succeed
	require.NoError(t, err)
	assert.Len(t, result, 1)
}

// Test ContainerCollector.CollectAll preserves container names correctly
func TestContainerCollector_CollectAll_PreservesNames(t *testing.T) {
	rt := newMockRuntime()
	rt.containers = []runtime.ContainerInfo{
		{ID: "id1", Name: "container-with-dashes"},
		{ID: "id2", Name: "container_with_underscores"},
		{ID: "id3", Name: "MixedCaseContainer"},
	}
	for _, c := range rt.containers {
		rt.stats[c.ID] = &runtime.ContainerStats{CPUPercent: 5.0}
	}

	collector := monitor.NewContainerCollector(rt)
	ctx := context.Background()

	result, err := collector.CollectAll(ctx)
	require.NoError(t, err)

	// Verify all container names are preserved exactly
	assert.Contains(t, result, "container-with-dashes")
	assert.Contains(t, result, "container_with_underscores")
	assert.Contains(t, result, "MixedCaseContainer")
}

// Verify mockRuntime implements runtime.ContainerRuntime
func TestMockRuntime_ImplementsInterface(t *testing.T) {
	var _ runtime.ContainerRuntime = (*mockRuntime)(nil)
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (m *mockRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
