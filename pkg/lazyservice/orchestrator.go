// Package lazyservice provides lazy container orchestration for the consuming project.
// Services are started on-demand when first requested, with support for
// dependency management, health checking, and multiple container runtimes.
package lazyservice

import (
	"context"
	"fmt"
	"sync"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/lifecycle"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/runtime"
)

// ServiceDefinition defines a lazily-loaded container service.
type ServiceDefinition struct {
	// Name is the unique service identifier
	Name string
	// ComposeFile is the path to the docker-compose.yml file
	ComposeFile string
	// Profile is the compose profile to use (optional)
	Profile string
	// Required indicates if this service is mandatory
	Required bool
	// Dependencies are service names that must start before this one
	Dependencies []string
	// HealthCheck defines how to verify service health
	HealthCheck *health.HealthTarget
	// StartTimeout is the maximum time to wait for service startup
	StartTimeout time.Duration
	// StopTimeout is the maximum time to wait for service shutdown
	StopTimeout time.Duration
	// Description of the service
	Description string
	// Category groups related services (e.g., "rag", "database", "mcp")
	Category string
	// CostModel indicates the pricing: "free", "freemium", "paid"
	CostModel string
	// AlternativeServices lists fallback service names if this one fails
	AlternativeServices []string
}

// LazyOrchestrator manages lazy container service startup.
type LazyOrchestrator struct {
	services      map[string]*ServiceDefinition
	booters       map[string]*lifecycle.LazyBooter
	orchestrator  compose.ComposeOrchestrator
	healthChecker health.HealthChecker
	logger        logging.Logger
	mu            sync.RWMutex
	started       map[string]bool
	failed        map[string]error
	// bootCtx carries the caller's context for the in-flight boot of each
	// service. The lifecycle.LazyBooter startFn closure is a ctx-less
	// `func() error`, so startServiceWithPath stows the caller ctx here
	// (keyed by service name) right before invoking booter.EnsureStarted();
	// the closure then reads it back so startServiceInternal derives its
	// timeout FROM the caller's context — a caller cancel/deadline aborts the
	// compose Up + health wait instead of being ignored (LZSVC-2). Guarded by
	// mu.
	bootCtx map[string]context.Context
	workDir string
	// Registry of available container runtimes (keyed by runtime.Name()).
	runtimes map[string]runtime.ContainerRuntime
	// Preferred runtime order (by name: "podman", "docker", etc.)
	runtimePreference []string
}

// Option configures the LazyOrchestrator.
type Option func(*LazyOrchestrator)

// WithOrchestrator sets the compose orchestrator.
func WithOrchestrator(o compose.ComposeOrchestrator) Option {
	return func(lo *LazyOrchestrator) { lo.orchestrator = o }
}

// WithHealthChecker sets the health checker.
func WithHealthChecker(hc health.HealthChecker) Option {
	return func(lo *LazyOrchestrator) { lo.healthChecker = hc }
}

// WithLogger sets the logger.
func WithLogger(l logging.Logger) Option {
	return func(lo *LazyOrchestrator) { lo.logger = l }
}

// WithWorkDir sets the working directory.
func WithWorkDir(dir string) Option {
	return func(lo *LazyOrchestrator) { lo.workDir = dir }
}

// WithRuntime adds a container runtime to the registry.
func WithRuntime(rt runtime.ContainerRuntime) Option {
	return func(lo *LazyOrchestrator) {
		lo.runtimes[rt.Name()] = rt
	}
}

// NewLazyOrchestrator creates a new lazy service orchestrator.
func NewLazyOrchestrator(opts ...Option) (*LazyOrchestrator, error) {
	lo := &LazyOrchestrator{
		services:          make(map[string]*ServiceDefinition),
		booters:           make(map[string]*lifecycle.LazyBooter),
		started:           make(map[string]bool),
		failed:            make(map[string]error),
		bootCtx:           make(map[string]context.Context),
		runtimes:          make(map[string]runtime.ContainerRuntime),
		runtimePreference: []string{"podman", "docker", "kubernetes"},
		logger:            logging.NopLogger{},
		workDir:           ".",
	}

	for _, opt := range opts {
		opt(lo)
	}

	// Create default orchestrator if not provided
	if lo.orchestrator == nil {
		o, err := compose.NewDefaultOrchestrator(lo.workDir, lo.logger)
		if err != nil {
			return nil, fmt.Errorf("create default orchestrator: %w", err)
		}
		lo.orchestrator = o
	}

	// Create default health checker if not provided
	if lo.healthChecker == nil {
		lo.healthChecker = health.NewDefaultChecker()
	}

	return lo, nil
}

