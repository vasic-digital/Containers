package boot_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"digital.vasic.containers/pkg/boot"
	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/discovery"
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/event"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/metrics"
	"digital.vasic.containers/pkg/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test doubles ---

type mockOrchestrator struct {
	upCalled   int
	downCalled int
	upErr      error
	downErr    error
}

func (m *mockOrchestrator) Up(
	_ context.Context, _ compose.ComposeProject,
	_ ...compose.UpOption,
) error {
	m.upCalled++
	return m.upErr
}
func (m *mockOrchestrator) Down(
	_ context.Context, _ compose.ComposeProject,
	_ ...compose.DownOption,
) error {
	m.downCalled++
	return m.downErr
}
func (m *mockOrchestrator) Status(
	_ context.Context, _ compose.ComposeProject,
) ([]compose.ServiceStatus, error) {
	return nil, nil
}
func (m *mockOrchestrator) Logs(
	_ context.Context, _ compose.ComposeProject, _ string,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

var _ compose.ComposeOrchestrator = (*mockOrchestrator)(nil)

type mockHealthChecker struct {
	results map[string]bool
}

func (m *mockHealthChecker) Check(
	_ context.Context, t health.HealthTarget,
) *health.HealthResult {
	h := true
	if m.results != nil {
		if v, ok := m.results[t.Name]; ok {
			h = v
		}
	}
	r := &health.HealthResult{
		Target:    t.Name,
		Healthy:   h,
		Timestamp: time.Now(),
	}
	if !h {
		r.Error = "unhealthy"
	}
	return r
}

func (m *mockHealthChecker) CheckAll(
	ctx context.Context, targets []health.HealthTarget,
) []*health.HealthResult {
	out := make([]*health.HealthResult, len(targets))
	for i, t := range targets {
		out[i] = m.Check(ctx, t)
	}
	return out
}

var _ health.HealthChecker = (*mockHealthChecker)(nil)

type mockDiscoverer struct {
	found map[string]bool
}

func (m *mockDiscoverer) Discover(
	_ context.Context, t discovery.DiscoveryTarget,
) (bool, error) {
	if m.found != nil {
		if v, ok := m.found[t.Name]; ok {
			return v, nil
		}
	}
	return false, errors.New("not discovered")
}

var _ discovery.Discoverer = (*mockDiscoverer)(nil)

type mockRuntime struct{}

func (m *mockRuntime) Name() string { return "mock" }
func (m *mockRuntime) Version(_ context.Context) (string, error) {
	return "1.0", nil
}
func (m *mockRuntime) IsAvailable(_ context.Context) bool { return true }
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
	_ context.Context, _ string,
) (*runtime.ContainerStatus, error) {
	return nil, nil
}
func (m *mockRuntime) List(
	_ context.Context, _ runtime.ListFilter,
) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (m *mockRuntime) Stats(
	_ context.Context, _ string,
) (*runtime.ContainerStats, error) {
	return nil, nil
}
func (m *mockRuntime) Exec(
	_ context.Context, _ string, _ []string,
) (*runtime.ExecResult, error) {
	return nil, nil
}
func (m *mockRuntime) Logs(
	_ context.Context, _ string, _ ...runtime.LogOption,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

var _ runtime.ContainerRuntime = (*mockRuntime)(nil)

type mockMetrics struct {
	bootDuration time.Duration
}

func (m *mockMetrics) IncContainerStarts(_ string)                          {}
func (m *mockMetrics) IncContainerStops(_ string)                           {}
func (m *mockMetrics) IncContainerFailures(_ string)                        {}
func (m *mockMetrics) ObserveHealthCheckDuration(_ string, _ time.Duration) {}
func (m *mockMetrics) ObserveBootDuration(d time.Duration) {
	m.bootDuration = d
}
func (m *mockMetrics) SetContainerUp(_ string, _ bool) {}

var _ metrics.MetricsCollector = (*mockMetrics)(nil)

type mockEventBus struct {
	events []event.Event
}

func (m *mockEventBus) Publish(_ context.Context, e event.Event) {
	m.events = append(m.events, e)
}
func (m *mockEventBus) Subscribe(
	_ event.EventFilter, _ event.EventHandler,
) event.SubscriptionID {
	return ""
}
func (m *mockEventBus) Unsubscribe(_ event.SubscriptionID) {}

var _ event.EventBus = (*mockEventBus)(nil)

// --- Tests ---

func TestBootManager_BootAll_BasicSuccess(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"redis": true, "pg": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"redis": {
			Host:        "localhost",
			Port:        "6379",
			Enabled:     true,
			Required:    true,
			ComposeFile: "docker-compose.yml",
			HealthType:  "tcp",
			Timeout:     5 * time.Second,
		},
		"pg": {
			Host:        "localhost",
			Port:        "5432",
			Enabled:     true,
			Required:    true,
			ComposeFile: "docker-compose.yml",
			HealthType:  "tcp",
			Timeout:     5 * time.Second,
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
		boot.WithLogger(logging.NewStdLogger("test")),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 2, summary.Started)
	assert.Equal(t, 0, summary.Failed)
	assert.Equal(t, 1, orch.upCalled) // grouped by compose file
}

func TestBootManager_BootAll_DisabledSkipped(t *testing.T) {
	endpoints := map[string]endpoint.ServiceEndpoint{
		"disabled": {
			Enabled: false,
		},
	}

	bm := boot.NewBootManager(endpoints)
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Skipped)
}

