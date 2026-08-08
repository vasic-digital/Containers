package scheduler

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	"digital.vasic.containers/pkg/remote"
)

// TestScore_NilResources2 re-confirms that Score with nil returns 0.0.
// (Complements TestResourceScorer_Score_NilResources in scorer_test.go by
// exercising the same branch via a differently named test to be safe.)
func TestScore_NilResources2(t *testing.T) {
	s := NewResourceScorer(DefaultSchedulerOptions())
	result := s.Score(nil, ContainerRequirements{CPUCores: 2, MemoryMB: 512})
	assert.Equal(t, 0.0, result)
}

// TestCanFit_NilResources2 exercises the nil branch in CanFit.
func TestCanFit_NilResources2(t *testing.T) {
	s := NewResourceScorer(DefaultSchedulerOptions())
	result := s.CanFit(nil, ContainerRequirements{CPUCores: 1})
	assert.False(t, result)
}

// TestCanFit_CPUInsufficient exercises the CPU insufficient branch in CanFit.
// The host has 4 cores at 90% usage → only 10% = 0.4 cores available.
// Requesting 2 cores must return false.
func TestCanFit_CPUInsufficient(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 0 // remove reserve so math is simpler
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		CPUPercent: 90, // 10% available
		CPUCores:   4,
	}
	// availCores = 4 * (10.0/100) * 1.0 = 0.4, req = 2 → cannot fit
	fit := s.CanFit(res, ContainerRequirements{CPUCores: 2})
	assert.False(t, fit)
}

// TestCanFit_MemoryInsufficient exercises the memory insufficient branch.
func TestCanFit_MemoryInsufficient(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 0
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 95, // only 5% free
		MemoryTotalMB: 4096,
	}
	// availMB = 4096 * 0.05 * 1.0 = 204.8, req = 8192 → cannot fit
	fit := s.CanFit(res, ContainerRequirements{MemoryMB: 8192})
	assert.False(t, fit)
}

// TestCanFit_DiskInsufficient exercises the disk insufficient branch.
func TestCanFit_DiskInsufficient(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 0
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		DiskPercent: 99,
		DiskTotalMB: 500000,
	}
	// availDiskMB = 500000 * 0.01 = 5000, req = 10000 → cannot fit
	fit := s.CanFit(res, ContainerRequirements{DiskMB: 10000})
	assert.False(t, fit)
}

// TestCanFit_AllFit exercises the all-resources-fit happy path.
func TestCanFit_AllFit(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 0
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		CPUPercent:    10,
		CPUCores:      16,
		MemoryPercent: 20,
		MemoryTotalMB: 32768,
		DiskPercent:   30,
		DiskTotalMB:   1000000,
	}
	fit := s.CanFit(res, ContainerRequirements{
		CPUCores: 2,
		MemoryMB: 4096,
		DiskMB:   10000,
	})
	assert.True(t, fit)
}

// TestScoreMemory_ZeroAvail exercises the avail <= 0 branch in scoreMemory
// (MemoryPercent = 100, so avail = 0).
func TestScoreMemory_ZeroAvail(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 100,
		MemoryTotalMB: 16384,
	}
	score := s.scoreMemory(res, ContainerRequirements{}, 0, 1.0)
	assert.Equal(t, 0.0, score)
}

// TestScoreMemory_WithRequest_Fits exercises the branch where MemoryMB is
// requested and the host has enough memory.
func TestScoreMemory_WithRequest_Fits(t *testing.T) {
	opts := DefaultSchedulerOptions()
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 10, // 90% free
		MemoryTotalMB: 32768,
	}
	// availMB = 32768 * (80/100) * 1.0 = 26214.4 (with 10% reserve),
	// requesting only 512 — should fit → score > 0.
	score := s.scoreMemory(res, ContainerRequirements{MemoryMB: 512}, 10.0, 1.0)
	assert.Greater(t, score, 0.0)
}

// TestScoreMemory_WithRequest_TooLarge exercises the branch where MemoryMB
// is requested but the host does not have enough → score = 0.
func TestScoreMemory_WithRequest_TooLarge(t *testing.T) {
	opts := DefaultSchedulerOptions()
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 90,  // only 10% free
		MemoryTotalMB: 512, // 512 MB total
	}
	// availMB = 512 * (0%  free after reserve) ≈ 0 < 8192 → score = 0.
	score := s.scoreMemory(res, ContainerRequirements{MemoryMB: 8192}, 10.0, 1.0)
	assert.Equal(t, 0.0, score)
}