// RegisterService registers a service for lazy loading.
func (lo *LazyOrchestrator) RegisterService(svc *ServiceDefinition) error {
	lo.mu.Lock()
	defer lo.mu.Unlock()

	if svc.Name == "" {
		return fmt.Errorf("service name is required")
	}
	if svc.ComposeFile == "" {
		return fmt.Errorf("service %s: compose file is required", svc.Name)
	}

	// Set defaults
	if svc.StartTimeout == 0 {
		svc.StartTimeout = 5 * time.Minute
	}
	if svc.StopTimeout == 0 {
		svc.StopTimeout = 30 * time.Second
	}
	if svc.CostModel == "" {
		svc.CostModel = "free"
	}

	lo.services[svc.Name] = svc

	// Create lazy booter for this service. The startFn is ctx-less (the
	// LazyBooter contract), so it resolves the caller's context from bootCtx
	// (stowed by startServiceWithPath immediately before EnsureStarted) — this
	// is how the caller ctx threads into the boot (LZSVC-2).
	startFn := func() error {
		return lo.startServiceInternal(lo.bootContext(svc.Name), svc)
	}
	lo.booters[svc.Name] = lifecycle.NewLazyBooter(startFn)

	lo.logger.Info("registered lazy service: %s (category=%s, cost=%s)",
		svc.Name, svc.Category, svc.CostModel)

	return nil
}

// StartService starts a service and its dependencies on-demand.
func (lo *LazyOrchestrator) StartService(ctx context.Context, name string) error {
	// Track the services currently on the dependency-resolution path so a
	// cyclic (A→B→A) or self-referential (A→A) Dependencies graph is detected
	// and rejected instead of recursing forever. Fresh set per top-level call.
	return lo.startServiceWithPath(ctx, name, make(map[string]bool))
}

// startServiceWithPath is the cycle-safe recursive core of StartService.
// inProgress holds every service ancestor on the CURRENT resolution path
// (added on entry, removed on return). Re-encountering a service already on
// the path is a dependency cycle: it is surfaced as a descriptive error rather
// than recursed into — which is what previously drove unbounded recursion →
// goroutine stack overflow → whole-process crash (a Go stack overflow is a
// fatal, non-recoverable runtime error). Because entries are removed on return,
// a service legitimately reachable via two disjoint paths (a DAG diamond) is
// still re-visited exactly as before — the LazyBooter once-guard keeps its
// compose Up to a single invocation — so acyclic behaviour is unchanged.
func (lo *LazyOrchestrator) startServiceWithPath(ctx context.Context, name string, inProgress map[string]bool) error {
	lo.mu.RLock()
	svc, exists := lo.services[name]
	booter, hasBooter := lo.booters[name]
	lo.mu.RUnlock()

	if !exists {
		return fmt.Errorf("service not found: %s", name)
	}
	if !hasBooter {
		return fmt.Errorf("service %s has no booter", name)
	}

	// Abort dependency resolution promptly if the caller's context is already
	// cancelled/expired (LZSVC-2): a dead caller ctx must not drive a deep
	// dependency chain (up to N×StartTimeout of ignored deadline).
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start service %s aborted: %w", name, err)
	}

	// Cycle detection: this service is already being resolved higher up the
	// current path, so its Dependencies form a cycle. Surface it (§11.4
	// anti-bluff: a clear error, never a silent swallow) and unwind.
	if inProgress[name] {
		return fmt.Errorf("dependency cycle detected involving service %q", name)
	}
	inProgress[name] = true
	defer delete(inProgress, name)

	// Start dependencies first
	for _, depName := range svc.Dependencies {
		if err := lo.startServiceWithPath(ctx, depName, inProgress); err != nil {
			return fmt.Errorf("dependency %s failed: %w", depName, err)
		}
	}

	// Stow the caller ctx so the (ctx-less) booter startFn can thread it into
	// startServiceInternal for the boot that EnsureStarted may trigger below
	// (LZSVC-2). Overwriting a stale entry is harmless — it is only read when
	// EnsureStarted actually runs the once-guarded startFn.
	lo.mu.Lock()
	lo.bootCtx[name] = ctx
	lo.mu.Unlock()

	// Start this service via lazy booter
	if err := booter.EnsureStarted(); err != nil {
		// Try alternatives if available
		for _, altName := range svc.AlternativeServices {
			lo.logger.Warn("service %s failed, trying alternative: %s", name, altName)
			if altErr := lo.startServiceWithPath(ctx, altName, inProgress); altErr == nil {
				return nil
			}
		}
		return fmt.Errorf("start service %s: %w", name, err)
	}

	return nil
}

