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
	workDir       string
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

	// Create lazy booter for this service
	startFn := func() error {
		return lo.startServiceInternal(svc)
	}
	lo.booters[svc.Name] = lifecycle.NewLazyBooter(startFn)

	lo.logger.Info("registered lazy service: %s (category=%s, cost=%s)",
		svc.Name, svc.Category, svc.CostModel)

	return nil
}

// StartService starts a service and its dependencies on-demand.
func (lo *LazyOrchestrator) StartService(ctx context.Context, name string) error {
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

	// Start dependencies first
	for _, depName := range svc.Dependencies {
		if err := lo.StartService(ctx, depName); err != nil {
			return fmt.Errorf("dependency %s failed: %w", depName, err)
		}
	}

	// Start this service via lazy booter
	if err := booter.EnsureStarted(); err != nil {
		// Try alternatives if available
		for _, altName := range svc.AlternativeServices {
			lo.logger.Warn("service %s failed, trying alternative: %s", name, altName)
			if altErr := lo.StartService(ctx, altName); altErr == nil {
				return nil
			}
		}
		return fmt.Errorf("start service %s: %w", name, err)
	}

	return nil
}

// StopService stops a running service.
func (lo *LazyOrchestrator) StopService(ctx context.Context, name string) error {
	lo.mu.Lock()
	defer lo.mu.Unlock()

	svc, exists := lo.services[name]
	if !exists {
		return fmt.Errorf("service not found: %s", name)
	}

	if !lo.started[name] {
		return nil // Already stopped or never started
	}

	project := compose.ComposeProject{
		File:    svc.ComposeFile,
		Profile: svc.Profile,
	}

	ctx, cancel := context.WithTimeout(ctx, svc.StopTimeout)
	defer cancel()

	if err := lo.orchestrator.Down(ctx, project); err != nil {
		return fmt.Errorf("stop service %s: %w", name, err)
	}

	lo.started[name] = false
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
		status.Started = booter.Started()
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

// startServiceInternal performs the actual service startup.
func (lo *LazyOrchestrator) startServiceInternal(svc *ServiceDefinition) error {
	project := compose.ComposeProject{
		File:    svc.ComposeFile,
		Profile: svc.Profile,
	}

	lo.logger.Info("starting lazy service: %s (file=%s)", svc.Name, svc.ComposeFile)

	ctx, cancel := context.WithTimeout(context.Background(), svc.StartTimeout)
	defer cancel()

	// Start the service
	if err := lo.orchestrator.Up(ctx, project, compose.WithUpDetach(true), compose.WithWait(true)); err != nil {
		lo.mu.Lock()
		lo.failed[svc.Name] = err
		lo.mu.Unlock()
		return fmt.Errorf("compose up failed: %w", err)
	}

	// Wait for health check if defined
	if svc.HealthCheck != nil {
		if err := lo.waitForHealth(ctx, svc); err != nil {
			return fmt.Errorf("health check failed: %w", err)
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

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(2 * time.Minute)
	}

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