func TestBootManager_BootAll_DiscoveryPhase(t *testing.T) {
	disc := &mockDiscoverer{
		found: map[string]bool{"remote-db": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"remote-db": {
			Host:             "db.example.com",
			Port:             "5432",
			Enabled:          true,
			DiscoveryEnabled: true,
			DiscoveryMethod:  "tcp",
			DiscoveryTimeout: time.Second,
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithDiscoverer(disc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Discovered)
}

func TestBootManager_BootAll_ComposeFailure(t *testing.T) {
	orch := &mockOrchestrator{
		upErr: errors.New("compose up failed"),
	}
	// §11.4.120 reconciliation for BOOT-4: the ORIGINAL test set NO health
	// checker, so Phase 3 never ran and the compose-failed endpoint was
	// never re-probed — hiding the double-count/error-clobber bug. Wiring an
	// unhealthy checker (the normal prod config: orchestrator + health
	// checker) forces the compose-failed endpoint through Phase 3, where the
	// pre-fix code re-recorded it (Failed==2) and replaced the compose
	// root-cause with the health error. The fix skips already-"failed"
	// endpoints in Phase 3.
	hc := &mockHealthChecker{
		results: map[string]bool{"svc": false},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:        "localhost",
			Port:        "8080",
			Enabled:     true,
			Required:    true,
			ComposeFile: "c.yml",
			HealthType:  "tcp",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	// Exactly ONE failure counted for the single endpoint (not 2).
	assert.Equal(t, 1, summary.Failed)
	// The root-cause compose error is preserved, NOT clobbered by health.
	require.NotNil(t, summary.Results["svc"])
	require.Error(t, summary.Results["svc"].Error)
	assert.Contains(t, summary.Results["svc"].Error.Error(), "compose up failed",
		"compose-up root cause must survive Phase 3, not be replaced by health error")
}

func TestBootManager_BootAll_HealthFailRequired(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"svc": false},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:        "localhost",
			Port:        "8080",
			Enabled:     true,
			Required:    true,
			ComposeFile: "c.yml",
			HealthType:  "tcp",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	assert.True(t, summary.HasFailures())
}

func TestBootManager_BootAll_RemoteService(t *testing.T) {
	hc := &mockHealthChecker{
		results: map[string]bool{"remote": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"remote": {
			Host:        "remote.host",
			Port:        "443",
			Enabled:     true,
			Remote:      true,
			ComposeFile: "c.yml",
			HealthType:  "http",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(&mockOrchestrator{}),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Remote)
}

func TestBootManager_HealthCheckAll(t *testing.T) {
	hc := &mockHealthChecker{
		results: map[string]bool{
			"ok":   true,
			"fail": false,
		},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"ok":   {Enabled: true, HealthType: "tcp"},
		"fail": {Enabled: true, HealthType: "tcp"},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithHealthChecker(hc),
	)

	errs := bm.HealthCheckAll(context.Background())
	assert.Nil(t, errs["ok"])
	assert.NotNil(t, errs["fail"])
}

func TestBootManager_HealthCheckAll_NoChecker(t *testing.T) {
	bm := boot.NewBootManager(
		map[string]endpoint.ServiceEndpoint{},
	)
	errs := bm.HealthCheckAll(context.Background())
	assert.Empty(t, errs)
}

func TestBootManager_Shutdown(t *testing.T) {
	orch := &mockOrchestrator{}
	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
			Profile:     "core",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	err := bm.Shutdown(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, orch.downCalled)
}

func TestBootManager_Shutdown_Error(t *testing.T) {
	orch := &mockOrchestrator{
		downErr: errors.New("down failed"),
	}
	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	err := bm.Shutdown(context.Background())
	assert.Error(t, err)
}

// Additional tests for options coverage

func TestBootManager_WithRuntime(t *testing.T) {
	rt := &mockRuntime{}
	endpoints := map[string]endpoint.ServiceEndpoint{}

	bm := boot.NewBootManager(endpoints,
		boot.WithRuntime(rt),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, summary)
}

func TestBootManager_WithMetrics(t *testing.T) {
	m := &mockMetrics{}
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithMetrics(m),
	)

	_, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Greater(t, m.bootDuration, time.Duration(0))
}

func TestBootManager_WithEventBus(t *testing.T) {
	bus := &mockEventBus{}
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithEventBus(bus),
	)

	_, err := bm.BootAll(context.Background())
	require.NoError(t, err)

	// Should have emitted boot started and completed events.
	assert.Len(t, bus.events, 2)
	assert.Equal(t, event.EventBootStarted, bus.events[0].Type)
	assert.Equal(t, event.EventBootCompleted, bus.events[1].Type)
}

func TestBootManager_WithProjectDir(t *testing.T) {
	endpoints := map[string]endpoint.ServiceEndpoint{}

	bm := boot.NewBootManager(endpoints,
		boot.WithProjectDir("/path/to/project"),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, summary)
}

func TestBootManager_Shutdown_WithEventBus(t *testing.T) {
	bus := &mockEventBus{}
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithEventBus(bus),
	)

	err := bm.Shutdown(context.Background())
	require.NoError(t, err)

	// Should have emitted shutdown started and completed events.
	assert.Len(t, bus.events, 2)
	assert.Equal(t, event.EventShutdownStarted, bus.events[0].Type)
	assert.Equal(t, event.EventShutdownCompleted, bus.events[1].Type)
}

func TestBootManager_BootAll_RemoteWithoutCompose(t *testing.T) {
	hc := &mockHealthChecker{
		results: map[string]bool{"remote": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"remote": {
			Host:    "remote.host",
			Port:    "443",
			Enabled: true,
			Remote:  true,
			// No ComposeFile - should still be marked as remote.
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Remote)
}

func TestBootManager_BootAll_RemoteHealthFail(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"remote": false},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"remote": {
			Host:        "remote.host",
			Port:        "443",
			Enabled:     true,
			Remote:      true,
			Required:    true,
			ComposeFile: "c.yml",
			HealthType:  "http",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	assert.Equal(t, 1, summary.Failed)
	// Remote count should be decremented when health fails.
	assert.Equal(t, 0, summary.Remote)
}

func TestBootManager_BootAll_DiscoveryNotFound(t *testing.T) {
	disc := &mockDiscoverer{
		found: map[string]bool{"remote-db": false},
	}
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"remote-db": {
			Host:             "db.example.com",
			Port:             "5432",
			Enabled:          true,
			DiscoveryEnabled: true,
			DiscoveryMethod:  "tcp",
			DiscoveryTimeout: time.Second,
			ComposeFile:      "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithDiscoverer(disc),
		boot.WithOrchestrator(orch),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// Should not be discovered, should start via compose instead.
	assert.Equal(t, 0, summary.Discovered)
	assert.Equal(t, 1, summary.Started)
}

func TestBootManager_BootAll_NoOrchestrator(t *testing.T) {
	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// With no orchestrator, endpoints with compose files are not started.
	assert.NotNil(t, summary)
}

func TestBootManager_Shutdown_NoOrchestrator(t *testing.T) {
	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints)

	err := bm.Shutdown(context.Background())
	require.NoError(t, err)
}

func TestBootManager_BootAll_WithProfile(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"svc1": true, "svc2": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc1": {
			Enabled:     true,
			ComposeFile: "c.yml",
			Profile:     "core",
		},
		"svc2": {
			Enabled:     true,
			ComposeFile: "c.yml",
			Profile:     "core",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Started)
	// Both services share the same compose file, so only one Up call.
	assert.Equal(t, 1, orch.upCalled)
}

func TestBootManager_Shutdown_WithProfile(t *testing.T) {
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc1": {
			Enabled:     true,
			ComposeFile: "c.yml",
			Profile:     "core",
		},
		"svc2": {
			Enabled:     true,
			ComposeFile: "c.yml",
			Profile:     "core",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	err := bm.Shutdown(context.Background())
	require.NoError(t, err)
	// Both share compose file, so one Down call.
	assert.Equal(t, 1, orch.downCalled)
}

func TestBootManager_BootAll_MultipleComposeFiles(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"db": true, "app": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"db": {
			Enabled:     true,
			ComposeFile: "db.yml",
		},
		"app": {
			Enabled:     true,
			ComposeFile: "app.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Started)
	// Different compose files, so two Up calls.
	assert.Equal(t, 2, orch.upCalled)
}

func TestBootManager_BootAll_LocalHealthFailOptional(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"svc": false},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:        "localhost",
			Port:        "8080",
			Enabled:     true,
			Required:    false, // Not required, so health fail is OK.
			ComposeFile: "c.yml",
			HealthType:  "tcp",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	// Should not error because service is not required.
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Failed)
}

func TestBootManager_BootAll_ComposeFailureMultipleServices(t *testing.T) {
	orch := &mockOrchestrator{
		upErr: errors.New("compose up failed"),
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc1": {
			Enabled:     true,
			Required:    true,
			ComposeFile: "c.yml",
		},
		"svc2": {
			Enabled:     true,
			Required:    true,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	// Both services fail because they share the same compose file.
	assert.Equal(t, 2, summary.Failed)
}

func TestBootManager_Shutdown_MultipleComposeFiles(t *testing.T) {
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"db": {
			Enabled:     true,
			ComposeFile: "db.yml",
		},
		"app": {
			Enabled:     true,
			ComposeFile: "app.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	err := bm.Shutdown(context.Background())
	require.NoError(t, err)
	// Different compose files, so two Down calls.
	assert.Equal(t, 2, orch.downCalled)
}

func TestBootManager_Shutdown_MultipleErrors(t *testing.T) {
	orch := &mockOrchestrator{
		downErr: errors.New("down failed"),
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"db": {
			Enabled:     true,
			ComposeFile: "db.yml",
		},
		"app": {
			Enabled:     true,
			ComposeFile: "app.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
	)

	err := bm.Shutdown(context.Background())
	// Should return the first error.
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "down failed")
}

func TestBootManager_BootAll_DisabledNotInComposeGroup(t *testing.T) {
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"enabled": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"enabled": {
			Enabled:     true,
			ComposeFile: "c.yml",
		},
		"disabled": {
			Enabled:     false,
			ComposeFile: "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Started)
	assert.Equal(t, 1, summary.Skipped)
}

func TestBootManager_BootAll_LocalServiceHealthFailDecrementStarted(t *testing.T) {
	// This test covers the specific case where a local (non-remote) service
	// starts successfully via compose but then fails health check.
	// This should decrement summary.Started (not summary.Remote).
	orch := &mockOrchestrator{}
	hc := &mockHealthChecker{
		results: map[string]bool{"local": false},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"local": {
			Host:        "localhost",
			Port:        "8080",
			Enabled:     true,
			Remote:      false, // Local service.
			Required:    true,
			ComposeFile: "c.yml",
			HealthType:  "tcp",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithOrchestrator(orch),
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	assert.Equal(t, 1, summary.Failed)
	// Started should be decremented to 0 after health check failure.
	assert.Equal(t, 0, summary.Started)
}

func TestBootManager_BootAll_AlreadyProcessedInDiscovery(t *testing.T) {
	// Test that if a service is discovered, it's not processed again
	// in the compose phase (the "already" check).
	disc := &mockDiscoverer{
		found: map[string]bool{"svc": true},
	}
	orch := &mockOrchestrator{}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:             "db.example.com",
			Port:             "5432",
			Enabled:          true,
			DiscoveryEnabled: true,
			ComposeFile:      "c.yml",
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithDiscoverer(disc),
		boot.WithOrchestrator(orch),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Discovered)
	assert.Equal(t, 0, summary.Started) // Should not also be started.
	assert.Equal(t, 1, orch.upCalled)   // Compose still runs for the group.
}

func TestBootManager_BootAll_DisabledInHandleWithoutCompose(t *testing.T) {
	// Test the branch in "Handle endpoints without a compose file"
	// where an endpoint is disabled but not yet processed.

	endpoints := map[string]endpoint.ServiceEndpoint{
		"no-compose": {
			Host:    "localhost",
			Port:    "8080",
			Enabled: false,
			// No ComposeFile.
		},
	}

	bm := boot.NewBootManager(endpoints)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Skipped)
}

func TestBootManager_BootAll_EnabledNoComposeNotRemote(t *testing.T) {
	// Test enabled endpoint without compose file that is not remote.
	// This should not add to any counts (falls through without action).

	hc := &mockHealthChecker{
		results: map[string]bool{"local-no-compose": true},
	}

	endpoints := map[string]endpoint.ServiceEndpoint{
		"local-no-compose": {
			Host:       "localhost",
			Port:       "8080",
			Enabled:    true,
			Remote:     false,
			HealthType: "tcp",
			// No ComposeFile.
		},
	}

	bm := boot.NewBootManager(endpoints,
		boot.WithHealthChecker(hc),
	)

	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// Should not be counted as started, remote, discovered, or failed.
	assert.Equal(t, 0, summary.Started)
	assert.Equal(t, 0, summary.Remote)
	assert.Equal(t, 0, summary.Discovered)
	assert.Equal(t, 0, summary.Failed)
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (m *mockRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
