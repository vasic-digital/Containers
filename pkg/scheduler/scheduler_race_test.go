package scheduler

import (
	"context"
	"sync"
	"testing"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// TestDefaultScheduler_ConcurrentSchedule_Race exercises concurrent
// Schedule/ScheduleBatch calls against a single shared DefaultScheduler
// using StrategySpread — the strategy that reads the scheduler's
// internal placements map (scheduleSpread in strategies.go sorts hosts
// by `existing[host]`).
//
// Second-pass concurrency audit finding: DefaultScheduler.placements was
// mutated (`s.placements[decision.HostName]++`) by Schedule and
// ScheduleBatch with no mutex guarding it, while scheduleSpread reads
// that same map concurrently inside a sort.Slice comparator. That is a
// textbook concurrent map read+write on a *DefaultScheduler shared
// across goroutines — the normal way a Scheduler is used (one
// long-lived instance driving concurrent placement requests from a
// distribution/orchestration layer, mirroring HostManager/ConnectionPool/
// EventBus/LazyOrchestrator, all of which mutex-guard their shared maps
// in this same module). Go's runtime can fatal with "concurrent map
// read and map write" here even without -race.
//
// Run with `go test -race`: fails on the unguarded version, passes once
// DefaultScheduler.mu guards every access to placements.
func TestDefaultScheduler_ConcurrentSchedule_Race(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "h1", Address: "10.0.0.1", User: "u"})
	_ = mgr.AddHost(remote.RemoteHost{Name: "h2", Address: "10.0.0.2", User: "u"})
	_ = mgr.AddHost(remote.RemoteHost{Name: "h3", Address: "10.0.0.3", User: "u"})
	mgr.snapshots["local"] = makeSnapshot("local", 10, 10, 16384, 500000, 8)
	mgr.snapshots["h1"] = makeSnapshot("h1", 10, 10, 16384, 500000, 8)
	mgr.snapshots["h2"] = makeSnapshot("h2", 10, 10, 16384, 500000, 8)
	mgr.snapshots["h3"] = makeSnapshot("h3", 10, 10, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{}, WithStrategy(StrategySpread))
	ctx := context.Background()

	const workers = 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = sched.Schedule(ctx, ContainerRequirements{Name: "single"})
		}()
		go func() {
			defer wg.Done()
			_, _ = sched.ScheduleBatch(ctx, []ContainerRequirements{
				{Name: "batch-a"},
				{Name: "batch-b"},
			})
		}()
	}
	wg.Wait()
}
