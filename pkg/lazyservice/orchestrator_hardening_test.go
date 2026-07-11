// Package lazyservice Wave-20 LZSVC-HARD regression guards.
//
// Four deterministic §11.4.115 GREEN-polarity guards for the Wave-20 hardening
// batch. Each guard PASSes on the fixed tree and reproduces its defect (FAILs)
// when the corresponding fix is surgically reverted:
//
//	LZSVC-1  container leak on health-fail-after-up — the just-started stack
//	         must be Down'd when Up succeeds but health fails, else it orphans
//	         (StopService/StopAll skip it because started was never set).
//	LZSVC-2  caller ctx ignored — StartService's ctx must thread into the boot
//	         so a caller cancel/deadline aborts the compose Up promptly instead
//	         of running to StartTimeout.
//	LZSVC-3  failed boot reports Started=true — GetServiceStatus must report
//	         Started from the orchestrator's own success flag, so a failed boot
//	         reports Started=false alongside its LastError.
//	LZSVC-4  spurious health timeout — an instantly-healthy service with
//	         StartTimeout<2s must start (immediate probe) instead of timing out
//	         waiting for the first 2s ticker tick.
//
// NO real docker/podman/daemon/network dependency is used: the compose
// orchestrator and health checker are in-process fakes injected via the
// functional options. Fakes are permitted in unit-test sources only; this file
// IS a unit-test source (`_test.go`).
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
)

// ---------------------------------------------------------------------------
// LZSVC-1 — container leak on health-fail-after-up
// ---------------------------------------------------------------------------

// TestLazyOrchestrator_StartService_HealthFailAfterUp_TearsDownStack asserts
// that when compose Up succeeds but the health check never passes, the
// just-started stack is torn down (a Down call is recorded) so no orphan
// survives, and that StopAll then leaves nothing running and does not perturb
// the already-torn-down stack.
//
// RED (LZSVC-1 fix reverted): Up succeeds, health fails, startServiceInternal
// returns before setting started=true and WITHOUT calling Down → Down count 0
// → orphaned stack. GREEN (fixed): Down count 1.
func TestLazyOrchestrator_StartService_HealthFailAfterUp_TearsDownStack(t *testing.T) {
	fo := newFakeOrchestrator()
	hc := &fakeHealthChecker{healthy: false}
	lo := newTestOrchestrator(t, fo, hc)
	if err := lo.RegisterService(&ServiceDefinition{
		Name:        "svc",
		ComposeFile: "svc.yml",
		HealthCheck: &health.HealthTarget{Name: "svc", Host: "127.0.0.1", Port: "1"},
		// Short bound: the immediate probe (LZSVC-4) fails, then the boot ctx
		// deadline fires quickly so the health wait returns fast — keeping the
		// RED capture bounded.
		StartTimeout: 300 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	err := lo.StartService(context.Background(), "svc")
	if err == nil {
		t.Fatal("expected StartService to fail when the health check never passes")
	}

	// Up genuinely started a stack.
	if fo.upCount("svc.yml") != 1 {
		t.Fatalf("compose Up calls for svc.yml = %d, want 1", fo.upCount("svc.yml"))
	}
	// LZSVC-1: the running-but-unhealthy stack MUST be torn down.
	if fo.downCount("svc.yml") != 1 {
		t.Fatalf("orphaned stack (LZSVC-1): Up succeeded but health failed and the "+
			"stack was NOT Down'd (Down calls = %d, want 1). startServiceInternal "+
			"returns before setting started=true, so StopService/StopAll never "+
			"clean it up — the containers leak.", fo.downCount("svc.yml"))
	}
	// The service must NOT be flagged started.
	lo.mu.RLock()
	started := lo.started["svc"]
	lo.mu.RUnlock()
	if started {
		t.Error("a health-failed service must not report started=true")
	}

	// StopAll must leave nothing running and must NOT double-Down the
	// already-torn-down stack (started is false, so it is skipped).
	if err := lo.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll error: %v", err)
	}
	if fo.downCount("svc.yml") != 1 {
		t.Fatalf("StopAll perturbed the torn-down stack: Down calls = %d, want 1", fo.downCount("svc.yml"))
	}
}

// ---------------------------------------------------------------------------
// LZSVC-2 — caller ctx ignored
// ---------------------------------------------------------------------------

// ctxBlockingUpOrchestrator is a compose orchestrator whose Up blocks until its
// context is done, letting the test observe whether the caller's ctx threads
// into the boot. Permitted in unit-test sources only.
type ctxBlockingUpOrchestrator struct {
	upEntered chan struct{}
	once      sync.Once
}

func (o *ctxBlockingUpOrchestrator) Up(ctx context.Context, _ compose.ComposeProject, _ ...compose.UpOption) error {
	o.once.Do(func() { close(o.upEntered) })
	<-ctx.Done()
	return ctx.Err()
}

func (o *ctxBlockingUpOrchestrator) Down(context.Context, compose.ComposeProject, ...compose.DownOption) error {
	return nil
}

func (o *ctxBlockingUpOrchestrator) Status(context.Context, compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (o *ctxBlockingUpOrchestrator) Logs(context.Context, compose.ComposeProject, string) (io.ReadCloser, error) {
	return nil, nil
}

// TestLazyOrchestrator_StartService_CallerCtxCancelAborts asserts that
// cancelling the ctx passed to StartService aborts an in-flight boot promptly.
// The boot's compose Up blocks on its context; once the caller cancels, a boot
// that correctly derives its context FROM the caller returns right away.
//
// RED (LZSVC-2 fix reverted): startServiceInternal builds its timeout from
// context.Background(), so the caller cancel has ZERO effect and Up blocks until
// StartTimeout — the probe budget elapses. GREEN (fixed): returns within the
// budget with a context error.
func TestLazyOrchestrator_StartService_CallerCtxCancelAborts(t *testing.T) {
	o := &ctxBlockingUpOrchestrator{upEntered: make(chan struct{})}
	lo := newTestOrchestrator(t, o, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{
		Name:        "svc",
		ComposeFile: "svc.yml",
		// Comfortably larger than the probe budget so the pre-fix path (which
		// ignores the caller cancel) is clearly distinguishable; also bounds
		// the reverted-RED run's background goroutine.
		StartTimeout: 3 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- lo.StartService(ctx, "svc") }()

	// Synchronize on Up actually blocking, THEN cancel the caller ctx.
	select {
	case <-o.upEntered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("compose Up was never invoked by StartService")
	}
	cancel()

	const probeBudget = 1 * time.Second
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("StartService returned nil after its caller ctx was cancelled mid-boot")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartService error does not carry the caller cancellation: %v", err)
		}
		if elapsed >= 3*time.Second {
			t.Fatalf("StartService took %v — it ran to StartTimeout rather than "+
				"aborting on the caller cancel (LZSVC-2)", elapsed)
		}
	case <-time.After(probeBudget):
		t.Fatalf("StartService ignored its caller ctx cancellation: still running "+
			">%v after cancel while compose Up blocks on a context NOT derived "+
			"from the caller (LZSVC-2 — caller ctx not threaded into the boot)", probeBudget)
	}
}

// ---------------------------------------------------------------------------
// LZSVC-3 — failed boot reports Started=true
// ---------------------------------------------------------------------------

// TestLazyOrchestrator_GetServiceStatus_FailedBootReportsNotStarted asserts
// that after a boot that fails (compose Up errors), GetServiceStatus reports
// Started=false together with a non-empty LastError.
//
// RED (LZSVC-3 fix reverted): status.Started = booter.Started(), which is true
// once the boot ATTEMPT settles (even on failure) → Started=true while
// LastError!=nil, a contradictory status. GREEN (fixed): status.Started reads
// lo.started[name], which stays false on a failed boot.
func TestLazyOrchestrator_GetServiceStatus_FailedBootReportsNotStarted(t *testing.T) {
	fo := newFakeOrchestrator()
	fo.upErr["svc.yml"] = errors.New("compose up boom")
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "svc", ComposeFile: "svc.yml"}); err != nil {
		t.Fatal(err)
	}

	if err := lo.StartService(context.Background(), "svc"); err == nil {
		t.Fatal("expected StartService to fail when compose Up errors")
	}

	st, err := lo.GetServiceStatus("svc")
	if err != nil {
		t.Fatal(err)
	}
	if st.Started {
		t.Fatal("a failed boot must report Started=false (LZSVC-3): GetServiceStatus " +
			"read booter.Started() — true after the attempt settled — instead of the " +
			"orchestrator's own success flag lo.started[name]")
	}
	if st.LastError == "" {
		t.Fatal("a failed boot must report a non-empty LastError")
	}
}

