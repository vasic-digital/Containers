// Package lazyservice lock-contention regression test (CT-HARDEN-71).
//
// Reproduces a dev-principle-#2 violation (constitution/CLAUDE.md "No
// blocking operations inside synchronized / shared-lock regions";
// §11.4.102 / §11.4.118): StopService() acquires lo.mu's WRITE lock and,
// via `defer lo.mu.Unlock()`, holds it across the blocking
// lo.orchestrator.Down(...) call — an external `docker/podman compose down`
// process bounded only by svc.StopTimeout (default 30s). While that Down is
// in flight, EVERY other method that takes lo.mu (GetServiceStatus /
// ListServices / StartService — RLock; StopAll / RegisterService — Lock)
// stalls for the whole Down duration.
//
// This is a deterministic lock-contention gate (§11.4.115 polarity): a
// concurrent reader (GetServiceStatus) is probed WHILE a blocking Down is
// executing. On the pre-fix tree the reader is stalled behind the held
// write lock (RED). Once StopService snapshots the needed state under the
// lock, RELEASES the lock, and only THEN calls the blocking Down, the reader
// returns promptly while Down is still in flight (GREEN).
package lazyservice

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/compose"
)

// blockingOrchestrator is a compose.ComposeOrchestrator whose Down blocks
// until released, letting the test observe whether StopService holds lo.mu
// across the (blocking) Down. Permitted in unit-test sources only.
type blockingOrchestrator struct {
	downEntered chan struct{} // closed when Down begins executing
	releaseDown chan struct{} // Down returns once this is closed
	downOnce    sync.Once
}

func (b *blockingOrchestrator) Up(context.Context, compose.ComposeProject, ...compose.UpOption) error {
	return nil
}

func (b *blockingOrchestrator) Down(ctx context.Context, _ compose.ComposeProject, _ ...compose.DownOption) error {
	b.downOnce.Do(func() { close(b.downEntered) })
	select {
	case <-b.releaseDown:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingOrchestrator) Status(context.Context, compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (b *blockingOrchestrator) Logs(context.Context, compose.ComposeProject, string) (io.ReadCloser, error) {
	return nil, nil
}

func TestLazyOrchestrator_StopService_DoesNotHoldLockDuringDown(t *testing.T) {
	bo := &blockingOrchestrator{
		downEntered: make(chan struct{}),
		releaseDown: make(chan struct{}),
	}
	lo := newTestOrchestrator(t, bo, &fakeHealthChecker{})
	if err := lo.RegisterService(&ServiceDefinition{
		Name:        "svc",
		ComposeFile: "svc.yml",
		// Realistic bound: Down is allowed to run up to StopTimeout, and the
		// defect holds lo.mu for that entire window.
		StopTimeout: 30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if err := lo.StartService(context.Background(), "svc"); err != nil {
		t.Fatal(err)
	}

	// G1: StopService — its Down blocks until releaseDown is closed.
	stopDone := make(chan error, 1)
	go func() { stopDone <- lo.StopService(context.Background(), "svc") }()

	// Wait until Down is actually executing. On the pre-fix tree lo.mu's
	// WRITE lock is held RIGHT NOW, across this blocking Down.
	select {
	case <-bo.downEntered:
	case <-time.After(5 * time.Second):
		close(bo.releaseDown)
		t.Fatal("compose Down was never invoked by StopService")
	}

	// G2: a concurrent reader. GetServiceStatus takes lo.mu.RLock(). If
	// StopService holds the write lock across Down (the defect), this RLock
	// blocks for the whole Down duration.
	statusDone := make(chan struct{})
	go func() {
		_, _ = lo.GetServiceStatus("svc")
		close(statusDone)
	}()

	const probeBudget = 2 * time.Second
	blocked := false
	select {
	case <-statusDone:
		// Reader returned promptly while Down is still in flight — lo.mu is
		// NOT held across Down. GREEN.
	case <-time.After(probeBudget):
		// Reader is stalled behind the write lock held across the blocking
		// Down — dev-principle #2 violation. RED.
		blocked = true
	}

	// Release Down and drain both goroutines regardless of outcome.
	close(bo.releaseDown)
	<-statusDone
	if err := <-stopDone; err != nil {
		t.Fatalf("StopService returned error: %v", err)
	}

	if blocked {
		t.Fatalf("GetServiceStatus stalled >%v while StopService held lo.mu "+
			"across a blocking compose Down: StopService holds the write lock "+
			"across lo.orchestrator.Down (dev-principle #2 — no blocking op "+
			"inside a held lock; constitution §11.4.102/§11.4.118). Fix: "+
			"snapshot state under the lock, release it, then call Down.",
			probeBudget)
	}
}
