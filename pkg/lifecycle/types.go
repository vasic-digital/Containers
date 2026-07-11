package lifecycle

import (
	"time"

	"digital.vasic.containers/pkg/health"
)

// ServiceLifecycleStatus describes the current state of a managed
// service including health, concurrency, and timing information.
type ServiceLifecycleStatus struct {
	// Name is the registered service name.
	Name string
	// State is the current lifecycle state
	// (e.g., "stopped", "starting", "running", "stopping").
	State string
	// Healthy indicates whether the last health check passed.
	Healthy bool
	// ActiveUsers is the number of concurrent users holding an
	// Acquire lease.
	ActiveUsers int
	// LastStarted is when the service was last started.
	LastStarted time.Time
	// LastStopped is when the service was last stopped.
	LastStopped time.Time
	// LastAcquired is when the most recent Acquire call completed.
	LastAcquired time.Time
}

// ServiceSpec defines the configuration for a lifecycle-managed
// service.
type ServiceSpec struct {
	// Name is the unique service identifier.
	Name string
	// LazyBoot defers container start until the first Acquire.
	LazyBoot bool
	// IdleTimeout stops the service after this duration of
	// inactivity. Zero disables idle shutdown.
	IdleTimeout time.Duration
	// MaxConcurrent limits the number of parallel Acquire holders.
	// Zero means unlimited.
	MaxConcurrent int
	// Priority is a reserved ordering hint: lower values are intended to
	// start first. LIFE-3 doc-honesty: it is declared but NOT YET WIRED — no
	// code reads it, so it currently has no effect on start or shutdown order.
	// Start ordering is driven by Dependencies; Shutdown iterates in
	// unspecified order. Kept as a public API field for forward compatibility
	// (wiring it is future work, never silently assumed).
	Priority int
	// Dependencies lists service names that must be running
	// before this service can start.
	Dependencies []string
	// ComposeFile is the path to the Docker Compose file.
	ComposeFile string
	// ServiceName is the name within the compose file.
	ServiceName string
	// Profile is the compose profile for this service.
	Profile string
	// HealthTarget is the health check configuration.
	HealthTarget health.HealthTarget
}

// ReleaseFunc is returned by Acquire and must be called when the
// caller no longer needs the service. Failure to call it will
// prevent idle shutdown from triggering.
type ReleaseFunc func()
