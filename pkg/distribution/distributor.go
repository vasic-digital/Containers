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
	// Hand the caller an INDEPENDENT copy: d.containers keeps the original
	// backing array (which Undistribute() mutates under d.mu), so a caller
	// iterating the returned summary never aliases d.containers — no
	// cross-goroutine read/write race on an escaped summary, and
	// Undistribute() can never retroactively flip a previously-returned
	// summary's State (CT-HARDEN-DIST-2, escaped-alias half).
	summary.Containers = append([]DistributedContainer(nil), containers...)

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
	// Mark the detached containers Stopped while STILL holding the lock:
	// mutating them after Unlock raced with any in-package reader (e.g.
	// HealthCheckAll) that had captured the same live backing array via
	// d.containers (CT-HARDEN-DIST-2). The escaped-summary alias is handled
	// separately by the copy-on-publish in Distribute(), so a returned
	// summary never shares this backing array. Same intent, in-package race
	// closed.
	d.mu.Lock()
	containers := d.containers
	for i := range containers {
		containers[i].State = StateStopped
	}
	d.containers = nil
	d.mu.Unlock()

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
	// Copy under the read-lock (mirrors Status()) so the loop reads an
	// independent backing array, never the live d.containers that
	// Undistribute() mutates — no cross-method data race (CT-HARDEN-DIST-2).
	d.mu.RLock()
	containers := make([]DistributedContainer, len(d.containers))
	copy(containers, d.containers)
	d.mu.RUnlock()

	errors := make(map[string]error)
	for _, dc := range containers {
		if dc.State != StateRunning {
			continue
		}
		// Basic check: verify host is reachable.
		if d.opts.Executor != nil && dc.HostName != "" &&
			dc.HostName != "local" {
			// HostManager may be nil even when Executor is configured (they are
			// independently settable). Surface an honest error rather than
			// panicking on GetHost() (CT-HARDEN-DIST-1) — silently skipping the
			// check would be a §11.4.69 sink-side bluff (a running-but-
			// unverifiable container reported healthy).
			if d.opts.HostManager == nil {
				errors[dc.Requirement.Name] = fmt.Errorf(
					"host manager not configured",
				)
				continue
			}
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

// shellQuote wraps s in single quotes for safe interpolation into a POSIX
// shell command. Any embedded single quote is rendered as the canonical
// close-quote, backslash-escaped-quote, reopen-quote sequence (the ReplaceAll
// below). Registry/requirement-controlled fields (container Name / Image)
// reach the remote `run`/`rm` command as raw shell text, so they MUST pass
// through this before interpolation — an unescaped shell metacharacter in a
// registry-controlled value is remote command execution (§11.4, ATM-C056).
// It always wraps (never leaves a value bare), so an empty value renders as an
// empty single-quoted argument. Mirrors the proven pkg/emulator shellQuote;
// kept local because that precedent is unexported and to keep pkg/distribution
// decoupled (§11.4.28).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildRemoteRemoveCommand renders the pre-deploy `rm -f` command. The
// container Name is untrusted registry/requirement input, so it is shell
// quoted before interpolation. Pure function so the built string is unit
// testable without a live executor (§11.4.115).
func buildRemoteRemoveCommand(rt, name string) string {
	return fmt.Sprintf(
		"%s rm -f %s 2>/dev/null || true",
		rt, shellQuote(name),
	)
}

// buildRemoteRunCommand renders the remote `run -d` command. Name and Image
// are untrusted registry/requirement input and are shell quoted; the publish
// flags are already allowlist-safe (buildPublishFlags); rt is host-config.
// Pure function so the built string is unit testable without a live executor
// (§11.4.115).
func buildRemoteRunCommand(
	rt, name, image string, ports []scheduler.PortMapping,
) string {
	return fmt.Sprintf(
		"%s run -d --name %s%s %s",
		rt, shellQuote(name),
		buildPublishFlags(ports),
		shellQuote(image),
	)
}

func (d *DefaultDistributor) deployRemote(
	ctx context.Context, dc *DistributedContainer,
) error {
	if d.opts.Executor == nil {
		return fmt.Errorf("no remote executor configured")
	}
	// HostManager is an independently-settable Option (options.go); a caller
	// can configure Executor without HostManager. Guard exactly like
	// HostStatus() does — GetHost() on a nil interface panics
	// (CT-HARDEN-DIST-1).
	if d.opts.HostManager == nil {
		return fmt.Errorf("no host manager configured")
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

	removeCmd := buildRemoteRemoveCommand(rt, dc.Requirement.Name)
	d.opts.Executor.Execute(ctx, *host, removeCmd)

	cmd := buildRemoteRunCommand(
		rt, dc.Requirement.Name, dc.Requirement.Image,
		dc.Requirement.Ports,
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
