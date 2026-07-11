package distribution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
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
	var ctxErr error
	for _, decision := range plan.Decisions {
		// CT-HARDEN-DIST-HARD DIST-4: honour context cancellation. The deploy
		// loop previously never checked ctx.Err(), so a cancelled/expired
		// context still issued every remote rm/run and every local Start. Once
		// ctx is cancelled, mark this and every remaining decision failed (no
		// command is issued to any host) and return the partial summary with the
		// ctx error; containers deployed BEFORE cancellation stay tracked in
		// d.containers so Undistribute() can still tear them down.
		if err := ctx.Err(); err != nil {
			ctxErr = err
			failed := DistributedContainer{
				Requirement: decision.Requirement,
				HostName:    decision.HostName,
				State:       StateFailed,
				Error:       err.Error(),
				TunnelPorts: make(map[string]string),
			}
			summary.FailedContainers++
			containers = append(containers, failed)
			continue
		}
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

	// Publish the new placement and capture the PRIOR one so a container that
	// relocated (or was dropped) can be torn down on its OLD host. deployRemote
	// only rm-f's the NEW host, so without this a re-Distribute/Rebalance moving
	// `foo` from host A to host B leaks `foo` still running on A (CT-HARDEN-DIST-
	// HARD DIST-2, a §11.4.69 sink-side leak). `prev` no longer aliases
	// d.containers after this critical section, so reading it lock-free below is
	// race-safe (in-package readers copy under the lock).
	d.mu.Lock()
	prev := d.containers
	d.containers = containers
	d.mu.Unlock()

	d.reconcileRelocations(ctx, prev, containers)

	d.opts.Logger.Info(
		"distribution complete: %d local, %d remote, %d failed "+
			"in %s",
		summary.LocalContainers, summary.RemoteContainers,
		summary.FailedContainers, summary.Duration,
	)

	return summary, ctxErr
}

// Undistribute stops all distributed containers.
func (d *DefaultDistributor) Undistribute(
	ctx context.Context,
) error {
	// Snapshot the tracked containers (a COPY, so the pre-stop State survives for
	// the teardown predicate below) and mark the live array Stopped while STILL
	// holding the lock: mutating after Unlock raced with any in-package reader
	// (e.g. HealthCheckAll) that had captured the same live backing array via
	// d.containers (CT-HARDEN-DIST-2). The escaped-summary alias is handled
	// separately by the copy-on-publish in Distribute(), so a returned summary
	// never shares this backing array. d.containers is niled under the lock, so
	// the snapshot is exclusively ours once unlocked.
	d.mu.Lock()
	snapshot := make([]DistributedContainer, len(d.containers))
	copy(snapshot, d.containers)
	for i := range d.containers {
		d.containers[i].State = StateStopped
	}
	d.containers = nil
	d.mu.Unlock()

	// CT-HARDEN-DIST-HARD DIST-1: actually tear the containers DOWN on their
	// hosts before dropping tracking. The prior code only flipped State to
	// Stopped in memory while every container kept RUNNING on its host — a
	// §11.4.69 sink-side bluff (State reports stopped, the host still runs it).
	// Only containers that reached StateRunning have a live deployment to remove;
	// failed/scheduled ones were never created. Per-container teardown errors are
	// aggregated (returned) but never abort the sweep — CloseAll/UnmountAll below
	// still run, and a double-Undistribute is safe (empty snapshot ⇒ no-op).
	var teardownErrs []error
	for _, dc := range snapshot {
		if dc.State != StateRunning {
			continue
		}
		if err := d.teardownContainer(ctx, dc); err != nil {
			teardownErrs = append(teardownErrs, err)
			d.opts.Logger.Error(
				"undistribute: teardown %s on %s failed: %v",
				dc.Requirement.Name, dc.HostName, err,
			)
		}
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
		len(snapshot),
	)
	return errors.Join(teardownErrs...)
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
	// CT-HARDEN-DIST-HARD DIST-3: a nil LocalRuntime is a MISCONFIGURATION, not a
	// silent success. The prior `return nil` let the caller mark the container
	// StateRunning + LocalContainers++ though nothing was deployed — a §11.4.69
	// sink-side bluff, and asymmetric with deployRemote (which errors "no remote
	// executor configured"). Restore symmetry: fail honestly.
	if d.opts.LocalRuntime == nil {
		return fmt.Errorf("no local runtime configured")
	}

	d.opts.Logger.Info("deploying %s locally",
		dc.Requirement.Name,
	)
	// The ContainerRuntime.Start contract takes a container ID/NAME (its doc
	// comment: "identified by its ID or name") — never an image. The prior code
	// passed dc.Requirement.Image, a wrong-arg contract violation; pass the
	// requirement Name (the container's identity, consistent with the remote
	// --name/rm path and teardownContainer). KNOWN LIMITATION (§11.4.6, out of
	// scope for this pkg/distribution-only batch): ContainerRuntime exposes NO
	// create/run-from-image step, so local Start succeeds only for a pre-existing
	// container — fully closing the local create-from-image gap needs a
	// runtime-interface change in another package, which this batch must not
	// make.
	return d.opts.LocalRuntime.Start(
		ctx, dc.Requirement.Name,
	)
}