// StopService stops a running service.
func (lo *LazyOrchestrator) StopService(ctx context.Context, name string) error {
	// Snapshot everything the blocking compose Down needs UNDER the lock,
	// then RELEASE the lock BEFORE calling Down. orchestrator.Down shells out
	// to an external `docker/podman compose down` process bounded only by
	// StopTimeout (default 30s); holding lo.mu across it would stall every
	// other lo.mu user (GetServiceStatus / ListServices / StartService —
	// RLock; StopAll / RegisterService — Lock) for the whole Down duration
	// (constitution/CLAUDE.md dev-principle #2 — no blocking op inside a
	// held lock).
	lo.mu.Lock()

	svc, exists := lo.services[name]
	if !exists {
		lo.mu.Unlock()
		return fmt.Errorf("service not found: %s", name)
	}

	if !lo.started[name] {
		lo.mu.Unlock()
		return nil // Already stopped or never started
	}

	project := compose.ComposeProject{
		File:    svc.ComposeFile,
		Profile: svc.Profile,
	}
	stopTimeout := svc.StopTimeout

	lo.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()

	if err := lo.orchestrator.Down(ctx, project); err != nil {
		return fmt.Errorf("stop service %s: %w", name, err)
	}

	// Re-acquire the lock only for the fast in-memory state mutation: clear
	// the started flag and reset the one-shot lazy booter so a subsequent
	// StartService genuinely restarts the service. The lifecycle.LazyBooter
	// created in RegisterService runs its startFn EXACTLY once; after a stop
	// its cached first-run success would make EnsureStarted() short-circuit,
	// turning the next StartService into a silent no-op that invokes no
	// compose Up yet returns nil — a fabricated success for a service that is
	// actually down (constitution/Constitution.md §11.4 lifecycle-layer
	// PASS-bluff). Recreating the booter here restores a clean not-started
	// booter so the service can boot again on demand.
	lo.mu.Lock()
	lo.started[name] = false
	lo.booters[name] = lifecycle.NewLazyBooter(func() error {
		return lo.startServiceInternal(lo.bootContext(svc.Name), svc)
	})
	lo.mu.Unlock()

	lo.logger.Info("stopped service: %s", name)

	return nil
}

