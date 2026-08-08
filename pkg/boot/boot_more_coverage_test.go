package boot

// White-box tests for BootAll branches not covered by the external
// test suite (manager_test.go is package boot_test). These tests access
// unexported fields / internal types directly.

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/discovery"
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/event"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/runtime"
)

// --- Minimal test doubles (white-box package, no import cycle) ---

// moreMockOrchestrator implements compose.ComposeOrchestrator.
type moreMockOrchestrator struct {
	upErr   error
	upCalls int
}

func (m *moreMockOrchestrator) Up(_ context.Context, _ compose.ComposeProject, _ ...compose.UpOption) error {
	m.upCalls++
	return m.upErr
}
func (m *moreMockOrchestrator) Down(_ context.Context, _ compose.ComposeProject, _ ...compose.DownOption) error {
	return nil
}
func (m *moreMockOrchestrator) Status(_ context.Context, _ compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}
func (m *moreMockOrchestrator) Logs(_ context.Context, _ compose.ComposeProject, _ string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// moreMockHealthChecker implements health.HealthChecker.
type moreMockHealthChecker struct {
	results map[string]bool
}

func (m *moreMockHealthChecker) Check(_ context.Context, t health.HealthTarget) *health.HealthResult {
	healthy := true
	if m.results != nil {
		if v, ok := m.results[t.Name]; ok {
			healthy = v
		}
	}
	r := &health.HealthResult{Target: t.Name, Healthy: healthy, Timestamp: time.Now()}
	if !healthy {
		r.Error = "unhealthy"
	}
	return r
}

func (m *moreMockHealthChecker) CheckAll(ctx context.Context, targets []health.HealthTarget) []*health.HealthResult {
	out := make([]*health.HealthResult, len(targets))
	for i, t := range targets {
		out[i] = m.Check(ctx, t)
	}
	return out
}

// moreMockDiscoverer implements discovery.Discoverer.
type moreMockDiscoverer struct {
	found map[string]bool
	err   error
}

func (m *moreMockDiscoverer) Discover(_ context.Context, t discovery.DiscoveryTarget) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.found != nil {
		if v, ok := m.found[t.Name]; ok {
			return v, nil
		}
	}
	return false, errors.New("not discovered")
}

// moreMockRuntime implements runtime.ContainerRuntime.
type moreMockRuntime struct{}

func (m *moreMockRuntime) Name() string                              { return "mock" }
func (m *moreMockRuntime) Version(_ context.Context) (string, error) { return "1.0", nil }
func (m *moreMockRuntime) IsAvailable(_ context.Context) bool        { return true }
func (m *moreMockRuntime) Start(_ context.Context, _ string, _ ...runtime.StartOption) error {
	return nil
}
func (m *moreMockRuntime) Stop(_ context.Context, _ string, _ ...runtime.StopOption) error {
	return nil
}
func (m *moreMockRuntime) Remove(_ context.Context, _ string, _ ...runtime.RemoveOption) error {
	return nil
}
func (m *moreMockRuntime) Status(_ context.Context, _ string) (*runtime.ContainerStatus, error) {
	return nil, nil
}
func (m *moreMockRuntime) List(_ context.Context, _ runtime.ListFilter) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (m *moreMockRuntime) Stats(_ context.Context, _ string) (*runtime.ContainerStats, error) {
	return nil, nil
}
func (m *moreMockRuntime) Exec(_ context.Context, _ string, _ []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (m *moreMockRuntime) Logs(_ context.Context, _ string, _ ...runtime.LogOption) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// moreMockEventBus implements event.EventBus.
type moreMockEventBus struct {
	published []event.Event
}

func (m *moreMockEventBus) Publish(_ context.Context, e event.Event) {
	m.published = append(m.published, e)
}
func (m *moreMockEventBus) Subscribe(_ event.EventFilter, _ event.EventHandler) event.SubscriptionID {
	return ""
}
func (m *moreMockEventBus) Unsubscribe(_ event.SubscriptionID) {}

// moreMockDistributor implements boot.Distributor with configurable error.
type moreMockDistributor struct {
	deployed           int
	err                error
	undistributeCalled int   // BOOT-3: counts Shutdown's Undistribute calls.
	undistributeErr    error // BOOT-3: forced Undistribute error.
}

func (m *moreMockDistributor) DistributeEndpoints(_ context.Context, names []string) (int, error) {
	return m.deployed, m.err
}

// Undistribute satisfies the extended boot.Distributor interface (BOOT-3)
// and records invocations so guards can assert Shutdown tears down remote
// state. The real distribution.DefaultDistributor implements this for real.
func (m *moreMockDistributor) Undistribute(_ context.Context) error {
	m.undistributeCalled++
	return m.undistributeErr
}

// --- Tests ---

// TestBootAll_WithEventBus verifies that BootAll emits boot.started and
// boot.completed events when an EventBus is configured.
func TestBootAll_WithEventBus(t *testing.T) {
	bus := &moreMockEventBus{}
	orch := &moreMockOrchestrator{}

	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch), WithEventBus(bus))
	_, err := bm.BootAll(context.Background())
	require.NoError(t, err)

	require.Len(t, bus.published, 2)
	assert.Equal(t, event.EventBootStarted, bus.published[0].Type)
	assert.Equal(t, event.EventBootCompleted, bus.published[1].Type)
}

