// Package lazyservice unit tests.
//
// These tests exercise the LazyOrchestrator's pure logic — registration
// validation + defaulting, dependency-ordered startup, alternative-service
// fallback, stop / stop-all bookkeeping, status mapping, and the filter
// queries (ListServices / ListByCategory / ListFreeServices).
//
// NO real docker/podman/daemon/network dependency is used: the compose
// orchestrator and health checker are injected as in-process fakes via the
// functional options (WithOrchestrator / WithHealthChecker), so every test
// runs hermetically. Per the constitution, fakes are permitted in unit-test
// sources only; this file IS a unit-test source (`_test.go`).
package lazyservice

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
)

// ---------------------------------------------------------------------------
// Fakes (in-process, no daemon). Permitted in unit tests only.
// ---------------------------------------------------------------------------

// fakeOrchestrator records Up/Down calls and returns scripted errors.
type fakeOrchestrator struct {
	mu       sync.Mutex
	upCalls  []compose.ComposeProject
	downCall []compose.ComposeProject
	upErr    map[string]error // keyed by ComposeProject.File
	downErr  map[string]error
}

func newFakeOrchestrator() *fakeOrchestrator {
	return &fakeOrchestrator{upErr: map[string]error{}, downErr: map[string]error{}}
}

func (f *fakeOrchestrator) Up(_ context.Context, p compose.ComposeProject, _ ...compose.UpOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upCalls = append(f.upCalls, p)
	return f.upErr[p.File]
}

func (f *fakeOrchestrator) Down(_ context.Context, p compose.ComposeProject, _ ...compose.DownOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downCall = append(f.downCall, p)
	return f.downErr[p.File]
}

