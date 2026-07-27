//go:build stress

package stress

import (
	"context"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/distribution"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDistribution_ConcurrentStatus verifies that 50 goroutines
// can call Distributor.Status concurrently without race conditions
// or panics.
func TestDistribution_ConcurrentStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode") // SKIP-OK: #short-mode
	}

	dist := distribution.NewDistributor(
		distribution.WithLogger(logging.NopLogger{}),
	)
	require.NotNil(t, dist)

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errors := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Each goroutine calls Status multiple times.
			for j := 0; j < 100; j++ {
				status := dist.Status(ctx)
				if status == nil {
					errors <- "status returned nil"
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for errMsg := range errors {
		t.Errorf("concurrent status error: %s", errMsg)
	}
}

// TestScheduler_ConcurrentSchedule verifies that 20 goroutines
// can call ScheduleBatch simultaneously without race conditions.
func TestScheduler_ConcurrentSchedule(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode") // SKIP-OK: #short-mode
	}

	executor, err := remote.NewSSHExecutor(
		logging.NopLogger{},
		remote.WithControlMaster(false),
	)
	require.NoError(t, err)
	defer executor.Close()

	hm := remote.NewHostManager(executor, logging.NopLogger{})
	sched := scheduler.NewScheduler(
		hm, logging.NopLogger{},
	)
	require.NotNil(t, sched)

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			reqs := []scheduler.ContainerRequirements{
				{
					Name:     "stress-svc",
					Image:    "alpine:latest",
					CPUCores: 0.5,
					MemoryMB: 128,
				},
			}

			plan, schedErr := sched.ScheduleBatch(ctx, reqs)
			if schedErr != nil {
				errs[id] = schedErr
				return
			}
			if plan == nil {
				t.Errorf(
					"goroutine %d: plan should not be nil",
					id,
				)
			}
		}(i)
	}

	wg.Wait()

	for i, schedErr := range errs {
		assert.NoError(t, schedErr,
			"goroutine %d should not error", i)
	}
}
