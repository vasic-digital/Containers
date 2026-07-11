package boot

import (
	"context"
	"fmt"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/discovery"
	"digital.vasic.containers/pkg/endpoint"
	"digital.vasic.containers/pkg/event"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/metrics"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
)

// BootManager orchestrates the startup of all configured service
// endpoints. It performs discovery, starts compose groups, runs
// health checks, and produces a summary.
type BootManager struct {
	endpoints     map[string]endpoint.ServiceEndpoint
	orchestrator  compose.ComposeOrchestrator
	healthChecker health.HealthChecker
	discoverer    discovery.Discoverer
	runtime       runtime.ContainerRuntime
	distributor   Distributor
	hostManager   remote.HostManager
	scheduler     scheduler.Scheduler
	logger        logging.Logger
	metrics       metrics.MetricsCollector
	eventBus      event.EventBus
	projectDir    string
	results       map[string]*BootResult
}

// NewBootManager creates a BootManager for the given endpoints.
// Use With* options to inject dependencies.
func NewBootManager(
	endpoints map[string]endpoint.ServiceEndpoint,
	opts ...BootManagerOption,
) *BootManager {
	bm := &BootManager{
		endpoints: endpoints,
		results:   make(map[string]*BootResult),
	}
	for _, opt := range opts {
		opt(bm)
	}
	if bm.logger == nil {
		bm.logger = logging.NopLogger{}
	}
	if bm.metrics == nil {
		bm.metrics = &metrics.NoopCollector{}
	}
	return bm
}

