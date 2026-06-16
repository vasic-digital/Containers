package distribution

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// Distributor defines the interface for distributing containers
// across local and remote hosts.
type Distributor interface {
	// Distribute places and deploys containers across hosts.
	Distribute(
		ctx context.Context,
		reqs []scheduler.ContainerRequirements,
	) (*DistributionSummary, error)

	// Undistribute stops and removes all distributed containers.
	Undistribute(ctx context.Context) error

	// Status returns the current state of all distributed
	// containers.
	Status(ctx context.Context) []DistributedContainer

	// HealthCheckAll checks all distributed containers.
	HealthCheckAll(ctx context.Context) map[string]error

	// Rebalance evaluates and redistributes containers.
	Rebalance(ctx context.Context) (*DistributionSummary, error)

	// HostStatus returns resource info for a specific host.
	HostStatus(
		ctx context.Context, hostName string,
	) (*remote.HostResources, error)
}

// DefaultDistributor implements Distributor by composing
// scheduler, remote executor, tunnel manager, and volume manager.
type DefaultDistributor struct {
	mu         sync.RWMutex
	opts       Options
	containers []DistributedContainer
}

// NewDistributor creates a DefaultDistributor.
func NewDistributor(opts ...Option) *DefaultDistributor {
	o := ApplyOptions(opts)
	return &DefaultDistributor{
		opts: o,
	}
}

// Distribute places and deploys containers.
func (d *DefaultDistributor) Distribute(
	ctx context.Context,
	reqs []scheduler.ContainerRequirements,
) (*DistributionSummary, error) {
	start := time.Now()

	if d.opts.Scheduler == nil {
		return nil, fmt.Errorf("scheduler not configured")
	}

	// Phase 1: Schedule placement.
	d.opts.Logger.Info(
		"distribution: scheduling %d containers", len(reqs),
	)
	plan, err := d.opts.Scheduler.ScheduleBatch(ctx, reqs)
	if err != nil {
		return nil, fmt.Errorf("schedule: %w", err)
	}

	summary := &DistributionSummary{
		TotalContainers: len(reqs),
		HostUtilization: plan.HostSnapshots,
	}

	// Phase 2-7: Deploy each container.
	containers := make([]DistributedContainer, 0, len(plan.Decisions))
	for _, decision := range plan.Decisions {
		dc := DistributedContainer{
			Requirement: decision.Requirement,
			HostName:    decision.HostName,
			State:       StateScheduled,
			TunnelPorts: make(map[string]string),
		}

		if decision.Score == 0 {
			dc.State = StateFailed
			dc.Error = decision.Reason
			summary.FailedContainers++
			containers = append(containers, dc)
			continue
		}

		// Deploy.
		dc.State = StateDeploying
		if err := d.deployContainer(ctx, &dc); err != nil {
			dc.State = StateFailed
			dc.Error = err.Error()
			summary.FailedContainers++
			d.opts.Logger.Error(
				"deploy %s to %s failed: %v",
				dc.Requirement.Name, dc.HostName, err,
			)
		} else {
			dc.State = StateRunning
			dc.DeployedAt = time.Now()
			if decision.IsLocal() {
				summary.LocalContainers++
			} else {
				summary.RemoteContainers++
			}
		}

		containers = append(containers, dc)
	}

	summary.Duration = time.Since(start)
	summary.Containers = containers

	d.mu.Lock()
	d.containers = containers
	d.mu.Unlock()

	d.opts.Logger.Info(
		"distribution complete: %d local, %d remote, %d failed "+
			"in %s",
		summary.LocalContainers, summary.RemoteContainers,
		summary.FailedContainers, summary.Duration,
	)

	return summary, nil
}

// Undistribute stops all distributed containers.
func (d *DefaultDistributor) Undistribute(
	ctx context.Context,
) error {
	d.mu.Lock()
	containers := d.containers
	d.containers = nil
	d.mu.Unlock()

	for i := range containers {
		containers[i].State = StateStopped
	}

	// Close tunnels.
	if d.opts.TunnelManager != nil {
		_ = d.opts.TunnelManager.CloseAll()
	}

	// Unmount volumes.
	if d.opts.VolumeManager != nil {
		_ = d.opts.VolumeManager.UnmountAll(ctx)
	}

	d.opts.Logger.Info("undistributed %d containers",
		len(containers),
	)
	return nil
}

// Status returns all distributed containers.
func (d *DefaultDistributor) Status(
	ctx context.Context,
) []DistributedContainer {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make(
		[]DistributedContainer, len(d.containers),
	)
	copy(result, d.containers)
	return result
}

// HealthCheckAll checks all distributed containers.
func (d *DefaultDistributor) HealthCheckAll(
	ctx context.Context,
) map[string]error {
	d.mu.RLock()
	containers := d.containers
	d.mu.RUnlock()

	errors := make(map[string]error)
	for _, dc := range containers {
		if dc.State != StateRunning {
			continue
		}
		// Basic check: verify host is reachable.
		if d.opts.Executor != nil && dc.HostName != "" &&
			dc.HostName != "local" {
			host, err := d.opts.HostManager.GetHost(dc.HostName)
			if err != nil || host == nil {
				errors[dc.Requirement.Name] = fmt.Errorf(
					"host %s not found", dc.HostName,
				)
				continue
			}
			if !d.opts.Executor.IsReachable(ctx, *host) {
				errors[dc.Requirement.Name] = fmt.Errorf(
					"host %s unreachable", dc.HostName,
				)
			}
		}
	}
	return errors
}

