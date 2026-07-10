package scheduler

import (
	"context"
	"fmt"
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

	s.mu.Lock()
	for _, req := range reqs {
		decision := s.scheduleOne(snapshots, hosts, req)
		if decision.HostName != "" {
			s.placements[decision.HostName]++
		}
		plan.Decisions = append(plan.Decisions, decision)

		s.logger.Info(
			"batch: scheduled %s -> %s (score=%.3f)",
			req.Name, decision.HostName, decision.Score,
		)
	}
	s.mu.Unlock()

	return plan, nil
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
			hosts, req, s.opts.LocalHostName, &s.rrCounter,
		)
	case StrategyAffinity:
		decision = scheduleAffinity(
			s.scorer, snapshots, hosts, req,
		)
	case StrategySpread:
		decision = scheduleSpread(
			snapshots, hosts, req,
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