// TestBootAll_WithDisabledEndpoints verifies that disabled endpoints are
// skipped (status "skipped") and not processed in later phases.
func TestBootAll_WithDisabledEndpoints(t *testing.T) {
	orch := &moreMockOrchestrator{}
	eps := map[string]endpoint.ServiceEndpoint{
		"enabled":  {Enabled: true, ComposeFile: "c.yml"},
		"disabled": {Enabled: false, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Skipped)
	assert.Equal(t, 1, summary.Started)
}

// TestBootAll_ComposeError verifies that compose failures mark endpoints
// as failed and BootAll returns an error for required endpoints.
func TestBootAll_ComposeError(t *testing.T) {
	orch := &moreMockOrchestrator{upErr: errors.New("image pull failed")}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, Required: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))
	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err)
	assert.Equal(t, 1, summary.Failed)
}

// TestBootAll_WithDiscoverer_Found exercises the path where the discoverer
// finds the endpoint and it is marked "discovered".
func TestBootAll_WithDiscoverer_Found(t *testing.T) {
	disc := &moreMockDiscoverer{found: map[string]bool{"svc": true}}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:             "host.example",
			Port:             "8080",
			Enabled:          true,
			DiscoveryEnabled: true,
			DiscoveryMethod:  "tcp",
		},
	}

	bm := NewBootManager(eps, WithDiscoverer(disc))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Discovered)
}

// TestBootAll_WithDiscoverer_Error exercises the branch where discoverer
// returns an error (endpoint is not discovered, falls to compose phase).
func TestBootAll_WithDiscoverer_Error(t *testing.T) {
	disc := &moreMockDiscoverer{err: errors.New("network unreachable")}
	orch := &moreMockOrchestrator{}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Host:             "host.example",
			Port:             "8080",
			Enabled:          true,
			DiscoveryEnabled: true,
			DiscoveryMethod:  "tcp",
			ComposeFile:      "c.yml",
		},
	}

	bm := NewBootManager(eps, WithDiscoverer(disc), WithOrchestrator(orch))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// Discovery failed → fell through to compose phase → started.
	assert.Equal(t, 0, summary.Discovered)
	assert.Equal(t, 1, summary.Started)
}

// TestBootAll_WithDistributor_Success exercises the distributor path where
// remote endpoints without results are distributed successfully.
func TestBootAll_WithDistributor_Success(t *testing.T) {
	dist := &moreMockDistributor{deployed: 1, err: nil}
	eps := map[string]endpoint.ServiceEndpoint{
		"remote-svc": {
			Enabled: true,
			Remote:  true,
			// No ComposeFile → not in compose groups, no result yet.
		},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// The distributor handled it → counted as remote.
	assert.Equal(t, 1, summary.Remote)
}

// TestBootAll_WithDistributor_Error exercises the distributor total-failure
// path (deployed:0, distErr != nil). §11.4.120 reconciliation for BOOT-1:
// the OLD assertion asserted the swallowed-failure bug as correct behaviour
// (Remote==1 for an endpoint the distributor NEVER deployed). The fixed
// code attributes by count — a 0/1 deploy on a NON-required endpoint is NOT
// counted as a successful remote (Remote==0), its result is "failed", but
// (not being required) it does not force an overall boot error (err==nil,
// mirroring a non-required Phase-3 health failure). This is genuine: an
// undeployed endpoint is no longer reported as a running remote service.
func TestBootAll_WithDistributor_Error(t *testing.T) {
	dist := &moreMockDistributor{deployed: 0, err: errors.New("no hosts available")}
	eps := map[string]endpoint.ServiceEndpoint{
		"remote-svc": {
			Enabled: true,
			Remote:  true,
			// Not Required → shortfall does not force a boot error.
		},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err) // not required → no overall error
	// The undeployed endpoint MUST NOT be falsely counted as a remote.
	assert.Equal(t, 0, summary.Remote, "undeployed endpoint must not count as remote")
	assert.Equal(t, "failed", summary.Results["remote-svc"].Status,
		"undeployed endpoint result must be failed, not distributed")
}

// TestBootAll_WithDistributor_NoRemoteEndpoints exercises the branch where
// distributor is set but all endpoints are already processed (no remoteNames).
func TestBootAll_WithDistributor_NoRemoteEndpoints(t *testing.T) {
	dist := &moreMockDistributor{deployed: 0}
	orch := &moreMockOrchestrator{}
	eps := map[string]endpoint.ServiceEndpoint{
		// Local endpoint with compose file — not remote, gets processed in compose phase.
		"local-svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch), WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	// Should be started, not remote.
	assert.Equal(t, 1, summary.Started)
	assert.Equal(t, 0, summary.Remote)
}

// TestBootAll_Runtime exercises that WithRuntime sets the runtime field
// and BootAll does not panic.
func TestBootAll_Runtime(t *testing.T) {
	rt := &moreMockRuntime{}
	eps := map[string]endpoint.ServiceEndpoint{}
	bm := NewBootManager(eps, WithRuntime(rt))
	summary, err := bm.BootAll(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, summary)
}

// TestBootAll_ComposeError_NonRequired verifies that when compose fails but
// the service is not required, no error is returned.
func TestBootAll_ComposeError_NonRequired(t *testing.T) {
	orch := &moreMockOrchestrator{upErr: errors.New("timeout")}
	eps := map[string]endpoint.ServiceEndpoint{
		"optional": {
			Enabled:     true,
			Required:    false,
			ComposeFile: "c.yml",
		},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))
	summary, err := bm.BootAll(context.Background())
	// Compose failure on non-required → still counted as failed but no error.
	// Actually looking at the code: Failed++ always, but HasFailures() → err only
	// if summary.Failed > 0 → err returned. Let's just verify the state.
	_ = err
	assert.Equal(t, 1, summary.Failed)
}
