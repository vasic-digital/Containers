package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-20 batch CT-HARDEN-SCHED-HARD — §11.4.115 RED->GREEN guards.
//
// Committed polarity is GUARD (GREEN with the fixes IN). Each guard is proven
// real by a SURGICAL REVERT of its fix -> genuine `--- FAIL` -> restore -> GREEN
// (evidence captured in the batch report).
//
// HONEST BOUNDARY (§11.4.107): these guards prove the placement /
// capacity-deduction / probe-gating / lock-scope logic on INJECTED snapshot
// fixtures via the existing mockHostManager + makeSnapshot seams. They do NOT
// exercise a live container runtime or a real remote SSH deploy — they validate
// the scheduler's decision logic, which is where the three defects live.

// TestWave20_SchedHard_SCHED1_BatchCapacityDeducted proves ScheduleBatch deducts
// within-batch capacity: three 5-core containers cannot all land on a single
// 8-core host. Pre-fix, every container was re-scored against the PRISTINE
// snapshot (CanFit: ~7.2 schedulable cores each iteration), so all three "fit"
// and piled onto the one host (15 cores on 8 = over-commit). Fixed: only the
// first fits; the deducted working copy makes the rest report no-host.
func TestWave20_SchedHard_SCHED1_BatchCapacityDeducted(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "h1", Address: "10.0.0.1", User: "u"})
	// 8 cores, 0% used. Default reserve 10% + overcommit 1.0 => ~7.2 schedulable
	// cores: exactly ONE 5-core container fits, a second would over-commit.
	mgr.snapshots["h1"] = makeSnapshot("h1", 0, 0, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyResourceAware),
	)

	reqs := []ContainerRequirements{
		{Name: "c1", CPUCores: 5},
		{Name: "c2", CPUCores: 5},
		{Name: "c3", CPUCores: 5},
	}
	plan, err := sched.ScheduleBatch(context.Background(), reqs)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 3)

	onH1 := 0
	for _, d := range plan.Decisions {
		if d.HostName == "h1" {
			onH1++
		}
	}
	assert.Equal(t, 1, onH1,
		"ScheduleBatch placed %d/3 five-core containers on one 8-core host; "+
			"within-batch capacity was not deducted between placements "+
			"(over-commit)", onH1)
	assert.Equal(t, "h1", plan.Decisions[0].HostName,
		"the first container must still be placed on the host it fits")
	assert.Empty(t, plan.Decisions[1].HostName,
		"the second container over-commits the host and must report no-host")
	assert.Empty(t, plan.Decisions[2].HostName,
		"the third container over-commits the host and must report no-host")
}

// TestWave20_SchedHard_SCHED1_BatchGPUDeducted proves the within-batch deduction
// covers GPUs (whole-GPU exclusive assignment): a host with ONE nvidia GPU
// cannot satisfy two GPU containers in one batch. Pre-fix both were re-scored
// against the pristine snapshot (the single GPU still matched each), so both
// landed on the host. Fixed: the first placement removes the GPU from the
// working copy, so the second finds no matching GPU.
func TestWave20_SchedHard_SCHED1_BatchGPUDeducted(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "g1", Address: "10.0.0.9", User: "u"})
	snap := makeSnapshot("g1", 0, 0, 32768, 1000000, 16)
	snap.GPU = []remote.GPUDevice{
		{
			Index: 0, Vendor: "nvidia", VRAMFreeMB: 24000,
			VRAMTotalMB: 24000, CUDASupported: true,
		},
	}
	mgr.snapshots["g1"] = snap

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyResourceAware),
	)

	reqs := []ContainerRequirements{
		{Name: "gc1", GPU: &GPURequirement{Count: 1, Vendor: "nvidia"}},
		{Name: "gc2", GPU: &GPURequirement{Count: 1, Vendor: "nvidia"}},
	}
	plan, err := sched.ScheduleBatch(context.Background(), reqs)
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 2)

	assert.Equal(t, "g1", plan.Decisions[0].HostName,
		"the first GPU container must be placed on the GPU host")
	assert.Empty(t, plan.Decisions[1].HostName,
		"the second GPU container was placed on a host whose only GPU is "+
			"already assigned (within-batch GPU capacity not deducted)")
}