// normHost normalises a placement host name so the empty string and the
// sentinel "local" compare equal — both denote the local host (mirrors
// scheduler.PlacementDecision.IsLocal).
func normHost(h string) string {
	if h == "" {
		return "local"
	}
	return h
}

// teardownContainer issues a best-effort stop+remove of a single tracked
// container on the host where it was placed. Remote: `rt rm -f <name>` via the
// Executor (mirrors deployRemote's pre-deploy rm — force-remove stops then
// deletes, and `|| true` keeps it idempotent for an already-absent container).
// Local: LocalRuntime.Stop then a force Remove. Used by both Undistribute()
// (DIST-1) and reconcileRelocations() (DIST-2).
//
// HONEST BOUNDARY (§11.4.107): a unit test with no live runtime can only assert
// the teardown COMMAND is ISSUED to the seam (Executor / LocalRuntime); it
// cannot confirm a real container actually died.
func (d *DefaultDistributor) teardownContainer(
	ctx context.Context, dc DistributedContainer,
) error {
	name := dc.Requirement.Name
	if normHost(dc.HostName) == "local" {
		if d.opts.LocalRuntime == nil {
			return nil
		}
		var errs []error
		if err := d.opts.LocalRuntime.Stop(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
		}
		if err := d.opts.LocalRuntime.Remove(
			ctx, name, runtime.WithForceRemove(true),
		); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
		}
		return errors.Join(errs...)
	}

	if d.opts.Executor == nil {
		return fmt.Errorf(
			"teardown %s on %s: no remote executor configured",
			name, dc.HostName,
		)
	}
	// Mirror deployRemote's nil-HostManager guard (GetHost on a nil interface
	// panics — CT-HARDEN-DIST-1).
	if d.opts.HostManager == nil {
		return fmt.Errorf(
			"teardown %s on %s: no host manager configured",
			name, dc.HostName,
		)
	}
	host, err := d.opts.HostManager.GetHost(dc.HostName)
	if err != nil || host == nil {
		return fmt.Errorf("teardown %s: host %s not found", name, dc.HostName)
	}
	rt := host.Runtime
	if rt == "" {
		rt = "docker"
	}
	removeCmd := buildRemoteRemoveCommand(rt, name)
	if _, err := d.opts.Executor.Execute(ctx, *host, removeCmd); err != nil {
		return fmt.Errorf("teardown %s on %s: %w", name, dc.HostName, err)
	}
	return nil
}

// reconcileRelocations tears down containers stranded on their OLD host after a
// re-Distribute/Rebalance moved or dropped them. deployRemote only rm-f's the
// NEW host, so without this a container relocating from host A to host B keeps
// running on A (CT-HARDEN-DIST-HARD DIST-2, a §11.4.69 sink-side leak). Only
// previously-RUNNING placements are torn down (failed/scheduled ones were never
// created); a name kept on the SAME host is left alone because deployRemote
// already rm-f'd it in place. Best-effort — per-container errors are logged,
// never abort the batch.
func (d *DefaultDistributor) reconcileRelocations(
	ctx context.Context, prev, current []DistributedContainer,
) {
	if len(prev) == 0 {
		return
	}
	newHost := make(map[string]string, len(current))
	for _, dc := range current {
		newHost[dc.Requirement.Name] = normHost(dc.HostName)
	}
	for _, old := range prev {
		if old.State != StateRunning {
			continue
		}
		if h, ok := newHost[old.Requirement.Name]; ok &&
			h == normHost(old.HostName) {
			continue // same host: deployRemote already rm-f'd it in place
		}
		if err := d.teardownContainer(ctx, old); err != nil {
			d.opts.Logger.Error(
				"reconcile: teardown stale %s on %s failed: %v",
				old.Requirement.Name, old.HostName, err,
			)
		}
	}
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
