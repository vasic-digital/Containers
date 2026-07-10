package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
)

// LifecycleManager controls the full lifecycle of registered
// services including lazy boot, idle shutdown, and concurrency
// limiting.
type LifecycleManager interface {
	// Register adds a service specification to the manager.
	Register(spec ServiceSpec) error

	// Start starts the named service and waits for it to become
	// healthy.
	Start(ctx context.Context, name string) error

	// Stop stops the named service.
	Stop(ctx context.Context, name string) error

	// Acquire obtains a lease on the service, starting it if
	// needed (lazy boot). The returned ReleaseFunc must be called
	// when the caller is finished.
	Acquire(ctx context.Context, name string) (ReleaseFunc, error)

	// Status returns the current lifecycle status of a service.
	Status(name string) (ServiceLifecycleStatus, error)

	// Shutdown gracefully stops all managed services.
	Shutdown(ctx context.Context) error
}

// serviceEntry holds runtime state for a single managed service.
type serviceEntry struct {
	spec       ServiceSpec
	state      string
	healthy    bool
	lastStart  time.Time
	lastStop   time.Time
	lastAcq    time.Time
	semaphore  *ConcurrencySemaphore
	idleCtrl   *IdleShutdown
	lazyBooter *LazyBooter
}

// DefaultManager is the standard LifecycleManager implementation.
// It uses a ComposeOrchestrator to start/stop containers and a
// HealthChecker to verify readiness.
type DefaultManager struct {
	mu           sync.Mutex
	services     map[string]*serviceEntry
	orchestrator compose.ComposeOrchestrator
	checker      health.HealthChecker
}

// NewDefaultManager creates a DefaultManager backed by the given
// orchestrator and health checker.
func NewDefaultManager(
	orch compose.ComposeOrchestrator,
	hc health.HealthChecker,
) *DefaultManager {
	return &DefaultManager{
		services:     make(map[string]*serviceEntry),
		orchestrator: orch,
		checker:      hc,
	}
}

// Register adds a service specification. Returns an error if a
// service with the same name is already registered.
func (m *DefaultManager) Register(spec ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if spec.Name == "" {
		return fmt.Errorf("lifecycle: service name is required")
	}
	if _, exists := m.services[spec.Name]; exists {
		return fmt.Errorf(
			"lifecycle: service %q already registered", spec.Name,
		)
	}

	entry := &serviceEntry{
		spec:  spec,
		state: "stopped",
	}

	if spec.MaxConcurrent > 0 {
		entry.semaphore = NewConcurrencySemaphore(
			spec.MaxConcurrent,
		)
	}

	m.services[spec.Name] = entry
	return nil
}

// Start starts the named service via compose and waits for health.
func (m *DefaultManager) Start(
	ctx context.Context,
	name string,
) error {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	// TOCTOU guard (constitution/Constitution.md §11.4.108 sibling-
	// primitive audit of the idle.go stale-fire race): the "not yet
	// running" check and the "starting" transition must happen inside
	// the SAME lock hold. Previously the lock was released between the
	// two, so two concurrent Start() calls that both observed the
	// service as not-running could both proceed past this point and
	// both execute the full boot sequence (double compose-up, double
	// health check, plus a second IdleShutdown controller that silently
	// orphans the first — its timer never Stop()'d, ticking in the
	// background and able to fire an unexpected Stop() later). Any
	// caller that arrives while a boot is already under way ("starting")
	// or already done ("running") returns immediately instead of
	// re-running the sequence.
	switch entry.state {
	case "running", "starting":
		m.mu.Unlock()
		return nil
	}
	entry.state = "starting"

	// Start dependencies first.
	deps := entry.spec.Dependencies
	m.mu.Unlock()

	for _, dep := range deps {
		if err := m.Start(ctx, dep); err != nil {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: dependency %q for %q: %w",
				dep, name, err,
			)
		}
	}

	// Start via compose.
	if m.orchestrator != nil && entry.spec.ComposeFile != "" {
		project := compose.ComposeProject{
			File:    entry.spec.ComposeFile,
			Profile: entry.spec.Profile,
		}
		if err := m.orchestrator.Up(ctx, project); err != nil {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: start %q: %w", name, err,
			)
		}
	}

	// Health check.
	if m.checker != nil {
		result := m.checker.Check(ctx, entry.spec.HealthTarget)
		m.mu.Lock()
		entry.healthy = result.Healthy
		m.mu.Unlock()
		// An unhealthy result must fail the start regardless of whether it
		// carries an error message. Gating on `result.Error != ""` too let an
		// unhealthy result with an empty message slip through and mark the
		// service "running" with a nil return — a fabricated success
		// (launch != working, §11.4.108-class bluff).
		if !result.Healthy {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			msg := result.Error
			if msg == "" {
				msg = "unhealthy (no error detail)"
			}
			return fmt.Errorf(
				"lifecycle: health check %q: %s",
				name, msg,
			)
		}
	}

	m.mu.Lock()
	entry.state = "running"
	entry.lastStart = time.Now()
	m.mu.Unlock()

	// Start idle shutdown monitor if configured. Capture the freshly
	// created controller into a local variable and call Start() on
	// THAT local, rather than re-reading entry.idleCtrl after
	// unlocking — the same unguarded-field-read hazard the fix above
	// closes for Stop()/Acquire() applies here too (constitution/
	// Constitution.md §11.4.108 sibling-primitive audit).
	if entry.spec.IdleTimeout > 0 {
		m.mu.Lock()
		idleCtrl := NewIdleShutdown(
			entry.spec.IdleTimeout,
			func() {
				_ = m.Stop(context.Background(), name)
			},
		)
		entry.idleCtrl = idleCtrl
		m.mu.Unlock()
		idleCtrl.Start()
	}

	return nil
}

