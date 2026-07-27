package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-20 batch CT-HARDEN-SCHED-HARD SC2 — genuine correctness/honesty defects
// found by anti-bluff audit of pkg/scheduler, each proven §11.4.115 RED->GREEN.
//
// Committed polarity is GUARD (GREEN with the fixes IN). Each guard is proven
// real by a SURGICAL REVERT of its single source anchor -> genuine `--- FAIL`
// -> restore -> GREEN (the anti-tautology self-proof, captured in the report).
//
// HONEST BOUNDARY (§11.4.107): these guards exercise the scheduler's DECISION
// logic on injected snapshot fixtures via the existing mockHostManager +
// makeSnapshot seams. They do NOT drive a live runtime or a real SSH deploy —
// the defects (a false "no host fits", and map-iteration-order-dependent
// placement) live entirely in the placement/selection logic.

// --- SC2-1: pickGPUAffinity honesty (§11.4.108) ----------------------------

// TestWave20_SC2_GPUAffinity_FittingZeroScoreHostSelected proves pickGPUAffinity
// does NOT report "no host fits" for a host that CanFit reports as genuinely
// fitting but whose composite score clamps to exactly 0. A host saturated on
// CPU/mem/disk/network with a free, matching GPU fits a GPU-only requirement
// (CanFit==true) yet Score()==0 (default GPUWeight is 0, every other component is
// 0). Pre-fix `bestScore := 0.0` + strict `>` dropped it and returned the
// "no host fits GPU requirement" sentinel — a false negative that also bypassed
// scheduleOne's minSelectedScore fixup. Fixed: `bestScore := -1.0` selects the
// fitting host even at score 0.
func TestWave20_SC2_GPUAffinity_FittingZeroScoreHostSelected(t *testing.T) {
	scorer := NewResourceScorer(Options{
		CPUWeight: 0.4, MemoryWeight: 0.4, DiskWeight: 0.1, NetworkWeight: 0.1,
		GPUWeight: 0, ReservePercent: 10, OvercommitRatio: 1,
	})
	saturated := &remote.HostResources{
		Host: "sat", CPUCores: 16, CPUPercent: 100,
		MemoryTotalMB: 64000, MemoryPercent: 100,
		DiskTotalMB: 1000000, DiskPercent: 100,
		NetworkRxBytesPerSec: 125_000_000, NetworkTxBytesPerSec: 125_000_000,
		GPU: []remote.GPUDevice{
			{
				Index: 0, Vendor: "nvidia", VRAMFreeMB: 24000,
				VRAMTotalMB: 24000, CUDASupported: true,
			},
		},
	}
	req := ContainerRequirements{
		Name: "g", GPU: &GPURequirement{Count: 1, Vendor: "nvidia"},
	}

	// Sanity: the fixture is genuine — the host truly fits, and its composite
	// score truly is exactly 0 (so the defect is a real false negative, not a
	// contrived assertion).
	require.True(t, scorer.CanFit(saturated, req),
		"fixture invalid: the saturated host must genuinely fit the GPU-only req")
	require.Equal(t, 0.0, scorer.Score(saturated, req),
		"fixture invalid: the saturated host's composite score must be exactly 0")

	host, reason := pickGPUAffinity(
		map[string]*remote.HostResources{"sat": saturated}, req, scorer,
	)
	assert.Equal(t, "sat", host,
		"pickGPUAffinity dropped a GPU host that CanFit reports as fitting because "+
			"its composite score clamped to 0 (§11.4.108 false 'no host fits')")
	assert.NotContains(t, reason, "no host fits",
		"a genuinely-fitting host must not yield the no-host sentinel reason")
}

// TestWave20_SC2_GPUAffinity_HigherScoreStillWins is the negative-control for
// SC2-1/SC2-2: the fix (bestScore := -1.0 + name tie-break) must NOT over-correct
// into "first fitting host always wins" — a strictly higher-scoring host must
// still beat a zero-scoring one regardless of name order.
func TestWave20_SC2_GPUAffinity_HigherScoreStillWins(t *testing.T) {
	scorer := NewResourceScorer(Options{
		CPUWeight: 0.4, MemoryWeight: 0.4, DiskWeight: 0.1, NetworkWeight: 0.1,
		GPUWeight: 0, ReservePercent: 10, OvercommitRatio: 1,
	})
	gpu := []remote.GPUDevice{
		{Index: 0, Vendor: "nvidia", VRAMFreeMB: 24000, VRAMTotalMB: 24000, CUDASupported: true},
	}
	// "z-zero" sorts AFTER "a-idle" by name, and is saturated (score 0); the idle
	// host "a-idle" has a strictly higher score and MUST win.
	zero := &remote.HostResources{
		Host: "z-zero", CPUCores: 16, CPUPercent: 100,
		MemoryTotalMB: 64000, MemoryPercent: 100,
		DiskTotalMB: 1000000, DiskPercent: 100,
		NetworkRxBytesPerSec: 125_000_000, NetworkTxBytesPerSec: 125_000_000,
		GPU: gpu,
	}
	idle := &remote.HostResources{
		Host: "a-idle", CPUCores: 16, CPUPercent: 10,
		MemoryTotalMB: 64000, MemoryPercent: 10,
		DiskTotalMB: 1000000, DiskPercent: 10,
		GPU: gpu,
	}
	req := ContainerRequirements{
		Name: "g", GPU: &GPURequirement{Count: 1, Vendor: "nvidia"},
	}
	require.Greater(t, scorer.Score(idle, req), scorer.Score(zero, req),
		"fixture invalid: the idle host must score strictly higher than the zero host")

	host, _ := pickGPUAffinity(
		map[string]*remote.HostResources{"z-zero": zero, "a-idle": idle}, req, scorer,
	)
	assert.Equal(t, "a-idle", host,
		"the strictly higher-scoring host must win; the fix must not degrade into "+
			"first-fitting-wins")
}