// TestScoreMemory_ZeroTotal exercises the MemoryTotalMB = 0 branch: when
// MemoryMB is requested but total is 0, the req branch is skipped because
// `r.MemoryTotalMB > 0` is false.
func TestScoreMemory_ZeroTotal(t *testing.T) {
	opts := DefaultSchedulerOptions()
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 50,
		MemoryTotalMB: 0, // no total info
	}
	// avail = 50%; because MemoryTotalMB == 0 the "if req.MemoryMB > 0
	// && r.MemoryTotalMB > 0" branch is not taken → score = avail/100.
	score := s.scoreMemory(res, ContainerRequirements{MemoryMB: 1024}, 0, 1.0)
	assert.InDelta(t, 0.5, score, 0.01)
}

// TestCanFit_ZeroRequirements ensures CanFit returns true when all
// requirements are zero (nothing to check).
func TestCanFit_ZeroRequirements(t *testing.T) {
	s := NewResourceScorer(DefaultSchedulerOptions())
	res := &remote.HostResources{
		CPUPercent:    50,
		MemoryPercent: 50,
		MemoryTotalMB: 8192,
		DiskPercent:   50,
		DiskTotalMB:   100000,
	}
	// Zero requirements → all checks skipped → true.
	fit := s.CanFit(res, ContainerRequirements{})
	assert.True(t, fit)
}

// TestCanFit_MemoryAvailNegative exercises the "availMB < 0" branch:
// when reserve > available percent, availMB becomes negative.
func TestCanFit_MemoryAvailNegative(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 80 // reserve 80% of capacity
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 50, // 50% used → 50% free → 50%-80% = -30% after reserve
		MemoryTotalMB: 4096,
	}
	// availMB will be negative → cannot fit
	fit := s.CanFit(res, ContainerRequirements{MemoryMB: 1})
	assert.False(t, fit)
}

// TestCanFit_DiskAvailNegative exercises the "availDiskMB < 0" branch.
func TestCanFit_DiskAvailNegative(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 90
	opts.OvercommitRatio = 1.0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		DiskPercent: 20, // 80% free − 90% reserve = negative
		DiskTotalMB: 10000,
	}
	fit := s.CanFit(res, ContainerRequirements{DiskMB: 1})
	assert.False(t, fit)
}

// TestScore_OvercommitZero exercises the "overcommit <= 0 → use 1.0" branch
// in Score.
func TestScore_OvercommitZero(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.OvercommitRatio = 0 // triggers: overcommit = 1.0
	opts.ReservePercent = 0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		CPUPercent:    20,
		MemoryPercent: 20,
		MemoryTotalMB: 16384,
		DiskPercent:   20,
		DiskTotalMB:   100000,
		CPUCores:      8,
	}
	score := s.Score(res, ContainerRequirements{})
	assert.Greater(t, score, 0.0)
}

// TestCanFit_OvercommitZero exercises the "overcommit <= 0 → use 1.0" branch
// in CanFit.
func TestCanFit_OvercommitZero(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.OvercommitRatio = -1 // triggers: overcommit = 1.0
	opts.ReservePercent = 0
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		CPUPercent:    10,
		CPUCores:      8,
		MemoryPercent: 10,
		MemoryTotalMB: 16384,
		DiskPercent:   10,
		DiskTotalMB:   100000,
	}
	fit := s.CanFit(res, ContainerRequirements{CPUCores: 1, MemoryMB: 512})
	assert.True(t, fit)
}

// TestScheduleResourceAware_PreferLocal exercises the PreferLocal branch
// in scheduleResourceAware.
func TestScheduleResourceAware_PreferLocal(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local": {
			CPUPercent:    10,
			CPUCores:      8,
			MemoryPercent: 10,
			MemoryTotalMB: 16384,
			DiskPercent:   10,
			DiskTotalMB:   100000,
		},
	}
	hosts := []remote.RemoteHost{} // no remote hosts

	req := ContainerRequirements{
		Name:        "pref-local",
		PreferLocal: true,
	}
	decision := scheduleResourceAware(scorer, snapshots, hosts, req, "local")
	assert.Equal(t, "local", decision.HostName)
	assert.Contains(t, decision.Reason, "preferred local")
}

