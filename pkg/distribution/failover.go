package distribution

import (
	"context"
	"fmt"

	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// FailoverHandler detects offline hosts and reschedules their
// containers.
type FailoverHandler struct {
	distributor *DefaultDistributor
}

// NewFailoverHandler creates a FailoverHandler.
func NewFailoverHandler(
	distributor *DefaultDistributor,
) *FailoverHandler {
	return &FailoverHandler{distributor: distributor}
}

// CheckAndFailover detects unreachable hosts and reschedules
// their containers to healthy hosts.
func (f *FailoverHandler) CheckAndFailover(
	ctx context.Context,
) ([]FailoverAction, error) {
	d := f.distributor
	if d.opts.HostManager == nil || d.opts.Executor == nil {
		return nil, fmt.Errorf(
			"host manager and executor required for failover",
		)
	}

	d.mu.RLock()
	containers := make(
		[]DistributedContainer, len(d.containers),
	)
	copy(containers, d.containers)
	d.mu.RUnlock()

	// Check which remote hosts are unreachable.
	offlineHosts := make(map[string]bool)
	for _, dc := range containers {
		if dc.HostName == "" || dc.HostName == "local" {
			continue
		}
		if offlineHosts[dc.HostName] {
			continue
		}
		host, err := d.opts.HostManager.GetHost(dc.HostName)
		if err != nil || host == nil {
			offlineHosts[dc.HostName] = true
			continue
		}
		if !d.opts.Executor.IsReachable(ctx, *host) {
			offlineHosts[dc.HostName] = true
			d.opts.Logger.Warn(
				"failover: host %s is offline", dc.HostName,
			)
		}
	}

	if len(offlineHosts) == 0 {
		return nil, nil
	}

	// Collect containers from offline hosts.
	var actions []FailoverAction
	var rescheduleReqs []scheduler.ContainerRequirements
	for _, dc := range containers {
		if offlineHosts[dc.HostName] {
			rescheduleReqs = append(
				rescheduleReqs, dc.Requirement,
			)
			actions = append(actions, FailoverAction{
				ContainerName: dc.Requirement.Name,
				OriginalHost:  dc.HostName,
				Reason:        "host offline",
			})
		}
	}

	// CT-HARDEN-DIST2HARD DI-4 (ASSESSED, minimal honest fix): a reschedule
	// PLAN was computed below but never APPLIED anywhere — d.Status() kept
	// reporting these containers StateRunning on a host CheckAndFailover
	// itself just proved offline (StateMigrating was dead code, never
	// assigned). Full auto-reschedule (re-driving Distribute for
	// rescheduleReqs here) has broader blast-radius implications — it would
	// need to reconcile the OLD offline-host entries the same way
	// reconcileRelocations does, decide whether a container currently
	// mid-failover should still count toward capacity on other hosts, and
	// interact with a concurrent caller-driven Distribute/Rebalance — which
	// this hardening batch flags as a larger follow-up rather than
	// implementing here. What IS fixed now: the tracked state stops lying —
	// every affected StateRunning entry is flipped to StateMigrating so
	// Status() honestly reflects that these containers are being failed over,
	// instead of silently claiming they are still healthily running on a
	// host proven offline.
	if len(actions) > 0 {
		affected := make(map[string]bool, len(actions))
		for _, a := range actions {
			affected[a.ContainerName] = true
		}
		d.mu.Lock()
		for i := range d.containers {
			if affected[d.containers[i].Requirement.Name] &&
				d.containers[i].State == StateRunning {
				d.containers[i].State = StateMigrating
			}
		}
		d.mu.Unlock()
	}

	// Reschedule if possible.
	if d.opts.Scheduler != nil && len(rescheduleReqs) > 0 {
		plan, err := d.opts.Scheduler.ScheduleBatch(
			ctx, rescheduleReqs,
		)
		if err != nil {
			return actions, fmt.Errorf(
				"failover reschedule: %w", err,
			)
		}
		for i, decision := range plan.Decisions {
			if i < len(actions) {
				actions[i].NewHost = decision.HostName
			}
		}
	}

	return actions, nil
}

// FailoverAction describes a single container migration.
type FailoverAction struct {
	// ContainerName is the affected container.
	ContainerName string
	// OriginalHost is where the container was running.
	OriginalHost string
	// NewHost is where the container should be rescheduled.
	NewHost string
	// Reason explains why failover was triggered.
	Reason string
}

// DetectDegradedHosts returns hosts that are reachable but
// resource-constrained.
func DetectDegradedHosts(
	snapshots map[string]*remote.HostResources,
	cpuThreshold, memThreshold float64,
) []string {
	var degraded []string
	for name, snap := range snapshots {
		if snap.CPUPercent > cpuThreshold ||
			snap.MemoryPercent > memThreshold {
			degraded = append(degraded, name)
		}
	}
	return degraded
}