// Rebalance evaluates and suggests redistribution.
func (d *DefaultDistributor) Rebalance(
	ctx context.Context,
) (*DistributionSummary, error) {
	if d.opts.Scheduler == nil {
		return nil, fmt.Errorf("scheduler not configured")
	}

	d.mu.RLock()
	reqs := make(
		[]scheduler.ContainerRequirements, len(d.containers),
	)
	for i, dc := range d.containers {
		reqs[i] = dc.Requirement
	}
	d.mu.RUnlock()

	return d.Distribute(ctx, reqs)
}

// HostStatus returns resource info for a specific host.
func (d *DefaultDistributor) HostStatus(
	ctx context.Context, hostName string,
) (*remote.HostResources, error) {
	if d.opts.HostManager == nil {
		return nil, fmt.Errorf("host manager not configured")
	}
	return d.opts.HostManager.ProbeHost(ctx, hostName)
}

// DistributeEndpoints distributes the named endpoints across
// remote hosts using the configured scheduler. Each name is
// converted to a ContainerRequirements with a minimal default
// image. Returns the number of successfully deployed containers.
// This method satisfies the boot.Distributor interface.
func (d *DefaultDistributor) DistributeEndpoints(
	ctx context.Context, names []string,
) (int, error) {
	reqs := make([]scheduler.ContainerRequirements, len(names))
	for i, name := range names {
		reqs[i] = scheduler.ContainerRequirements{
			Name:  name,
			Image: name, // Use name as image; caller can override.
		}
	}

	summary, err := d.Distribute(ctx, reqs)
	if err != nil {
		return 0, err
	}

	deployed := summary.LocalContainers + summary.RemoteContainers
	return deployed, nil
}

func (d *DefaultDistributor) deployContainer(
	ctx context.Context, dc *DistributedContainer,
) error {
	if dc.HostName == "" || dc.HostName == "local" {
		return d.deployLocal(ctx, dc)
	}
	return d.deployRemote(ctx, dc)
}

func (d *DefaultDistributor) deployLocal(
	ctx context.Context, dc *DistributedContainer,
) error {
	if d.opts.LocalRuntime == nil {
		return nil
	}

	d.opts.Logger.Info("deploying %s locally",
		dc.Requirement.Name,
	)
	return d.opts.LocalRuntime.Start(
		ctx, dc.Requirement.Image,
	)
}

// buildPublishFlags renders the `-p host:container[/proto]` fragment (each with
// a leading space) for the remote `run` command. Mappings with a non-positive
// ContainerPort are skipped; an empty/zero HostPort lets the runtime pick an
// ephemeral host port. Empty input returns "".
func buildPublishFlags(ports []scheduler.PortMapping) string {
	var b strings.Builder
	for _, p := range ports {
		if p.ContainerPort <= 0 {
			continue
		}
		// Allowlist the protocol — it is interpolated into the remote shell
		// `run` command, so an unvalidated value would be a command-injection
		// vector. HostPort/ContainerPort are ints (%d) and inherently safe.
		var proto string
		switch strings.ToLower(p.Protocol) {
		case "", "tcp":
			proto = "tcp"
		case "udp":
			proto = "udp"
		case "sctp":
			proto = "sctp"
		default:
			continue // skip mappings with an unrecognized protocol
		}
		if p.HostPort > 0 {
			fmt.Fprintf(&b, " -p %d:%d/%s", p.HostPort, p.ContainerPort, proto)
		} else {
			fmt.Fprintf(&b, " -p %d/%s", p.ContainerPort, proto)
		}
	}
	return b.String()
}

func (d *DefaultDistributor) deployRemote(
	ctx context.Context, dc *DistributedContainer,
) error {
	if d.opts.Executor == nil {
		return fmt.Errorf("no remote executor configured")
	}

	host, err := d.opts.HostManager.GetHost(dc.HostName)
	if err != nil || host == nil {
		return fmt.Errorf(
			"host %s not found", dc.HostName,
		)
	}

	rt := host.Runtime
	if rt == "" {
		rt = "docker"
	}

	removeCmd := fmt.Sprintf(
		"%s rm -f %s 2>/dev/null || true",
		rt, dc.Requirement.Name,
	)
	d.opts.Executor.Execute(ctx, *host, removeCmd)

	cmd := fmt.Sprintf(
		"%s run -d --name %s%s %s",
		rt, dc.Requirement.Name,
		buildPublishFlags(dc.Requirement.Ports),
		dc.Requirement.Image,
	)

	d.opts.Logger.Info("deploying %s on %s: %s",
		dc.Requirement.Name, dc.HostName, cmd,
	)

	result, err := d.opts.Executor.Execute(ctx, *host, cmd)
	if err != nil {
		return fmt.Errorf("deploy on %s: %w", dc.HostName, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"deploy on %s: exit %d: %s",
			dc.HostName, result.ExitCode, result.Stderr,
		)
	}

	return nil
}