// StopAll stops all running services.
func (lo *LazyOrchestrator) StopAll(ctx context.Context) error {
	lo.mu.Lock()
	startedServices := make([]string, 0)
	for name, started := range lo.started {
		if started {
			startedServices = append(startedServices, name)
		}
	}
	lo.mu.Unlock()

	var errs []error
	for _, name := range startedServices {
		if err := lo.StopService(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to stop %d services", len(errs))
	}
	return nil
}

// GetServiceStatus returns the current status of a service.
func (lo *LazyOrchestrator) GetServiceStatus(name string) (*ServiceStatus, error) {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	svc, exists := lo.services[name]
	if !exists {
		return nil, fmt.Errorf("service not found: %s", name)
	}

	booter, hasBooter := lo.booters[name]
	status := &ServiceStatus{
		Name:        svc.Name,
		Category:    svc.Category,
		CostModel:   svc.CostModel,
		Description: svc.Description,
	}

	if hasBooter {
		// LZSVC-3: report Started from the orchestrator's OWN success flag,
		// NOT booter.Started(). LazyBooter.Started() flips true once the boot
		// ATTEMPT settles — on success AND on failure — so a failed or
		// health-failed boot would report Started=true alongside a non-nil
		// LastError (a contradictory status). lo.started[name] is set true
		// ONLY on a genuine end-to-end success (compose Up + health pass), so
		// a failed boot correctly reports Started=false with its LastError.
		// Read under the RLock already held for the whole method.
		status.Started = lo.started[name]
		status.IsStarting = booter.IsStarting()
		if err := booter.GetError(); err != nil {
			status.LastError = err.Error()
		}
	}

	return status, nil
}

// ListServices returns all registered services.
func (lo *LazyOrchestrator) ListServices() []*ServiceDefinition {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	result := make([]*ServiceDefinition, 0, len(lo.services))
	for _, svc := range lo.services {
		result = append(result, svc)
	}
	return result
}

// ListByCategory returns services filtered by category.
func (lo *LazyOrchestrator) ListByCategory(category string) []*ServiceDefinition {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	result := make([]*ServiceDefinition, 0)
	for _, svc := range lo.services {
		if svc.Category == category {
			result = append(result, svc)
		}
	}
	return result
}

// ListFreeServices returns only free/freemium services.
func (lo *LazyOrchestrator) ListFreeServices() []*ServiceDefinition {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	result := make([]*ServiceDefinition, 0)
	for _, svc := range lo.services {
		if svc.CostModel == "free" || svc.CostModel == "freemium" {
			result = append(result, svc)
		}
	}
	return result
}

// bootContext returns the caller context stowed by startServiceWithPath for
// the named service's in-flight boot, or context.Background() when none is
// recorded (e.g. a boot not driven through StartService). It is the read side
// of the LZSVC-2 ctx-threading seam.
func (lo *LazyOrchestrator) bootContext(name string) context.Context {
	lo.mu.RLock()
	ctx := lo.bootCtx[name]
	lo.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// startServiceInternal performs the actual service startup. ctx is the caller's
// context (threaded in via bootContext / LZSVC-2); the per-service StartTimeout
// is applied ON TOP of it, so a caller cancel/deadline aborts the boot.
func (lo *LazyOrchestrator) startServiceInternal(ctx context.Context, svc *ServiceDefinition) error {
	project := compose.ComposeProject{
		File:    svc.ComposeFile,
		Profile: svc.Profile,
	}

	lo.logger.Info("starting lazy service: %s (file=%s)", svc.Name, svc.ComposeFile)

	// Bound the boot by StartTimeout, but derive FROM the caller ctx so a
	// caller cancel/deadline aborts the Up + health wait (LZSVC-2). A nil ctx
	// would panic context.WithTimeout, so fall back to Background defensively.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, svc.StartTimeout)
	defer cancel()

	// Start the service
	if err := lo.orchestrator.Up(ctx, project, compose.WithUpDetach(true), compose.WithWait(true)); err != nil {
		lo.mu.Lock()
		lo.failed[svc.Name] = err
		lo.mu.Unlock()
		return fmt.Errorf("compose up failed: %w", err)
	}

	// Wait for health check if defined.
	if svc.HealthCheck != nil {
		if healthErr := lo.waitForHealth(ctx, svc); healthErr != nil {
			// LZSVC-1: Up SUCCEEDED, so a stack is genuinely running, but the
			// service never became healthy. We return BEFORE setting
			// lo.started[svc.Name]=true, and StopService/StopAll only tear down
			// services flagged started — so this running stack would survive as
			// an orphan (and with AlternativeServices, the primary would leak
			// while the alternative also boots). Tear the just-started stack
			// down here so no orphan survives. Use a FRESH context bounded by
			// StopTimeout: the health failure may be a ctx-deadline expiry, in
			// which case the boot ctx is already Done and would abort Down
			// immediately. Surface (never swallow) a teardown failure.
			downCtx, downCancel := context.WithTimeout(context.Background(), svc.StopTimeout)
			defer downCancel()
			if downErr := lo.orchestrator.Down(downCtx, project); downErr != nil {
				return fmt.Errorf("health check failed: %w (teardown of orphaned stack also failed: %v)", healthErr, downErr)
			}
			return fmt.Errorf("health check failed: %w", healthErr)
		}
	}

	lo.mu.Lock()
	lo.started[svc.Name] = true
	lo.mu.Unlock()

	lo.logger.Info("lazy service started successfully: %s", svc.Name)
	return nil
}

// waitForHealth waits for a service to become healthy.
func (lo *LazyOrchestrator) waitForHealth(ctx context.Context, svc *ServiceDefinition) error {
	if svc.HealthCheck == nil {
		return nil
	}

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(2 * time.Minute)
	}

	// LZSVC-4: probe ONCE immediately, before entering the ticker loop. The
	// loop below only checks health inside `case <-ticker.C` (first tick at
	// 2s), so an instantly-healthy service incurred >=2s of spurious latency,
	// and a service with StartTimeout<2s timed out (ctx.Done fires) before the
	// first probe ever ran. An immediate probe returns success right away for
	// an already-healthy service.
	if res := lo.healthChecker.Check(ctx, *svc.HealthCheck); res.Healthy {
		return nil
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("health check timeout")
			}

			result := lo.healthChecker.Check(ctx, *svc.HealthCheck)
			if result.Healthy {
				return nil
			}
		}
	}
}

// ServiceStatus represents the runtime status of a service.
type ServiceStatus struct {
	Name        string
	Category    string
	CostModel   string
	Description string
	Started     bool
	IsStarting  bool
	LastError   string
}