// TestScheduleResourceAware_PreferLocal_NotAvailable exercises the case where
// PreferLocal is set but local does not have enough resources, falling through
// to the best remote host.
func TestScheduleResourceAware_PreferLocal_NotAvailable(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		// local is overloaded
		"local": {CPUPercent: 99, CPUCores: 1, MemoryPercent: 99, MemoryTotalMB: 1024, DiskPercent: 99, DiskTotalMB: 1000},
		// remote has capacity
		"remote-1": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
	}
	hosts := []remote.RemoteHost{
		{Name: "remote-1", Labels: map[string]string{}},
	}

	req := ContainerRequirements{
		Name:        "pref-local-fallback",
		PreferLocal: true,
		CPUCores:    1,   // local has only 1 core at 99% — cannot fit
		MemoryMB:    512, // local has only 1024MB at 99% — cannot fit
	}
	decision := scheduleResourceAware(scorer, snapshots, hosts, req, "local")
	// local cannot fit → falls through to best remote
	assert.Equal(t, "remote-1", decision.HostName)
}

// TestScheduleBinPack_NoCandidates exercises the "no host can fit" branch.
func TestScheduleBinPack_NoCandidates(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 99, CPUCores: 1, MemoryPercent: 99, MemoryTotalMB: 512, DiskPercent: 99, DiskTotalMB: 500},
	}
	hosts := []remote.RemoteHost{}

	req := ContainerRequirements{Name: "big", CPUCores: 100, MemoryMB: 999999}
	decision := scheduleBinPack(scorer, snapshots, hosts, req, "local")
	assert.Equal(t, "", decision.HostName)
	assert.Contains(t, decision.Reason, "no host can fit")
}

// TestScheduleBinPack_WithRemoteHost exercises the remote host path in
// scheduleBinPack.
func TestScheduleBinPack_WithRemoteHost(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 90, CPUCores: 1, MemoryPercent: 90, MemoryTotalMB: 512, DiskPercent: 90, DiskTotalMB: 500},
		"big":   {CPUPercent: 20, CPUCores: 16, MemoryPercent: 20, MemoryTotalMB: 65536, DiskPercent: 20, DiskTotalMB: 1000000},
	}
	hosts := []remote.RemoteHost{
		{Name: "big", Labels: map[string]string{}},
	}

	req := ContainerRequirements{Name: "needs-resources", CPUCores: 2}
	decision := scheduleBinPack(scorer, snapshots, hosts, req, "local")
	// "big" has highest utilization among fitting hosts
	assert.NotEmpty(t, decision.HostName)
}

// TestScheduleRoundRobin_LabelFiltering confirms label filtering in
// scheduleRoundRobin: the gpu-labelled host matches the required label and is
// selected, while the unlabelled local host is correctly EXCLUDED from a
// labelled request (the local host carries no labels, so it satisfies only
// label-free requests). gpu-host is therefore the sole candidate.
func TestScheduleRoundRobin_LabelFiltering(t *testing.T) {
	hosts := []remote.RemoteHost{
		{Name: "gpu-host", Labels: map[string]string{"gpu": "true"}},
	}
	req := ContainerRequirements{
		Name:   "needs-gpu",
		Labels: map[string]string{"gpu": "true"},
	}
	var counter atomic.Uint64
	decision := scheduleRoundRobin(hosts, req, "local", &counter)
	assert.Equal(t, "gpu-host", decision.HostName)
}

// TestScheduleSpread_SingleHost exercises scheduleSpread with only local.
func TestScheduleSpread_SingleHost(t *testing.T) {
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 30, MemoryPercent: 30},
	}
	hosts := []remote.RemoteHost{}
	req := ContainerRequirements{Name: "spread-test"}
	existing := map[string]int{"local": 3}
	decision := scheduleSpread(snapshots, hosts, req, "local", existing)
	assert.Equal(t, "local", decision.HostName)
}

// TestScheduleAffinity_LabelNotMatched exercises the case where no snapshot
// is found for a host that matches labels (ok=false branch in scheduleAffinity).
func TestScheduleAffinity_LabelNotMatched_NoSnapshot(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{} // no snapshots
	hosts := []remote.RemoteHost{
		{Name: "gpu-host", Labels: map[string]string{"gpu": "true"}},
	}
	req := ContainerRequirements{
		Name:   "affinity-no-snap",
		Labels: map[string]string{"gpu": "true"},
	}
	decision := scheduleAffinity(scorer, snapshots, hosts, req)
	assert.Equal(t, "", decision.HostName)
	assert.Contains(t, decision.Reason, "no host matches affinity")
}

