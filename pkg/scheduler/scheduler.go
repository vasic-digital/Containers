package scheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Scheduler decides where to place containers across available
// hosts.
type Scheduler interface {
	// Schedule determines the best host for a single container.
	Schedule(
		ctx context.Context, req ContainerRequirements,
	) (*PlacementDecision, error)

	// ScheduleBatch determines placement for multiple containers.
	ScheduleBatch(
		ctx context.Context, reqs []ContainerRequirements,
	) (*PlacementPlan, error)

	// Rebalance evaluates current placement and suggests moves.
	Rebalance(ctx context.Context) (*PlacementPlan, error)
}

// DefaultScheduler implements Scheduler using configurable
// strategies.
type DefaultScheduler struct {
	opts        Options
	hostManager remote.HostManager
	scorer      *ResourceScorer
	logger      logging.Logger
	// mu guards placements. Schedule and ScheduleBatch both mutate it
	// (`s.placements[decision.HostName]++`) and StrategySpread reads it
	// concurrently inside a sort.Slice comparator (scheduleSpread in
	// strategies.go) — an unguarded concurrent map read+write on a
	// *DefaultScheduler shared across goroutines (the normal way a
	// Scheduler is used: one long-lived instance driving concurrent
	// placement requests from a distribution/orchestration layer).
	// Without this lock, Go's runtime can fatal with "concurrent map
	// read and map write" — not just a benign data race.
	mu         sync.Mutex
	placements map[string]int // host -> container count
	// rrCounter is this scheduler's own round-robin rotation. It is
	// per-instance (not a package global) so two independent schedulers do
	// not share one counter and desynchronise each other's fair rotation.
	rrCounter atomic.Uint64
}

// minSelectedScore is the floor applied to a selected placement's score.
// The distribution layer (pkg/distribution) treats Score==0 as the "no host
// found" sentinel and drops the container as failed, so a genuinely selected
// but resource-saturated host — whose score can clamp to exactly 0 — must
// report a tiny positive score instead, keeping Score==0 unambiguous.
const minSelectedScore = 1e-4

// NewScheduler creates a DefaultScheduler.
func NewScheduler(
	hostManager remote.HostManager,
	logger logging.Logger,
	opts ...Option,
) *DefaultScheduler {
	o := ApplyOptions(opts)
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultScheduler{
		opts:        o,
		hostManager: hostManager,
		scorer:      NewResourceScorer(o),
		logger:      logger,
		placements:  make(map[string]int),
	}
}

// Schedule determines placement for a single container.
func (s *DefaultScheduler) Schedule(
	ctx context.Context, req ContainerRequirements,
) (*PlacementDecision, error) {
	snapshots := s.hostManager.ProbeAll(ctx)
	hosts := s.hostManager.ListHosts()

	s.mu.Lock()
	decision := s.scheduleOne(snapshots, hosts, req)
	if decision.HostName != "" {
		s.placements[decision.HostName]++
	}
	s.mu.Unlock()

	s.logger.Info("scheduled %s -> %s (score=%.3f, reason=%s)",
		req.Name, decision.HostName, decision.Score,
		decision.Reason,
	)
	return &decision, nil
}

// ScheduleBatch determines placement for multiple containers.
func (s *DefaultScheduler) ScheduleBatch(
	ctx context.Context, reqs []ContainerRequirements,
) (*PlacementPlan, error) {
	if len(reqs) == 0 {
		return &PlacementPlan{}, nil
	}

	snapshots := s.hostManager.ProbeAll(ctx)
	hosts := s.hostManager.ListHosts()

	plan := &PlacementPlan{
		Decisions:     make([]PlacementDecision, 0, len(reqs)),
		HostSnapshots: snapshots,
	}

	// working is a per-batch DEEP COPY of the probed snapshots. After each
	// placement, deductCapacity shrinks the chosen host's snapshot in `working`,
	// so later containers in the SAME batch are scored against the reduced
	// capacity instead of re-scoring the pristine snapshot — the fix for
	// CT-HARDEN-SCHED-1 (Wave-20): resource_aware/affinity/bin_pack all score off
	// the snapshot and ignored s.placements, so an un-decremented snapshot let
	// every container in a batch pile onto the same top host (e.g. three 5-core
	// containers all landing on one 8-core host = 15 cores on 8). `working` is
	// goroutine-local to this call, so mutating it needs no lock.
	working := cloneSnapshots(snapshots)

	// batchLog defers logging until AFTER s.mu is released. Logging can perform
	// file/network I/O; the pre-fix loop held s.mu across every log write for the
	// whole batch, serialising all concurrent Schedule/ScheduleBatch/Release for
	// that duration (CT-HARDEN-SCHED-3, Wave-20). Schedule already unlocks before
	// logging; this mirrors it — the lock now guards ONLY the shared state
	// (scheduleOne's read of s.placements + the s.placements increment).
	type batchLog struct {
		name  string
		host  string
		score float64
	}
	logs := make([]batchLog, 0, len(reqs))

	for _, req := range reqs {
		s.mu.Lock()
		decision := s.scheduleOne(working, hosts, req)
		if decision.HostName != "" {
			s.placements[decision.HostName]++
		}
		s.mu.Unlock()

		// Deduct on the batch-local working copy (no shared state) OUTSIDE the
		// lock.
		if decision.HostName != "" {
			deductCapacity(working[decision.HostName], req)
		}

		plan.Decisions = append(plan.Decisions, decision)
		logs = append(logs, batchLog{
			name: req.Name, host: decision.HostName, score: decision.Score,
		})
	}

	for _, l := range logs {
		s.logger.Info(
			"batch: scheduled %s -> %s (score=%.3f)",
			l.name, l.host, l.score,
		)
	}

	return plan, nil
}