// ---------------------------------------------------------------------------
// LZSVC-4 — spurious health timeout
// ---------------------------------------------------------------------------

// TestLazyOrchestrator_StartService_ImmediateHealthNoSpuriousTimeout asserts
// that an instantly-healthy service with StartTimeout<2s starts successfully
// and quickly (an immediate health probe runs before the 2s ticker loop).
//
// RED (LZSVC-4 fix reverted): waitForHealth only probes inside `case
// <-ticker.C` (first tick at 2s), so with StartTimeout=500ms the boot ctx
// deadline fires first → StartService returns a timeout error. GREEN (fixed):
// the immediate probe returns healthy → StartService succeeds well under 2s.
func TestLazyOrchestrator_StartService_ImmediateHealthNoSpuriousTimeout(t *testing.T) {
	fo := newFakeOrchestrator()
	hc := &fakeHealthChecker{healthy: true}
	lo := newTestOrchestrator(t, fo, hc)
	if err := lo.RegisterService(&ServiceDefinition{
		Name:         "svc",
		ComposeFile:  "svc.yml",
		HealthCheck:  &health.HealthTarget{Name: "svc", Host: "127.0.0.1", Port: "1"},
		StartTimeout: 500 * time.Millisecond, // < the 2s ticker period
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := lo.StartService(context.Background(), "svc")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("an instantly-healthy service with StartTimeout<2s must start, got: %v "+
			"(LZSVC-4 — no immediate health probe, so it timed out before the first "+
			"2s ticker tick)", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("StartService took %v; an immediate health probe must return well "+
			"under the 2s ticker period (LZSVC-4)", elapsed)
	}
	if hc.callCount() == 0 {
		t.Error("health checker was never invoked")
	}
	st, err := lo.GetServiceStatus("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started {
		t.Error("service should report Started after an immediate healthy probe")
	}
}