// BootAll runs the full boot sequence: discovery, compose up,
// health checks, and returns a summary. It returns an error only
// when a required service fails.
func (bm *BootManager) BootAll(
	ctx context.Context,
) (*BootSummary, error) {
	start := time.Now()
	summary := &BootSummary{
		Results: make(map[string]*BootResult),
	}

	// BOOT-5: reset the per-manager results map at entry so BootAll is
	// re-runnable. Previously bm.results was allocated once in the
	// constructor and never reset, and summary.Results aliased it — a
	// 2nd BootAll saw stale run-1 entries (the Phase-2 already-recorded
	// guard skipped every run-1 endpoint, under-counting, while stale
	// "failed" results leaked into run-2's summary and disagreed with
	// the fresh counters). A fresh map makes each run self-consistent.
	bm.results = make(map[string]*BootResult)

	// BOOT-2: track compose groups successfully Up'd, in order, so a
	// required/compose/ctx-cancel failure can tear them down (reverse
	// order) before returning — no partial-boot leak.
	var bootedProjects []compose.ComposeProject

	if bm.eventBus != nil {
		bm.eventBus.Publish(ctx, event.NewEvent(
			event.EventBootStarted, "boot", "all",
		))
	}

	// Phase 1: Discovery for remote/discoverable endpoints.
	// Also marks disabled endpoints as skipped.
	bm.logger.Info("boot: starting discovery phase")
	for name, ep := range bm.endpoints {
		if !ep.Enabled {
			bm.results[name] = &BootResult{
				Name:   name,
				Status: "skipped",
			}
			summary.Skipped++
			continue
		}

		if ep.DiscoveryEnabled && bm.discoverer != nil {
			found, err := bm.discoverer.Discover(ctx,
				discovery.DiscoveryTarget{
					Name:    name,
					Host:    ep.Host,
					Port:    ep.Port,
					Method:  ep.DiscoveryMethod,
					Timeout: ep.DiscoveryTimeout,
				},
			)
			if err == nil && found {
				bm.results[name] = &BootResult{
					Name:   name,
					Status: "discovered",
				}
				summary.Discovered++
				bm.logger.Info(
					"boot: discovered %s at %s:%s",
					name, ep.Host, ep.Port,
				)
				continue
			}
		}
	}

	// BOOT-2: honor cancellation between phases.
	if err := ctx.Err(); err != nil {
		bm.rollback(ctx, bootedProjects)
		return summary, err
	}

	// Phase 2: Group remaining by compose file and start.
	bm.logger.Info("boot: starting compose phase")
	composeGroups := bm.groupByCompose()
	for file, group := range composeGroups {
		if bm.orchestrator == nil {
			break
		}
		// Use the first profile found in the group.
		profile := ""
		for _, ep := range group {
			if ep.Profile != "" {
				profile = ep.Profile
				break
			}
		}

		bm.logger.Info("boot: starting compose %s", file)
		svcStart := time.Now()

		project := compose.ComposeProject{
			File:    file,
			Profile: profile,
		}
		if err := bm.orchestrator.Up(
			ctx, project,
		); err != nil {
			for name := range group {
				bm.results[name] = &BootResult{
					Name:     name,
					Status:   "failed",
					Duration: time.Since(svcStart),
					Error:    err,
				}
				summary.Failed++
			}
			continue
		}
		// BOOT-2: this group is now running — record it for rollback.
		bootedProjects = append(bootedProjects, project)

		for name := range group {
			if _, already := bm.results[name]; already {
				continue
			}
			ep := bm.endpoints[name]
			status := "started"
			if ep.Remote {
				status = "remote"
			}
			bm.results[name] = &BootResult{
				Name:     name,
				Status:   status,
				Duration: time.Since(svcStart),
			}
			if status == "remote" {
				summary.Remote++
			} else {
				summary.Started++
			}
		}
	}

	// BOOT-2: honor cancellation between phases.
	if err := ctx.Err(); err != nil {
		bm.rollback(ctx, bootedProjects)
		return summary, err
	}

	// Phase 2.5: Distribute remote endpoints via distributor.
	if bm.distributor != nil {
		var remoteNames []string
		for name, ep := range bm.endpoints {
			if _, exists := bm.results[name]; exists {
				continue
			}
			if ep.Remote && ep.Enabled {
				remoteNames = append(remoteNames, name)
			}
		}
		if len(remoteNames) > 0 {
			bm.logger.Info(
				"boot: distributing %d remote endpoints",
				len(remoteNames),
			)
			deployed, distErr := bm.distributor.DistributeEndpoints(
				ctx, remoteNames,
			)
			// BOOT-1: the distributor reports only an aggregate count of
			// successfully-deployed containers, not which names. Attribute
			// by count: mark exactly `deployed` endpoints distributed and
			// the (len-deployed) shortfall FAILED — never mark an
			// undeployed endpoint "distributed"/Remote++ (the swallowed-
			// failure bug). Counts + the pass/fail verdict are exact; the
			// specific failed NAME in a partial deploy is best-effort given
			// the interface. A shortfall on a REQUIRED remote endpoint is
			// propagated into summary.Failed (→ the returned error), like a
			// required health-check failure in Phase 3.
			if deployed < 0 {
				deployed = 0
			}
			if deployed > len(remoteNames) {
				deployed = len(remoteNames)
			}
			for i, name := range remoteNames {
				if _, exists := bm.results[name]; exists {
					continue
				}
				if i < deployed {
					bm.results[name] = &BootResult{
						Name:   name,
						Status: "distributed",
					}
					summary.Remote++
					continue
				}
				// Shortfall: this endpoint was NOT deployed.
				depErr := distErr
				if depErr == nil {
					depErr = fmt.Errorf(
						"boot: remote endpoint %q not deployed "+
							"(%d/%d deployed)",
						name, deployed, len(remoteNames),
					)
				}
				bm.results[name] = &BootResult{
					Name:   name,
					Status: "failed",
					Error:  depErr,
				}
				if bm.endpoints[name].Required {
					summary.Failed++
				}
			}
			if distErr != nil {
				bm.logger.Warn(
					"boot: distribution partial: %d/%d deployed: %v",
					deployed, len(remoteNames), distErr,
				)
			} else {
				bm.logger.Info(
					"boot: distributed %d remote endpoints",
					deployed,
				)
			}
		}
	}

	// Handle enabled endpoints without a compose file.
	// Note: Disabled endpoints are already handled in Phase 1.
	for name, ep := range bm.endpoints {
		if _, exists := bm.results[name]; exists {
			continue
		}
		if ep.Remote {
			bm.results[name] = &BootResult{
				Name:   name,
				Status: "remote",
			}
			summary.Remote++
		}
	}

	// BOOT-2: honor cancellation between phases.
	if err := ctx.Err(); err != nil {
		bm.rollback(ctx, bootedProjects)
		return summary, err
	}

	// Phase 3: Health checks.
	bm.logger.Info("boot: starting health check phase")
	if bm.healthChecker != nil {
		healthErrors := bm.HealthCheckAll(ctx)
		for name, hcErr := range healthErrors {
			if hcErr == nil {
				continue
			}
			ep := bm.endpoints[name]
			if !ep.Required {
				continue
			}
			prev, ok := bm.results[name]
			// BOOT-4: an endpoint whose compose Up already failed keeps
			// Status "failed" here (still Enabled, so HealthCheckAll
			// re-probes it → unhealthy). Skip it: re-recording would
			// (a) clobber the compose-up root-cause error with a health
			// error and (b) double-count it in summary.Failed (Failed==2
			// for one endpoint). Its original failure already counts.
			if ok && prev.Status == "failed" {
				continue
			}
			// Decrement the counter the endpoint was actually counted in,
			// keyed off its PREVIOUS status — not a blanket Remote/Started
			// guess. A "discovered" endpoint was counted in Discovered
			// (never in Started/Remote), so a flat Started-- here would
			// corrupt the summary into negative counts.
			if ok {
				switch prev.Status {
				case "started":
					summary.Started--
				case "remote", "distributed":
					summary.Remote--
				case "discovered":
					summary.Discovered--
				}
			}
			bm.results[name] = &BootResult{
				Name:   name,
				Status: "failed",
				Error:  hcErr,
			}
			summary.Failed++
		}
	}

	summary.Results = bm.results
	summary.TotalDuration = time.Since(start)

	if bm.eventBus != nil {
		bm.eventBus.Publish(ctx, event.NewEvent(
			event.EventBootCompleted, "boot", "all",
		).WithData("summary", summary.String()))
	}

	bm.logger.Info("boot: %s", summary.String())
	bm.metrics.ObserveBootDuration(summary.TotalDuration)

	if summary.HasFailures() {
		// BOOT-2: a required service failed — tear down the compose groups
		// already booted this run so BootAll never returns an error while
		// leaving a partial boot running.
		bm.rollback(ctx, bootedProjects)
		return summary, fmt.Errorf(
			"boot: %d service(s) failed", summary.Failed,
		)
	}
	return summary, nil
}