// TestWave20_SchedHard_SCHED2_RoundRobinSkipsUnprobedHost proves round_robin does
// not rotate onto a remote host that failed to probe (present in ListHosts,
// ABSENT from ProbeAll's snapshots). Pre-fix the rotation list was built purely
// from ListHosts, so an offline host stayed eligible and the distributor would
// SSH-deploy to a dead host. Fixed: hostsPresentInSnapshots filters it out.
func TestWave20_SchedHard_SCHED2_RoundRobinSkipsUnprobedHost(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "h1", Address: "10.0.0.1", User: "u"})
	_ = mgr.AddHost(remote.RemoteHost{Name: "h2", Address: "10.0.0.2", User: "u"})
	// h2 failed to probe: registered but absent from snapshots.
	mgr.snapshots["local"] = makeSnapshot("local", 20, 20, 16384, 500000, 8)
	mgr.snapshots["h1"] = makeSnapshot("h1", 20, 20, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyRoundRobin),
	)

	seenH2 := false
	for i := 0; i < 6; i++ {
		d, err := sched.Schedule(context.Background(),
			ContainerRequirements{Name: "app"},
		)
		require.NoError(t, err)
		if d.HostName == "h2" {
			seenH2 = true
		}
	}
	// Pre-fix: candidate list [local,h1,h2] -> over 6 rotations h2 is selected
	// twice. Fixed: h2 (unprobed) excluded -> never selected.
	assert.False(t, seenH2,
		"round_robin selected unprobed/offline host h2; the distributor would "+
			"SSH-deploy to a dead host")
}

// TestWave20_SchedHard_SCHED2_SpreadSkipsUnprobedHost proves the same probe-gating
// for spread. Pre-fix spread balanced across all registered hosts including an
// unprobed one; fixed, the unprobed host is excluded.
func TestWave20_SchedHard_SCHED2_SpreadSkipsUnprobedHost(t *testing.T) {
	mgr := newMockHostManager()
	_ = mgr.AddHost(remote.RemoteHost{Name: "h1", Address: "10.0.0.1", User: "u"})
	_ = mgr.AddHost(remote.RemoteHost{Name: "h2", Address: "10.0.0.2", User: "u"})
	mgr.snapshots["local"] = makeSnapshot("local", 20, 20, 16384, 500000, 8)
	mgr.snapshots["h1"] = makeSnapshot("h1", 20, 20, 16384, 500000, 8)
	// h2 absent from snapshots (probe failed).

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategySpread),
	)

	reqs := make([]ContainerRequirements, 6)
	for i := range reqs {
		reqs[i] = ContainerRequirements{Name: "app"}
	}
	plan, err := sched.ScheduleBatch(context.Background(), reqs)
	require.NoError(t, err)

	onH2 := 0
	for _, d := range plan.Decisions {
		if d.HostName == "h2" {
			onH2++
		}
	}
	// Pre-fix: spread balances across [local,h1,h2] -> h2 gets 2 of 6. Fixed: h2
	// excluded -> 0.
	assert.Equal(t, 0, onH2,
		"spread placed %d/6 containers on unprobed/offline host h2", onH2)
}

// lockProbeLogger is a white-box Logger (same package) that, on every Info call,
// probes whether the scheduler's mutex s.mu is currently held. sync.Mutex.TryLock
// succeeds ONLY if the mutex is free; the pre-fix ScheduleBatch calls Info while
// holding s.mu (same goroutine), so TryLock fails and heldWhileLogging is bumped.
type lockProbeLogger struct {
	sched            *DefaultScheduler
	calls            int
	heldWhileLogging int
}

func (l *lockProbeLogger) Debug(string, ...any) {}
func (l *lockProbeLogger) Warn(string, ...any)  {}
func (l *lockProbeLogger) Error(string, ...any) {}
func (l *lockProbeLogger) Info(string, ...any) {
	l.calls++
	if l.sched.mu.TryLock() {
		l.sched.mu.Unlock()
	} else {
		l.heldWhileLogging++
	}
}

// TestWave20_SchedHard_SCHED3_LogsOutsideLock proves ScheduleBatch does not hold
// s.mu while logging (PRINCIPLE #2: no blocking I/O inside a shared-lock region).
// A user Logger doing file/network I/O under s.mu would serialise every
// concurrent Schedule/ScheduleBatch/Release for the batch's cumulative log time.
// Pre-fix all Info calls run inside the lock (heldWhileLogging == len(reqs)).
// Fixed: logging happens after s.mu is released (heldWhileLogging == 0).
func TestWave20_SchedHard_SCHED3_LogsOutsideLock(t *testing.T) {
	mgr := newMockHostManager()
	mgr.snapshots["local"] = makeSnapshot("local", 20, 20, 16384, 500000, 8)

	sched := NewScheduler(mgr, logging.NopLogger{},
		WithStrategy(StrategyResourceAware),
	)
	probe := &lockProbeLogger{sched: sched}
	sched.logger = probe

	reqs := []ContainerRequirements{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	_, err := sched.ScheduleBatch(context.Background(), reqs)
	require.NoError(t, err)

	require.Equal(t, len(reqs), probe.calls,
		"expected exactly one logger.Info per batched container")
	assert.Equal(t, 0, probe.heldWhileLogging,
		"logger.Info was called while s.mu was held (%d/%d times); log I/O "+
			"under the scheduler lock serialises concurrent Schedule/Release",
		probe.heldWhileLogging, probe.calls)
}