// cloneSnapshots deep-copies a probed snapshot map so within-batch capacity
// deductions mutate a per-batch working copy, never the caller's snapshots (also
// exposed via PlacementPlan.HostSnapshots) nor the HostManager's internal state.
// Each *HostResources is copied by value and its GPU slice is cloned so a GPU
// deduction does not alias the source backing array.
func cloneSnapshots(
	snapshots map[string]*remote.HostResources,
) map[string]*remote.HostResources {
	out := make(map[string]*remote.HostResources, len(snapshots))
	for name, snap := range snapshots {
		if snap == nil {
			out[name] = nil
			continue
		}
		cp := *snap
		if snap.GPU != nil {
			cp.GPU = append([]remote.GPUDevice(nil), snap.GPU...)
		}
		out[name] = &cp
	}
	return out
}

// deductCapacity reduces a working-copy host snapshot by the resources a
// just-placed container consumes, so later CanFit/Score calls in the SAME batch
// see reduced capacity (CT-HARDEN-SCHED-1). CPU/mem/disk are modelled as physical
// utilization increases in percentage points — matching how
// AvailableCPUPercent/AvailableMemoryPercent/AvailableDiskPercent derive
// availability (100 - used%). Percentages are capped at 100.
func deductCapacity(r *remote.HostResources, req ContainerRequirements) {
	if r == nil {
		return
	}
	if req.CPUCores > 0 && r.CPUCores > 0 {
		r.CPUPercent = capPercent(
			r.CPUPercent + req.CPUCores/float64(r.CPUCores)*100.0,
		)
	}
	if req.MemoryMB > 0 && r.MemoryTotalMB > 0 {
		r.MemoryPercent = capPercent(
			r.MemoryPercent +
				float64(req.MemoryMB)/float64(r.MemoryTotalMB)*100.0,
		)
		r.MemoryUsedMB += req.MemoryMB
	}
	if req.DiskMB > 0 && r.DiskTotalMB > 0 {
		r.DiskPercent = capPercent(
			r.DiskPercent +
				float64(req.DiskMB)/float64(r.DiskTotalMB)*100.0,
		)
		r.DiskUsedMB += req.DiskMB
	}
	r.RunningContainers++
	if req.GPU != nil {
		deductGPU(r, *req.GPU)
	}
}

func capPercent(v float64) float64 {
	if v > 100 {
		return 100
	}
	return v
}

// deductGPU removes the GPUs a just-placed container consumes from a working-copy
// snapshot (whole-GPU exclusive assignment), so a later GPU request in the same
// batch cannot be matched to the same physical GPUs. It drops the `need` matching
// GPUs with the most free VRAM, mirroring scoreGPU's "most free VRAM" selection.
func deductGPU(r *remote.HostResources, req GPURequirement) {
	need := req.Count
	if need <= 0 {
		// Mirror CanFit's documented "zero (or negative) Count defaults to 1".
		need = 1
	}
	type gpuIdx struct {
		idx  int
		free int
	}
	var cand []gpuIdx
	for i, g := range r.GPU {
		if gpuMatchesReq(g, req) {
			cand = append(cand, gpuIdx{idx: i, free: g.VRAMFreeMB})
		}
	}
	if len(cand) == 0 {
		return
	}
	sort.Slice(cand, func(a, b int) bool {
		return cand[a].free > cand[b].free
	})
	drop := make(map[int]bool, need)
	for k := 0; k < need && k < len(cand); k++ {
		drop[cand[k].idx] = true
	}
	kept := make([]remote.GPUDevice, 0, len(r.GPU))
	for i, g := range r.GPU {
		if !drop[i] {
			kept = append(kept, g)
		}
	}
	r.GPU = kept
}

