package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// TestScheduleSpread_LocalRespectsRequiredLabels proves the spread strategy
// no longer force-includes the unlabelled local host in a labelled request.
// The local host carries no labels, so a request requiring zone=us must not
// land on it; the only remote host is in zone=eu and is correctly skipped, so
// NO host qualifies. Pre-fix, scheduleSpread seeded allNames with localName
// unconditionally, so a zone=us container was silently placed on a host that
// is not in zone=us — breaking the zone guarantee (DEFECT-1). The sibling
// strategies (resource-aware / bin-pack / round-robin) are guarded by
// TestScheduleResourceAware_LabelMismatch, TestScheduleBinPack_LabelMismatch
// and TestScheduleRoundRobin_LabelFiltering.
func TestScheduleSpread_LocalRespectsRequiredLabels(t *testing.T) {
	hosts := []remote.RemoteHost{
		{Name: "eu-host", Labels: map[string]string{"zone": "eu"}},
	}
	req := ContainerRequirements{
		Name:   "needs-us",
		Labels: map[string]string{"zone": "us"},
	}
	decision := scheduleSpread(nil, hosts, req, "local", map[string]int{})
	if decision.HostName != "" {
		t.Fatalf("labelled request (zone=us) landed on host %q via spread; the "+
			"unlabelled local host must not satisfy a labelled request",
			decision.HostName)
	}
}

// TestDefaultScheduler_SelectedHostNeverReportsZeroScore proves a genuinely
// selected host never carries Score==0. The distribution layer
// (pkg/distribution/distributor.go) treats Score==0 as the "no host found"
// sentinel and drops the container as a failed placement. A fully
// resource-saturated host still passes CanFit for a bare request (no
// CPU/mem/disk/GPU minimums), so it IS selected — yet ResourceScorer.Score
// clamps to exactly 0 on such a host. Pre-fix the decision carried Score==0
// and the container was silently dropped (DEFECT-2).
func TestDefaultScheduler_SelectedHostNeverReportsZeroScore(t *testing.T) {
	mgr := newMockHostManager()
	// Saturated on every scored axis: CPU/mem/disk at 100% and network at the
	// 1 Gbps normalisation ceiling, so every sub-score is 0 and the weighted
	// total clamps to 0.
	mgr.snapshots["local"] = &remote.HostResources{
		Host:                 "local",
		CPUPercent:           100,
		CPUCores:             8,
		MemoryPercent:        100,
		MemoryTotalMB:        16384,
		DiskPercent:          100,
		DiskTotalMB:          100000,
		NetworkRxBytesPerSec: 125_000_000,
	}

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyResourceAware),
	)

	decision, err := sched.Schedule(context.Background(),
		ContainerRequirements{Name: "any"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, decision.HostName,
		"a host WAS selectable (CanFit true for a bare request); the scheduler "+
			"must not report no-host")
	if decision.Score == 0 {
		t.Fatalf("selected host %q reported Score==0; pkg/distribution treats "+
			"that as the no-host sentinel and silently drops the container as "+
			"a failed placement", decision.HostName)
	}
}

// TestDefaultScheduler_Spread_ReleaseDecrementsPlacements proves Release()
// actually decrements the per-host placement counter that StrategySpread reads
// to pick the least-loaded host. Pre-fix the counter was write-only (never
// decremented), so a drained host was avoided forever (DEFECT-3).
func TestDefaultScheduler_Spread_ReleaseDecrementsPlacements(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "h1", Address: "10.0.0.1", User: "u",
	})
	mgr.snapshots["local"] = makeSnapshot("local", 20, 20, 16384, 500000, 8)
	mgr.snapshots["h1"] = makeSnapshot("h1", 20, 20, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategySpread),
	)

	// First placement lands on local (both hosts have 0 existing; local is the
	// first candidate). placements[local] -> 1.
	d1, err := sched.Schedule(context.Background(),
		ContainerRequirements{Name: "a"},
	)
	require.NoError(t, err)
	require.Equal(t, "local", d1.HostName)

	// Drain local. With Release working, placements[local] -> 0.
	sched.Release("local")

	// Next placement sees local and h1 both at 0 again, so spread returns to
	// local. If Release were a no-op, placements[local] would still be 1 and
	// spread would steer this container to h1 instead.
	d2, err := sched.Schedule(context.Background(),
		ContainerRequirements{Name: "b"},
	)
	require.NoError(t, err)
	assert.Equal(t, "local", d2.HostName,
		"Release did not decrement the placement counter; spread avoided the "+
			"drained local host")
}

// TestDefaultScheduler_RoundRobin_CounterIsPerInstance proves two independent
// schedulers do not share one round-robin rotation. Each scheduler has the
// candidate list [local, h1]; a fresh scheduler's FIRST placement must be
// index 0 -> "local". Pre-fix the counter was a package-level global, so one
// scheduler's placement advanced every other scheduler's rotation, and the
// second scheduler's first placement started at a shifted index (DEFECT-4).
func TestDefaultScheduler_RoundRobin_CounterIsPerInstance(t *testing.T) {
	newSched := func() *DefaultScheduler {
		mgr := newMockHostManager()
		_ = mgr.AddHost(remote.RemoteHost{
			Name: "h1", Address: "10.0.0.1", User: "u",
		})
		mgr.snapshots["local"] = makeSnapshot("local", 20, 20, 16384, 500000, 8)
		return NewScheduler(mgr, logging.NopLogger{},
			WithStrategy(StrategyRoundRobin),
		)
	}

	a := newSched()
	b := newSched()

	// a's first placement advances a's counter (0 -> 1). With a shared global
	// counter this also advances b's rotation.
	da1, err := a.Schedule(context.Background(),
		ContainerRequirements{Name: "x"},
	)
	require.NoError(t, err)
	require.Equal(t, "local", da1.HostName)

	// b's FIRST placement must still be index 0 -> local (its own counter).
	db1, err := b.Schedule(context.Background(),
		ContainerRequirements{Name: "y"},
	)
	require.NoError(t, err)
	if db1.HostName != "local" {
		t.Fatalf("second scheduler's first round-robin placement was %q, not "+
			"local (index 0); the round-robin counter is shared across "+
			"scheduler instances", db1.HostName)
	}
}