func (f *fakeOrchestrator) Status(_ context.Context, _ compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (f *fakeOrchestrator) Logs(_ context.Context, _ compose.ComposeProject, _ string) (io.ReadCloser, error) {
	return nil, nil
}

// upCount returns how many times Up was invoked for a given compose file.
func (f *fakeOrchestrator) upCount(file string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.upCalls {
		if p.File == file {
			n++
		}
	}
	return n
}

func (f *fakeOrchestrator) downCount(file string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.downCall {
		if p.File == file {
			n++
		}
	}
	return n
}

// fakeHealthChecker returns a scripted health result.
type fakeHealthChecker struct {
	healthy bool
	calls   int
	mu      sync.Mutex
}

func (f *fakeHealthChecker) Check(_ context.Context, t health.HealthTarget) *health.HealthResult {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return &health.HealthResult{Target: t.Name, Healthy: f.healthy}
}

func (f *fakeHealthChecker) CheckAll(ctx context.Context, targets []health.HealthTarget) []*health.HealthResult {
	out := make([]*health.HealthResult, len(targets))
	for i, t := range targets {
		out[i] = f.Check(ctx, t)
	}
	return out
}

func (f *fakeHealthChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestOrchestrator constructs a LazyOrchestrator wired to the supplied
// fakes so no real default orchestrator / health checker is created.
func newTestOrchestrator(t *testing.T, o compose.ComposeOrchestrator, hc health.HealthChecker) *LazyOrchestrator {
	t.Helper()
	lo, err := NewLazyOrchestrator(
		WithOrchestrator(o),
		WithHealthChecker(hc),
	)
	if err != nil {
		t.Fatalf("NewLazyOrchestrator returned error: %v", err)
	}
	return lo
}

// ---------------------------------------------------------------------------
// Functional options
// ---------------------------------------------------------------------------

func TestLazyOrchestrator_Options_ApplyLoggerAndWorkDir(t *testing.T) {
	lg := logging.NopLogger{}
	lo, err := NewLazyOrchestrator(
		WithOrchestrator(newFakeOrchestrator()),
		WithHealthChecker(&fakeHealthChecker{}),
		WithLogger(lg),
		WithWorkDir("/tmp/lazyservice-test-workdir"),
	)
	if err != nil {
		t.Fatalf("NewLazyOrchestrator error: %v", err)
	}
	if lo.workDir != "/tmp/lazyservice-test-workdir" {
		t.Errorf("WithWorkDir not applied: workDir = %q", lo.workDir)
	}
	if lo.logger != lg {
		t.Error("WithLogger not applied")
	}
}

// ---------------------------------------------------------------------------
// RegisterService — validation + defaulting
// ---------------------------------------------------------------------------

func TestLazyOrchestrator_RegisterService_ValidationAndDefaults(t *testing.T) {
	tests := []struct {
		name    string
		svc     *ServiceDefinition
		wantErr bool
	}{
		{
			name:    "empty name rejected",
			svc:     &ServiceDefinition{ComposeFile: "x.yml"},
			wantErr: true,
		},
		{
			name:    "empty compose file rejected",
			svc:     &ServiceDefinition{Name: "svc"},
			wantErr: true,
		},
		{
			name:    "valid minimal service accepted",
			svc:     &ServiceDefinition{Name: "svc", ComposeFile: "x.yml"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
			err := lo.RegisterService(tc.svc)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				return
			}
			// Defaults must be applied.
			if tc.svc.StartTimeout != 5*time.Minute {
				t.Errorf("StartTimeout default = %v, want 5m", tc.svc.StartTimeout)
			}
			if tc.svc.StopTimeout != 30*time.Second {
				t.Errorf("StopTimeout default = %v, want 30s", tc.svc.StopTimeout)
			}
			if tc.svc.CostModel != "free" {
				t.Errorf("CostModel default = %q, want free", tc.svc.CostModel)
			}
		})
	}
}

func TestLazyOrchestrator_RegisterService_PreservesExplicitValues(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	svc := &ServiceDefinition{
		Name:         "svc",
		ComposeFile:  "x.yml",
		StartTimeout: 99 * time.Second,
		StopTimeout:  7 * time.Second,
		CostModel:    "paid",
	}
	if err := lo.RegisterService(svc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.StartTimeout != 99*time.Second {
		t.Errorf("StartTimeout overwritten: %v", svc.StartTimeout)
	}
	if svc.StopTimeout != 7*time.Second {
		t.Errorf("StopTimeout overwritten: %v", svc.StopTimeout)
	}
	if svc.CostModel != "paid" {
		t.Errorf("CostModel overwritten: %v", svc.CostModel)
	}
}

// ---------------------------------------------------------------------------
// StartService — happy path, not-found, dependency ordering, fallback
// ---------------------------------------------------------------------------

func TestLazyOrchestrator_StartService_NotFound(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	err := lo.StartService(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestLazyOrchestrator_StartService_HappyPath_NoHealthCheck(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "a", ComposeFile: "a.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "a"); err != nil {
		t.Fatalf("StartService error: %v", err)
	}
	if fo.upCount("a.yml") != 1 {
		t.Errorf("compose Up calls for a.yml = %d, want 1", fo.upCount("a.yml"))
	}
	st, err := lo.GetServiceStatus("a")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started {
		t.Error("service a should report Started after StartService")
	}
}

func TestLazyOrchestrator_StartService_StartsDependenciesFirst(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	// "app" depends on "db"; starting app must also start db.
	if err := lo.RegisterService(&ServiceDefinition{Name: "db", ComposeFile: "db.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.RegisterService(&ServiceDefinition{
		Name: "app", ComposeFile: "app.yml", Dependencies: []string{"db"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "app"); err != nil {
		t.Fatalf("StartService error: %v", err)
	}
	if fo.upCount("db.yml") != 1 {
		t.Errorf("dependency db NOT started: db.yml Up calls = %d, want 1", fo.upCount("db.yml"))
	}
	if fo.upCount("app.yml") != 1 {
		t.Errorf("app.yml Up calls = %d, want 1", fo.upCount("app.yml"))
	}
}

func TestLazyOrchestrator_StartService_MissingDependencyFails(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{
		Name: "app", ComposeFile: "app.yml", Dependencies: []string{"ghost"},
	}); err != nil {
		t.Fatal(err)
	}
	err := lo.StartService(context.Background(), "app")
	if err == nil {
		t.Fatal("expected error when dependency does not exist")
	}
	if fo.upCount("app.yml") != 0 {
		t.Errorf("app must NOT start when its dependency is missing; Up calls = %d", fo.upCount("app.yml"))
	}
}

func TestLazyOrchestrator_StartService_FallsBackToAlternative(t *testing.T) {
	fo := newFakeOrchestrator()
	// Primary's compose Up fails; alternative succeeds.
	fo.upErr["primary.yml"] = errors.New("primary boom")
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "alt", ComposeFile: "alt.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.RegisterService(&ServiceDefinition{
		Name: "primary", ComposeFile: "primary.yml", AlternativeServices: []string{"alt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "primary"); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if fo.upCount("alt.yml") != 1 {
		t.Errorf("alternative alt NOT started: alt.yml Up calls = %d, want 1", fo.upCount("alt.yml"))
	}
	altSt, err := lo.GetServiceStatus("alt")
	if err != nil {
		t.Fatal(err)
	}
	if !altSt.Started {
		t.Error("alternative should be Started after fallback")
	}
}

func TestLazyOrchestrator_StartService_FailsWhenNoAlternativeWorks(t *testing.T) {
	fo := newFakeOrchestrator()
	fo.upErr["primary.yml"] = errors.New("primary boom")
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{
		Name: "primary", ComposeFile: "primary.yml",
	}); err != nil {
		t.Fatal(err)
	}
	err := lo.StartService(context.Background(), "primary")
	if err == nil {
		t.Fatal("expected error when primary fails and no alternative exists")
	}
}

func TestLazyOrchestrator_StartService_HealthCheckFailureFails(t *testing.T) {
	fo := newFakeOrchestrator()
	hc := &fakeHealthChecker{healthy: false}
	lo := newTestOrchestrator(t, fo, hc)
	if err := lo.RegisterService(&ServiceDefinition{
		Name:        "svc",
		ComposeFile: "svc.yml",
		HealthCheck: &health.HealthTarget{Name: "svc", Host: "127.0.0.1", Port: "1"},
		// Timeout long enough for >=1 poll tick (ticker is 2s) so the
		// unhealthy-poll path is genuinely exercised before the deadline.
		StartTimeout: 3 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	err := lo.StartService(context.Background(), "svc")
	if err == nil {
		t.Fatal("expected error when health check never becomes healthy")
	}
	if hc.callCount() == 0 {
		t.Error("health checker was never invoked")
	}
}

func TestLazyOrchestrator_StartService_HealthCheckPassSucceeds(t *testing.T) {
	fo := newFakeOrchestrator()
	hc := &fakeHealthChecker{healthy: true}
	lo := newTestOrchestrator(t, fo, hc)
	if err := lo.RegisterService(&ServiceDefinition{
		Name:         "svc",
		ComposeFile:  "svc.yml",
		HealthCheck:  &health.HealthTarget{Name: "svc", Host: "127.0.0.1", Port: "1"},
		StartTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "svc"); err != nil {
		t.Fatalf("expected healthy service to start, got error: %v", err)
	}
	if hc.callCount() == 0 {
		t.Error("health checker was never invoked")
	}
	st, err := lo.GetServiceStatus("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started {
		t.Error("service should be Started after a passing health check")
	}
}

// ---------------------------------------------------------------------------
// StopService / StopAll
// ---------------------------------------------------------------------------

func TestLazyOrchestrator_StopService_NotFound(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	err := lo.StopService(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error stopping unknown service")
	}
}

func TestLazyOrchestrator_StopService_NeverStartedIsNoop(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "svc", ComposeFile: "svc.yml"}); err != nil {
		t.Fatal(err)
	}
	// Not started yet — Stop must be a no-op (no Down call, no error).
	if err := lo.StopService(context.Background(), "svc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fo.downCount("svc.yml") != 0 {
		t.Errorf("Down must not be called for a never-started service; got %d", fo.downCount("svc.yml"))
	}
}

func TestLazyOrchestrator_StopService_StartedServiceCallsDown(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "svc", ComposeFile: "svc.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}
	if err := lo.StopService(context.Background(), "svc"); err != nil {
		t.Fatalf("StopService error: %v", err)
	}
	if fo.downCount("svc.yml") != 1 {
		t.Errorf("Down calls = %d, want 1", fo.downCount("svc.yml"))
	}
	// After stop, the started flag must be cleared.
	lo.mu.RLock()
	started := lo.started["svc"]
	lo.mu.RUnlock()
	if started {
		t.Error("started flag must be false after StopService")
	}
}

func TestLazyOrchestrator_StopService_PropagatesDownError(t *testing.T) {
	fo := newFakeOrchestrator()
	fo.downErr["svc.yml"] = errors.New("down boom")
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "svc", ComposeFile: "svc.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}
	err := lo.StopService(context.Background(), "svc")
	if err == nil {
		t.Fatal("expected Down error to propagate")
	}
}

func TestLazyOrchestrator_StopAll_StopsOnlyStartedServices(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	for _, n := range []string{"a", "b", "c"} {
		if err := lo.RegisterService(&ServiceDefinition{Name: n, ComposeFile: n + ".yml"}); err != nil {
			t.Fatal(err)
		}
	}
	// Only start a and b; c stays down.
	if err := lo.StartService(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := lo.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll error: %v", err)
	}
	if fo.downCount("a.yml") != 1 || fo.downCount("b.yml") != 1 {
		t.Errorf("started services a,b must be stopped: a=%d b=%d", fo.downCount("a.yml"), fo.downCount("b.yml"))
	}
	if fo.downCount("c.yml") != 0 {
		t.Errorf("never-started service c must NOT be stopped: c=%d", fo.downCount("c.yml"))
	}
}

func TestLazyOrchestrator_StopAll_AggregatesErrors(t *testing.T) {
	fo := newFakeOrchestrator()
	fo.downErr["a.yml"] = errors.New("a boom")
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "a", ComposeFile: "a.yml"}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	err := lo.StopAll(context.Background())
	if err == nil {
		t.Fatal("expected StopAll to report aggregated error")
	}
}

// ---------------------------------------------------------------------------
// GetServiceStatus
// ---------------------------------------------------------------------------

func TestLazyOrchestrator_GetServiceStatus_NotFound(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	if _, err := lo.GetServiceStatus("nope"); err == nil {
		t.Fatal("expected error for unknown service status")
	}
}

func TestLazyOrchestrator_GetServiceStatus_MapsFields(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	svc := &ServiceDefinition{
		Name:        "svc",
		ComposeFile: "svc.yml",
		Category:    "database",
		CostModel:   "freemium",
		Description: "test db",
	}
	if err := lo.RegisterService(svc); err != nil {
		t.Fatal(err)
	}
	st, err := lo.GetServiceStatus("svc")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "svc" || st.Category != "database" || st.CostModel != "freemium" || st.Description != "test db" {
		t.Errorf("status field mapping wrong: %+v", st)
	}
	if st.Started {
		t.Error("freshly-registered service must not report Started")
	}
}

// ---------------------------------------------------------------------------
// List queries
// ---------------------------------------------------------------------------

func registerSet(t *testing.T, lo *LazyOrchestrator) {
	t.Helper()
	defs := []*ServiceDefinition{
		{Name: "pg", ComposeFile: "pg.yml", Category: "database", CostModel: "free"},
		{Name: "redis", ComposeFile: "redis.yml", Category: "database", CostModel: "freemium"},
		{Name: "openai", ComposeFile: "openai.yml", Category: "llm", CostModel: "paid"},
	}
	for _, d := range defs {
		if err := lo.RegisterService(d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLazyOrchestrator_ListServices_ReturnsAll(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	registerSet(t, lo)
	if got := len(lo.ListServices()); got != 3 {
		t.Errorf("ListServices count = %d, want 3", got)
	}
}

func TestLazyOrchestrator_ListByCategory_Filters(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	registerSet(t, lo)
	if got := len(lo.ListByCategory("database")); got != 2 {
		t.Errorf("ListByCategory(database) = %d, want 2", got)
	}
	if got := len(lo.ListByCategory("llm")); got != 1 {
		t.Errorf("ListByCategory(llm) = %d, want 1", got)
	}
	if got := len(lo.ListByCategory("nonexistent")); got != 0 {
		t.Errorf("ListByCategory(nonexistent) = %d, want 0", got)
	}
}

func TestLazyOrchestrator_ListFreeServices_ExcludesPaid(t *testing.T) {
	lo := newTestOrchestrator(t, newFakeOrchestrator(), &fakeHealthChecker{})
	registerSet(t, lo)
	free := lo.ListFreeServices()
	if len(free) != 2 {
		t.Fatalf("ListFreeServices count = %d, want 2 (free + freemium, not paid)", len(free))
	}
	for _, s := range free {
		if s.CostModel == "paid" {
			t.Errorf("paid service %q leaked into ListFreeServices", s.Name)
		}
	}
}