// --- SC2-2: pickGPUAffinity tie-break determinism (§11.4.50) ----------------

// TestWave20_SC2_GPUAffinity_TieBreakDeterministic proves pickGPUAffinity selects
// the same host on every run when two hosts score identically. `candidates` is a
// map; Go randomizes map iteration order per range, and the pre-fix `sc >
// bestScore` first-seen tie-break therefore chose non-deterministically. Fixed: a
// `name < bestHost` tie-break makes the name-min host win every time.
func TestWave20_SC2_GPUAffinity_TieBreakDeterministic(t *testing.T) {
	scorer := NewResourceScorer(Options{
		CPUWeight: 0.4, MemoryWeight: 0.4, DiskWeight: 0.1, NetworkWeight: 0.1,
		GPUWeight: 0, ReservePercent: 10, OvercommitRatio: 1,
	})
	mk := func(name string) *remote.HostResources {
		return &remote.HostResources{
			Host: name, CPUCores: 16, CPUPercent: 20,
			MemoryTotalMB: 64000, MemoryPercent: 20,
			DiskTotalMB: 1000000, DiskPercent: 20,
			GPU: []remote.GPUDevice{
				{Index: 0, Vendor: "nvidia", VRAMFreeMB: 24000, VRAMTotalMB: 24000, CUDASupported: true},
			},
		}
	}
	cands := map[string]*remote.HostResources{"gpu-a": mk("gpu-a"), "gpu-b": mk("gpu-b")}
	req := ContainerRequirements{
		Name: "g", GPU: &GPURequirement{Count: 1, Vendor: "nvidia"},
	}
	require.Equal(t, scorer.Score(cands["gpu-a"], req), scorer.Score(cands["gpu-b"], req),
		"fixture invalid: the two hosts must score identically for a genuine tie")

	first, _ := pickGPUAffinity(cands, req, scorer)
	for i := 0; i < 200; i++ {
		got, _ := pickGPUAffinity(cands, req, scorer)
		require.Equal(t, first, got,
			"pickGPUAffinity returned different hosts across runs for an exact score "+
				"tie (map-iteration-order dependence, §11.4.50)")
	}
	assert.Equal(t, "gpu-a", first,
		"the deterministic tie-break must select the name-min host")
}

// --- SC2-3: scheduleResourceAware tie-break determinism (§11.4.50) ----------

// TestWave20_SC2_ResourceAware_TieBreakDeterministic proves the DEFAULT strategy
// places on the same host on every run when two candidates score identically.
// candidates come from HostManager.ListHosts() (map order, random per call) and
// sort.Slice is not stable, so pre-fix the top-of-two tie resolved in
// map-iteration order. Fixed: a `name` tie-break in the sort comparator.
func TestWave20_SC2_ResourceAware_TieBreakDeterministic(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "ra-a", Address: "10.0.0.1", User: "u"})
	_ = mgr.AddHost(remote.RemoteHost{Name: "ra-b", Address: "10.0.0.2", User: "u"})
	// Byte-identical snapshots -> identical scores -> exact tie. No "local"
	// snapshot, so the local host is not a candidate and cannot mask the tie.
	mgr.snapshots["ra-a"] = makeSnapshot("ra-a", 20, 20, 16384, 500000, 8)
	mgr.snapshots["ra-b"] = makeSnapshot("ra-b", 20, 20, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{}, WithStrategy(StrategyResourceAware))
	req := ContainerRequirements{Name: "app", MemoryMB: 512}

	d0, err := sched.Schedule(context.Background(), req)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		d, err := sched.Schedule(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, d0.HostName, d.HostName,
			"resource_aware placed on different hosts across runs for an exact score "+
				"tie (ListHosts map order + non-stable sort, §11.4.50)")
	}
	assert.Equal(t, "ra-a", d0.HostName,
		"the deterministic tie-break must select the name-min host")
}

// --- SC2-4: scheduleAffinity tie-break determinism (§11.4.50) ---------------

