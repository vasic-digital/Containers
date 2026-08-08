package distribution

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// TestExecutorConfiguredWithoutHostManager_NoDeref is the §11.4.115 guard for
// CT-HARDEN-DIST-1: a caller wiring WithExecutor without WithHostManager (two
// independently-settable Options) must not crash Distribute() on the first
// remote placement. Pre-fix, deployRemote called d.opts.HostManager.GetHost on
// a nil interface -> panic. Fixed: an explicit "no host manager configured"
// error, mirroring HostStatus()'s precedent.
//
//	RED_MODE default "1": assert the misconfiguration is handled safely.
//	RED_MODE=0: forensic reproduce — assert the panic actually fires (pre-fix).
func TestExecutorConfiguredWithoutHostManager_NoDeref(t *testing.T) {
	wantSafe := os.Getenv("RED_MODE") != "0" // default "1"

	var (
		summary  *DistributionSummary
		err      error
		panicked bool
		panicVal interface{}
	)

	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicVal = r
			}
		}()

		dist := NewDistributor(
			WithScheduler(&mockScheduler{
				batchFunc: func(
					ctx context.Context,
					reqs []scheduler.ContainerRequirements,
				) (*scheduler.PlacementPlan, error) {
					decisions := make(
						[]scheduler.PlacementDecision, len(reqs),
					)
					for i, req := range reqs {
						decisions[i] = scheduler.PlacementDecision{
							Requirement: req,
							HostName:    "remote-1",
							Score:       0.7,
							Reason:      "test",
						}
					}
					return &scheduler.PlacementPlan{
						Decisions:     decisions,
						HostSnapshots: map[string]*remote.HostResources{},
					}, nil
				},
			}),
			WithExecutor(&mockExecutor{}),
			// WithHostManager intentionally NOT called: the misconfiguration.
			WithLogger(logging.NopLogger{}),
		)

		summary, err = dist.Distribute(
			context.Background(),
			[]scheduler.ContainerRequirements{
				{Name: "app-1", Image: "nginx"},
			},
		)
	}()

	if wantSafe {
		if panicked {
			t.Fatalf(
				"CT-HARDEN-DIST-1: Executor configured without HostManager "+
					"still panics instead of failing honestly: %v",
				panicVal,
			)
		}
		require.NoError(t, err)
		require.NotNil(t, summary)
		assert.Equal(t, 1, summary.TotalContainers)
		assert.Equal(t, 0, summary.RemoteContainers)
		assert.Equal(
			t, 1, summary.FailedContainers,
			"misconfigured deploy must surface as a FAILED container, "+
				"not a crash and not a silent success",
		)
		require.Len(t, summary.Containers, 1)
		assert.Equal(t, StateFailed, summary.Containers[0].State)
		assert.Contains(t, summary.Containers[0].Error, "host manager")
		return
	}

	if !panicked {
		t.Fatalf(
			"RED_MODE=0 reproduce: expected the nil-HostManager panic " +
				"but none occurred (already fixed?)",
		)
	}
}

// TestHealthCheckAll_ExecutorConfiguredWithoutHostManager_NoDeref is the
// §11.4.115 guard for the HealthCheckAll half of CT-HARDEN-DIST-1: an
// unverifiable running remote container (Executor set, HostManager nil) must
// surface an honest "host manager not configured" error, never a crash and
// never a silent healthy result (§11.4.69).
func TestHealthCheckAll_ExecutorConfiguredWithoutHostManager_NoDeref(t *testing.T) {
	wantSafe := os.Getenv("RED_MODE") != "0" // default "1"

	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithExecutor(&mockExecutor{}),
		// WithHostManager intentionally NOT called.
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(
		context.Background(),
		[]scheduler.ContainerRequirements{{Name: "local-app"}},
	)
	require.NoError(t, err)

	dist.mu.Lock()
	dist.containers = append(dist.containers, DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "remote-app"},
		HostName:    "remote-1",
		State:       StateRunning,
	})
	dist.mu.Unlock()

	var (
		panicked bool
		panicVal interface{}
		errs     map[string]error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicVal = r
			}
		}()
		errs = dist.HealthCheckAll(context.Background())
	}()

	if wantSafe {
		if panicked {
			t.Fatalf(
				"CT-HARDEN-DIST-1: HealthCheckAll panics on a nil "+
					"HostManager instead of failing honestly: %v",
				panicVal,
			)
		}
		require.Contains(t, errs, "remote-app")
		assert.Contains(
			t, errs["remote-app"].Error(), "host manager not configured",
			"unverifiable reachability must surface as an honest error, "+
				"not a silent healthy/no-error result (§11.4.69)",
		)
		return
	}

	if !panicked {
		t.Fatalf(
			"RED_MODE=0 reproduce: expected the nil-HostManager panic " +
				"but none occurred (already fixed?)",
		)
	}
}