// rollback tears down the given compose groups (already Up'd this boot)
// in reverse order. It is best-effort cleanup on an already-failing or
// cancelled boot path (§11.4.14 quiescent-state): errors are logged, not
// returned. A detached context is used so teardown still runs even when
// the boot ctx was cancelled.
func (bm *BootManager) rollback(
	ctx context.Context, booted []compose.ComposeProject,
) {
	if bm.orchestrator == nil || len(booted) == 0 {
		return
	}
	downCtx := context.WithoutCancel(ctx)
	for i := len(booted) - 1; i >= 0; i-- {
		bm.logger.Info(
			"boot: rollback ComposeDown %s", booted[i].File,
		)
		if err := bm.orchestrator.Down(
			downCtx, booted[i],
		); err != nil {
			bm.logger.Warn(
				"boot: rollback ComposeDown %s failed: %v",
				booted[i].File, err,
			)
		}
	}
}

// HealthCheckAll checks all enabled endpoints and returns errors
// keyed by name. A nil value means the check passed.
func (bm *BootManager) HealthCheckAll(
	ctx context.Context,
) map[string]error {
	errors := make(map[string]error)
	if bm.healthChecker == nil {
		return errors
	}

	var targets []health.HealthTarget
	var names []string
	for name, ep := range bm.endpoints {
		if !ep.Enabled {
			continue
		}
		targets = append(targets, health.HealthTarget{
			Name:     name,
			Host:     ep.Host,
			Port:     ep.Port,
			URL:      ep.URL,
			Type:     health.HealthType(ep.HealthType),
			Path:     ep.HealthPath,
			Timeout:  ep.Timeout,
			Required: ep.Required,
		})
		names = append(names, name)
	}

	results := bm.healthChecker.CheckAll(ctx, targets)
	for i, result := range results {
		if !result.Healthy {
			errors[names[i]] = fmt.Errorf(
				"health check failed: %s", result.Error,
			)
		}
	}
	return errors
}

// Shutdown stops all compose-managed services.
func (bm *BootManager) Shutdown(ctx context.Context) error {
	if bm.eventBus != nil {
		bm.eventBus.Publish(ctx, event.NewEvent(
			event.EventShutdownStarted, "boot", "all",
		))
	}

	bm.logger.Info("boot: shutting down services")
	var firstErr error

	// BOOT-3: tear down distributed remote endpoints (containers,
	// tunnels, volumes) FIRST — symmetric with BootAll Phase 2.5, which
	// distributes via bm.distributor. Without this, remote state
	// distributed during boot leaked because Shutdown only ran
	// ComposeDown. distribution.DefaultDistributor.Undistribute already
	// exists, so the real distributor satisfies the extended interface.
	if bm.distributor != nil {
		if err := bm.distributor.Undistribute(ctx); err != nil {
			bm.logger.Warn("boot: undistribute failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	groups := bm.groupByCompose()

	for file, group := range groups {
		if bm.orchestrator == nil {
			break
		}
		profile := ""
		for _, ep := range group {
			if ep.Profile != "" {
				profile = ep.Profile
				break
			}
		}

		project := compose.ComposeProject{
			File:    file,
			Profile: profile,
		}
		if err := bm.orchestrator.Down(
			ctx, project,
		); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if bm.eventBus != nil {
		bm.eventBus.Publish(ctx, event.NewEvent(
			event.EventShutdownCompleted, "boot", "all",
		))
	}

	return firstErr
}

// groupByCompose returns endpoints grouped by their compose file.
// Remote endpoints and endpoints without compose files are excluded.
func (bm *BootManager) groupByCompose() map[string]map[string]endpoint.ServiceEndpoint {
	groups := make(
		map[string]map[string]endpoint.ServiceEndpoint,
	)
	for name, ep := range bm.endpoints {
		if ep.ComposeFile == "" || !ep.Enabled {
			continue
		}
		if _, exists := groups[ep.ComposeFile]; !exists {
			groups[ep.ComposeFile] = make(
				map[string]endpoint.ServiceEndpoint,
			)
		}
		groups[ep.ComposeFile][name] = ep
	}
	return groups
}
