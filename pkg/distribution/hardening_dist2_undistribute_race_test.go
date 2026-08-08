package distribution

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/scheduler"
)

// TestConcurrentUndistributeAndHealthCheck_NoDataRace is the §11.4.115 guard for
// CT-HARDEN-DIST-2: Undistribute() mutated containers[i].State AFTER releasing
// d.mu, and HealthCheckAll() read the same aliased backing array AFTER releasing
// its RLock — the "unlock then keep touching shared memory" anti-pattern.
// `go test -race` is the binary oracle: it deterministically FAILs against the
// pre-fix source and PASSes once Undistribute mutates under the lock and
// HealthCheckAll copies-under-lock (mirroring Status()). No RED_MODE needed.
func TestConcurrentUndistributeAndHealthCheck_NoDataRace(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := make([]scheduler.ContainerRequirements, 300)
	for i := range reqs {
		reqs[i] = scheduler.ContainerRequirements{
			Name: fmt.Sprintf("app-%d", i),
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = dist.Distribute(context.Background(), reqs)
			_ = dist.Undistribute(context.Background())
		}
	}()

	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = dist.HealthCheckAll(context.Background())
			_ = dist.Status(context.Background())
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestReturnedSummaryIsIndependentCopy_NotAliased is the DETERMINISTIC §11.4.115
// guard for the escaped-summary half of CT-HARDEN-DIST-2: Distribute() published
// the SAME backing array to both summary.Containers (returned to the caller) and
// d.containers (mutated by Undistribute under d.mu). Pre-fix, a caller holding a
// returned summary would see its Containers[i].State retroactively flipped to
// StateStopped the moment Undistribute() ran — a shared-mutable-aliasing footgun
// AND (for a concurrent reader) a data race. No goroutines/scheduler luck needed:
// this asserts the copy-on-publish invariant directly. Pre-fix (aliased) the
// snapshot diverges -> FAIL; post-fix (independent copy) it holds -> PASS.
func TestReturnedSummaryIsIndependentCopy_NotAliased(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLogger(logging.NopLogger{}),
	)
	reqs := []scheduler.ContainerRequirements{{Name: "app-1"}, {Name: "app-2"}}

	summary, err := dist.Distribute(context.Background(), reqs)
	if err != nil {
		t.Fatalf("Distribute: %v", err)
	}
	if len(summary.Containers) == 0 {
		t.Fatalf("expected a non-empty summary")
	}

	// Snapshot the States the caller observes at return time.
	before := make([]DistributionState, len(summary.Containers))
	for i := range summary.Containers {
		before[i] = summary.Containers[i].State
	}

	// Undistribute flips every d.containers[i].State to StateStopped.
	if err := dist.Undistribute(context.Background()); err != nil {
		t.Fatalf("Undistribute: %v", err)
	}

	// The previously-returned summary MUST be unchanged (independent copy) —
	// Undistribute must never reach back and mutate a summary the caller holds.
	for i := range summary.Containers {
		if summary.Containers[i].State != before[i] {
			t.Fatalf("CT-HARDEN-DIST-2: summary.Containers[%d].State was "+
				"retroactively mutated by Undistribute (before=%v after=%v) — "+
				"the returned summary aliases d.containers instead of being a copy",
				i, before[i], summary.Containers[i].State)
		}
	}
}