// TestScheduleResourceAware_NoSnapshot_ForRemoteHost exercises the
// "snap not found" continue branch in scheduleResourceAware.
func TestScheduleResourceAware_NoSnapshot_ForRemoteHost(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	// local has a snapshot, but remote-1 has no snapshot.
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
	}
	hosts := []remote.RemoteHost{
		{Name: "remote-1", Labels: map[string]string{}}, // no snapshot
	}

	req := ContainerRequirements{Name: "test"}
	decision := scheduleResourceAware(scorer, snapshots, hosts, req, "local")
	// remote-1 skipped (no snapshot), local is chosen.
	assert.Equal(t, "local", decision.HostName)
}

// TestScheduleResourceAware_LabelMismatch exercises the label-mismatch
// continue branch in scheduleResourceAware.
func TestScheduleResourceAware_LabelMismatch(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local":    {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
		"gpu-host": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
	}
	hosts := []remote.RemoteHost{
		{Name: "gpu-host", Labels: map[string]string{"type": "cpu"}}, // has wrong label
	}

	req := ContainerRequirements{
		Name:   "needs-gpu",
		Labels: map[string]string{"type": "gpu"}, // mismatch
	}
	decision := scheduleResourceAware(scorer, snapshots, hosts, req, "local")
	// gpu-host skipped (label mismatch); local ALSO excluded — it carries no
	// labels, so it cannot satisfy the required type=gpu label either. No host
	// qualifies for a labelled request that nothing matches.
	assert.Empty(t, decision.HostName)
}

// TestScheduleResourceAware_CannotFitRemote exercises the CanFit false
// branch for remote hosts in scheduleResourceAware.
func TestScheduleResourceAware_CannotFitRemote(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
		"small": {CPUPercent: 99, CPUCores: 1, MemoryPercent: 99, MemoryTotalMB: 512, DiskPercent: 99, DiskTotalMB: 500},
	}
	hosts := []remote.RemoteHost{
		{Name: "small", Labels: map[string]string{}},
	}

	req := ContainerRequirements{
		Name:     "big-container",
		CPUCores: 4,
		MemoryMB: 8192,
	}
	decision := scheduleResourceAware(scorer, snapshots, hosts, req, "local")
	// small cannot fit → only local chosen.
	assert.Equal(t, "local", decision.HostName)
}

// TestScheduleBinPack_LabelMismatch exercises the label-mismatch branch
// in scheduleBinPack.
func TestScheduleBinPack_LabelMismatch(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local":  {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
		"remote": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
	}
	hosts := []remote.RemoteHost{
		{Name: "remote", Labels: map[string]string{"zone": "eu"}},
	}

	req := ContainerRequirements{
		Name:   "needs-us",
		Labels: map[string]string{"zone": "us"}, // mismatch
	}
	decision := scheduleBinPack(scorer, snapshots, hosts, req, "local")
	// remote skipped (label mismatch); local ALSO excluded — it carries no
	// labels, so it cannot satisfy the required zone=us label. No host fits.
	assert.Empty(t, decision.HostName)
}

// TestScheduleBinPack_NoSnapshot exercises the no-snapshot branch for
// remote hosts in scheduleBinPack.
func TestScheduleBinPack_NoSnapshot(t *testing.T) {
	scorer := NewResourceScorer(DefaultSchedulerOptions())
	snapshots := map[string]*remote.HostResources{
		"local": {CPUPercent: 10, CPUCores: 8, MemoryPercent: 10, MemoryTotalMB: 16384, DiskPercent: 10, DiskTotalMB: 100000},
	}
	hosts := []remote.RemoteHost{
		{Name: "no-snap", Labels: map[string]string{}},
	}

	req := ContainerRequirements{Name: "pack-test"}
	decision := scheduleBinPack(scorer, snapshots, hosts, req, "local")
	// no-snap skipped (no snapshot).
	assert.Equal(t, "local", decision.HostName)
}

// TestScoreMemory_AvailNegativeWithRequest exercises the case where reserve
// makes available memory negative before the MemoryMB check.
func TestScoreMemory_AvailNegativeWithRequest(t *testing.T) {
	opts := DefaultSchedulerOptions()
	opts.ReservePercent = 90 // 90% reserve
	s := NewResourceScorer(opts)

	res := &remote.HostResources{
		MemoryPercent: 20, // 80% free, but 90% reserve → avail = -10
		MemoryTotalMB: 4096,
	}
	// avail = 80 - 90 = -10 → return 0
	score := s.scoreMemory(res, ContainerRequirements{MemoryMB: 512}, 90.0, 1.0)
	assert.Equal(t, 0.0, score)
}
