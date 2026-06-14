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
			for _, name := range remoteNames {
				if _, exists := bm.results[name]; exists {
					continue
				}
				bm.results[name] = &BootResult{
					Name:   name,
					Status: "distributed",
				}
				summary.Remote++
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

	// Phase 3: Health checks.
	bm.logger.Info("boot: starting health check phase")
	if bm.healthChecker != nil {
		healthErrors := bm.HealthCheckAll(ctx)
		for name, hcErr := range healthErrors {
			if hcErr != nil {
				ep := bm.endpoints[name]
				if ep.Required {
					// Decrement the counter the endpoint was actually
					// counted in, keyed off its PREVIOUS status — not a
					// blanket Remote/Started guess. A "discovered"
					// endpoint was counted in Discovered (never in
					// Started/Remote), so a flat Started-- here would
					// corrupt the summary into negative counts.
					if prev, ok := bm.results[name]; ok {
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
		return summary, fmt.Errorf(
			"boot: %d service(s) failed", summary.Failed,
		)
	}
	return summary, nil
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
	groups := bm.groupByCompose()
	var firstErr error

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