// Rebalance suggests redistributing existing containers.
func (s *DefaultScheduler) Rebalance(
	ctx context.Context,
) (*PlacementPlan, error) {
	snapshots := s.hostManager.ProbeAll(ctx)
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("no host snapshots available")
	}

	plan := &PlacementPlan{
		HostSnapshots: snapshots,
	}

	// Identify overloaded hosts (>80% CPU or memory).
	for name, snap := range snapshots {
		if snap.CPUPercent > 80 || snap.MemoryPercent > 80 {
			s.logger.Warn(
				"host %s overloaded: CPU=%.1f%% Mem=%.1f%%",
				name, snap.CPUPercent, snap.MemoryPercent,
			)
		}
	}

	return plan, nil
}

// Release decrements the placement counter for a host, reflecting a container
// that has been removed / undistributed. StrategySpread places each new
// container on the least-loaded host by reading s.placements; without a
// decrement the counter only ever grows, so spread keeps steering away from a
// host that has long since been drained. Release is a method on the concrete
// *DefaultScheduler rather than part of the Scheduler interface — adding a
// method to the interface would break external implementers; lifecycle-aware
// callers hold the concrete type.
func (s *DefaultScheduler) Release(hostName string) {
	if hostName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.placements[hostName] > 0 {
		s.placements[hostName]--
	}
}

func (s *DefaultScheduler) scheduleOne(
	snapshots map[string]*remote.HostResources,
	hosts []remote.RemoteHost,
	req ContainerRequirements,
) PlacementDecision {
	var decision PlacementDecision
	switch s.opts.Strategy {
	case StrategyRoundRobin:
		decision = scheduleRoundRobin(
			hostsPresentInSnapshots(hosts, snapshots), req,
			s.opts.LocalHostName, &s.rrCounter,
		)
	case StrategyAffinity:
		decision = scheduleAffinity(
			s.scorer, snapshots, hosts, req,
		)
	case StrategySpread:
		decision = scheduleSpread(
			snapshots, hostsPresentInSnapshots(hosts, snapshots), req,
			s.opts.LocalHostName, s.placements,
		)
	case StrategyBinPack:
		decision = scheduleBinPack(
			s.scorer, snapshots, hosts, req,
			s.opts.LocalHostName,
		)
	case StrategyGPUAffinity:
		host, reason := pickGPUAffinity(snapshots, req, s.scorer)
		decision = PlacementDecision{
			Requirement: req,
			HostName:    host,
			Score:       s.scorer.Score(snapshots[host], req),
			Reason:      reason,
		}
	default:
		decision = scheduleResourceAware(
			s.scorer, snapshots, hosts, req,
			s.opts.LocalHostName,
		)
	}

	// A selected host must never report Score==0 (see minSelectedScore).
	if decision.HostName != "" && decision.Score == 0 {
		decision.Score = minSelectedScore
	}
	return decision
}

// hostsPresentInSnapshots keeps only remote hosts that produced a probe snapshot
// this cycle. remote.HostManager.ProbeAll OMITS any host whose probe errored
// (host_manager.go ProbeAll), so a host in ListHosts but absent from snapshots is
// unreachable/offline this cycle. The snapshot-based strategies (resource_aware,
// affinity, bin_pack) already gate on `snapshots[h.Name]`; round_robin and spread
// did not, so an offline REMOTE host stayed in their rotation and the distributor
// would SSH-deploy to a dead host — the fix for CT-HARDEN-SCHED-2 (Wave-20).
//
// HONEST BOUNDARY (§11.4.6): this gates only the REMOTE candidate list by
// probe-presence. The local host is intentionally NOT filtered here — it is
// always reachable (it is the current process's host) and is never SSH-deployed
// (the distributor runs it via the local runtime), so these resource-agnostic
// strategies keep it eligible without requiring a probe, exactly as before.
func hostsPresentInSnapshots(
	hosts []remote.RemoteHost,
	snapshots map[string]*remote.HostResources,
) []remote.RemoteHost {
	out := make([]remote.RemoteHost, 0, len(hosts))
	for _, h := range hosts {
		if _, ok := snapshots[h.Name]; ok {
			out = append(out, h)
		}
	}
	return out
}