// Stop stops the named service via compose.
func (m *DefaultManager) Stop(
	ctx context.Context,
	name string,
) error {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	if entry.state == "stopped" {
		m.mu.Unlock()
		return nil
	}

	entry.state = "stopping"
	// entry.idleCtrl is written under m.mu by Start() (see above); read
	// it under the same lock here rather than after unlocking, closing
	// a data race between this read and a concurrent Start()'s write
	// (constitution/Constitution.md §11.4.108 sibling-primitive audit
	// of the idle.go stale-fire race — go test -race caught the
	// unguarded access).
	idleCtrl := entry.idleCtrl
	m.mu.Unlock()

	// Stop idle controller.
	if idleCtrl != nil {
		idleCtrl.Stop()
	}

	// Stop via compose.
	if m.orchestrator != nil && entry.spec.ComposeFile != "" {
		project := compose.ComposeProject{
			File:    entry.spec.ComposeFile,
			Profile: entry.spec.Profile,
		}
		if err := m.orchestrator.Down(ctx, project); err != nil {
			m.mu.Lock()
			entry.state = "running"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: stop %q: %w", name, err,
			)
		}
	}

	m.mu.Lock()
	entry.state = "stopped"
	entry.healthy = false
	entry.lastStop = time.Now()
	// Reset the one-shot LazyBooter so a lazy service that was idle-stopped (or
	// stopped manually) is REVIVED on the next Acquire. Without this, the already-
	// fired sync.Once inside the LazyBooter returns its cached nil forever, so
	// Start is never re-invoked and Acquire hands back a live-looking lease on a
	// dead container (§11.4.108). A concurrent lazy Acquire copies its LazyBooter
	// pointer under m.mu before calling EnsureStarted, so nil-ing it here only
	// affects the NEXT Acquire.
	entry.lazyBooter = nil
	m.mu.Unlock()

	return nil
}

// Acquire obtains a lease on the named service. If the service is
// configured for lazy boot it will be started on first Acquire.
func (m *DefaultManager) Acquire(
	ctx context.Context,
	name string,
) (ReleaseFunc, error) {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}
	m.mu.Unlock()

	// Lazy boot: start on first acquire if not running.
	if entry.spec.LazyBoot {
		m.mu.Lock()
		if entry.lazyBooter == nil {
			entry.lazyBooter = NewLazyBooter(func() error {
				return m.Start(ctx, name)
			})
		}
		lb := entry.lazyBooter
		m.mu.Unlock()

		if err := lb.EnsureStarted(); err != nil {
			return nil, fmt.Errorf(
				"lifecycle: lazy boot %q: %w", name, err,
			)
		}
	} else {
		m.mu.Lock()
		if entry.state != "running" {
			m.mu.Unlock()
			return nil, fmt.Errorf(
				"lifecycle: service %q is not running", name,
			)
		}
		m.mu.Unlock()
	}

	// Acquire semaphore slot.
	if entry.semaphore != nil {
		if err := entry.semaphore.Acquire(ctx); err != nil {
			return nil, fmt.Errorf(
				"lifecycle: semaphore %q: %w", name, err,
			)
		}
	}

	// Reset idle timer. entry.idleCtrl is written under m.mu by Start();
	// read it under the same lock rather than unguarded, closing a data
	// race with a concurrent Start() (constitution/Constitution.md
	// §11.4.108 sibling-primitive audit; go test -race caught the
	// unguarded access).
	m.mu.Lock()
	idleCtrl := entry.idleCtrl
	entry.lastAcq = time.Now()
	m.mu.Unlock()

	if idleCtrl != nil {
		idleCtrl.Touch()
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		if entry.semaphore != nil {
			entry.semaphore.Release()
		}
		m.mu.Lock()
		relIdleCtrl := entry.idleCtrl
		m.mu.Unlock()
		if relIdleCtrl != nil {
			relIdleCtrl.Touch()
		}
	}, nil
}

// Status returns the current lifecycle status of the named service.
func (m *DefaultManager) Status(
	name string,
) (ServiceLifecycleStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.services[name]
	if !ok {
		return ServiceLifecycleStatus{}, fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	activeUsers := 0
	if entry.semaphore != nil {
		activeUsers = entry.semaphore.ActiveCount()
	}

	return ServiceLifecycleStatus{
		Name:         name,
		State:        entry.state,
		Healthy:      entry.healthy,
		ActiveUsers:  activeUsers,
		LastStarted:  entry.lastStart,
		LastStopped:  entry.lastStop,
		LastAcquired: entry.lastAcq,
	}, nil
}

// Shutdown stops all managed services in reverse priority order.
func (m *DefaultManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	m.mu.Unlock()

	var firstErr error
	for _, name := range names {
		if err := m.Stop(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
