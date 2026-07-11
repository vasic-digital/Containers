package boot

import (
	"context"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/discovery"
	"digital.vasic.containers/pkg/event"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/metrics"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
)

// Distributor is a local interface satisfied by
// distribution.DefaultDistributor. Defined here to avoid a
// circular import between boot and distribution.
type Distributor interface {
	// DistributeEndpoints distributes remote endpoints and returns
	// the number of successfully deployed containers.
	DistributeEndpoints(ctx context.Context, names []string) (int, error)
	// Undistribute tears down all distributed remote endpoints
	// (containers, tunnels, volumes). Called from Shutdown so remote
	// state distributed during BootAll Phase 2.5 does not leak
	// (BOOT-3). distribution.DefaultDistributor already implements
	// this, so the real type satisfies the extended interface.
	Undistribute(ctx context.Context) error
}

// BootManagerOption configures a BootManager during construction.
type BootManagerOption func(*BootManager)

// WithRuntime sets the container runtime.
func WithRuntime(r runtime.ContainerRuntime) BootManagerOption {
	return func(bm *BootManager) {
		bm.runtime = r
	}
}

// WithLogger sets the logger.
func WithLogger(l logging.Logger) BootManagerOption {
	return func(bm *BootManager) {
		bm.logger = l
	}
}

// WithMetrics sets the metrics collector.
func WithMetrics(m metrics.MetricsCollector) BootManagerOption {
	return func(bm *BootManager) {
		bm.metrics = m
	}
}

// WithEventBus sets the event bus.
func WithEventBus(bus event.EventBus) BootManagerOption {
	return func(bm *BootManager) {
		bm.eventBus = bus
	}
}

// WithOrchestrator sets the compose orchestrator.
func WithOrchestrator(
	o compose.ComposeOrchestrator,
) BootManagerOption {
	return func(bm *BootManager) {
		bm.orchestrator = o
	}
}

// WithHealthChecker sets the health checker.
func WithHealthChecker(h health.HealthChecker) BootManagerOption {
	return func(bm *BootManager) {
		bm.healthChecker = h
	}
}

// WithProjectDir sets the project root directory used for
// resolving relative compose file paths.
func WithProjectDir(dir string) BootManagerOption {
	return func(bm *BootManager) {
		bm.projectDir = dir
	}
}

// WithDiscoverer sets the service discoverer.
func WithDiscoverer(d discovery.Discoverer) BootManagerOption {
	return func(bm *BootManager) {
		bm.discoverer = d
	}
}

// WithDistributor sets the container distributor for remote
// host placement. When set, remote endpoints are delegated to
// the distributor instead of only health-checking.
func WithDistributor(d Distributor) BootManagerOption {
	return func(bm *BootManager) {
		bm.distributor = d
	}
}

// WithHostManager sets the remote host manager for the boot
// manager.
func WithHostManager(hm remote.HostManager) BootManagerOption {
	return func(bm *BootManager) {
		bm.hostManager = hm
	}
}

// WithScheduler sets the container scheduler for the boot
// manager.
func WithScheduler(s scheduler.Scheduler) BootManagerOption {
	return func(bm *BootManager) {
		bm.scheduler = s
	}
}
