// Package lazyservice restart-after-stop regression test.
//
// Reproduces a lifecycle-layer PASS-bluff (constitution/Constitution.md
// §11.4 / §11.4.108): StopService() calls compose Down and clears the
// started flag, but never resets the one-shot lifecycle.LazyBooter that
// StartService() drives. A subsequent StartService() therefore hits the
// booter's cached-success fast path, invokes NO compose Up, leaves the
// service genuinely stopped, yet returns nil — fabricating success for a
// service that was never restarted.
//
// This is an outcome/counter test (a logic defect, not a data race): the
// proof is that compose Up is invoked exactly once across Start→Stop→Start
// on the pre-fix tree instead of the expected twice (§11.4.115 polarity —
// RED on the broken tree, GREEN once StopService resets the booter).
package lazyservice

import (
	"context"
	"testing"
)

func TestLazyOrchestrator_StartService_AfterStop_ActuallyRestarts(t *testing.T) {
	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{Name: "svc", ComposeFile: "svc.yml"}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// First start: real boot, one compose Up.
	if err := lo.StartService(ctx, "svc"); err != nil {
		t.Fatalf("first StartService error: %v", err)
	}
	if got := fo.upCount("svc.yml"); got != 1 {
		t.Fatalf("after first start: Up calls = %d, want 1", got)
	}

	// Stop: one compose Down, started flag cleared.
	if err := lo.StopService(ctx, "svc"); err != nil {
		t.Fatalf("StopService error: %v", err)
	}
	if got := fo.downCount("svc.yml"); got != 1 {
		t.Fatalf("after stop: Down calls = %d, want 1", got)
	}

	// Second start: MUST actually restart the service, i.e. invoke compose
	// Up a second time. On the pre-fix tree the one-shot booter short-
	// circuits and Up is NOT called again, so StartService returns a
	// fabricated success for a service that is still down.
	if err := lo.StartService(ctx, "svc"); err != nil {
		t.Fatalf("second StartService error: %v", err)
	}
	if got := fo.upCount("svc.yml"); got != 2 {
		t.Fatalf("restart did not re-run compose Up: Up calls = %d, want 2 "+
			"(StopService failed to reset the LazyBooter — §11.4 fabricated-"+
			"success restart no-op)", got)
	}

	// And the reported status must reflect a genuinely running service.
	st, err := lo.GetServiceStatus("svc")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started {
		t.Error("service must report Started=true after a successful restart")
	}
}