// TestWave20_SC2_Affinity_TieBreakDeterministic proves the affinity strategy
// selects the same host on every run when two label-matching hosts score
// identically. Pre-fix `s > best.score` broke ties by first-seen over `hosts`
// (ListHosts map order). Fixed: a `h.Name < best.name` tie-break.
func TestWave20_SC2_Affinity_TieBreakDeterministic(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "af-a", Address: "10.0.0.1", User: "u",
		Labels: map[string]string{"tier": "gpu"},
	})
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "af-b", Address: "10.0.0.2", User: "u",
		Labels: map[string]string{"tier": "gpu"},
	})
	mgr.snapshots["af-a"] = makeSnapshot("af-a", 20, 20, 16384, 500000, 8)
	mgr.snapshots["af-b"] = makeSnapshot("af-b", 20, 20, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{}, WithStrategy(StrategyAffinity))
	req := ContainerRequirements{
		Name: "app", Labels: map[string]string{"tier": "gpu"}, MemoryMB: 512,
	}

	d0, err := sched.Schedule(context.Background(), req)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		d, err := sched.Schedule(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, d0.HostName, d.HostName,
			"affinity placed on different hosts across runs for an exact score tie "+
				"(ListHosts map order, first-seen tie-break, §11.4.50)")
	}
	assert.Equal(t, "af-a", d0.HostName,
		"the deterministic tie-break must select the name-min host")
}

// --- SC2-5: scheduleSpread tie-break determinism (§11.4.50) -----------------

// TestWave20_SC2_Spread_TieBreakDeterministic proves the spread strategy selects
// the same host on every fresh scheduler when two label-matching hosts have equal
// container counts. The tie exists only on the FIRST placement (counts diverge
// after), so each iteration uses a fresh scheduler. Pre-fix the sort resolved the
// equal-count tie in ListHosts map order; fixed: a `name` tie-break.
func TestWave20_SC2_Spread_TieBreakDeterministic(t *testing.T) {
	build := func() *DefaultScheduler {
		mgr := newMockHostManager()
		_ = mgr.AddHost(remote.RemoteHost{
			Name: "sp-a", Address: "10.0.0.1", User: "u",
			Labels: map[string]string{"tier": "gpu"},
		})
		_ = mgr.AddHost(remote.RemoteHost{
			Name: "sp-b", Address: "10.0.0.2", User: "u",
			Labels: map[string]string{"tier": "gpu"},
		})
		mgr.snapshots["sp-a"] = makeSnapshot("sp-a", 20, 20, 16384, 500000, 8)
		mgr.snapshots["sp-b"] = makeSnapshot("sp-b", 20, 20, 16384, 500000, 8)
		return NewScheduler(mgr, logging.NopLogger{}, WithStrategy(StrategySpread))
	}
	// req requires tier=gpu so the unlabelled local host is excluded and cannot
	// mask the sp-a/sp-b tie.
	req := ContainerRequirements{Name: "app", Labels: map[string]string{"tier": "gpu"}}

	first, err := build().Schedule(context.Background(), req)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		d, err := build().Schedule(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, first.HostName, d.HostName,
			"spread placed on different hosts across fresh schedulers for an equal "+
				"container-count tie (ListHosts map order + non-stable sort, §11.4.50)")
	}
	assert.Equal(t, "sp-a", first.HostName,
		"the deterministic tie-break must select the name-min host")
}

// --- SC2-6: scheduleBinPack tie-break determinism (§11.4.50) ----------------

// TestWave20_SC2_BinPack_TieBreakDeterministic proves the bin_pack strategy
// selects the same host on every run when two label-matching hosts have equal
// utilization (CPUPercent+MemoryPercent). Pre-fix the sort resolved the equal-used
// tie in ListHosts map order; fixed: a `name` tie-break.
func TestWave20_SC2_BinPack_TieBreakDeterministic(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "bp-a", Address: "10.0.0.1", User: "u",
		Labels: map[string]string{"tier": "gpu"},
	})
	_ = mgr.AddHost(remote.RemoteHost{
		Name: "bp-b", Address: "10.0.0.2", User: "u",
		Labels: map[string]string{"tier": "gpu"},
	})
	// Equal CPUPercent+MemoryPercent -> equal `used` -> exact tie.
	mgr.snapshots["bp-a"] = makeSnapshot("bp-a", 40, 30, 16384, 500000, 8)
	mgr.snapshots["bp-b"] = makeSnapshot("bp-b", 40, 30, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{}, WithStrategy(StrategyBinPack))
	req := ContainerRequirements{
		Name: "app", Labels: map[string]string{"tier": "gpu"}, MemoryMB: 512,
	}

	d0, err := sched.Schedule(context.Background(), req)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		d, err := sched.Schedule(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, d0.HostName, d.HostName,
			"bin_pack placed on different hosts across runs for an equal-utilization "+
				"tie (ListHosts map order + non-stable sort, §11.4.50)")
	}
	assert.Equal(t, "bp-a", d0.HostName,
		"the deterministic tie-break must select the name-min host")
}
