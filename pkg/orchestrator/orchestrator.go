package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

type Service struct {
	Name         string
	ComposeFile  string
	Profile      string
	Required     bool
	HealthPort   int
	HealthPath   string
	Description  string
	Dependencies []string
}

type Orchestrator interface {
	DiscoverServices(dockerDir string) error
	StartAll(ctx context.Context) error
	StartService(ctx context.Context, name string) error
	StopAll(ctx context.Context) error
	ListServices() []Service
}

type ComposeOrchestrator interface {
	Up(ctx context.Context, project compose.ComposeProject) error
	Down(ctx context.Context, project compose.ComposeProject) error
}

type RemoteExecutor interface {
	Execute(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
	CopyDir(ctx context.Context, host remote.RemoteHost, src, dst string) error
}

type HostManager interface {
	ListHosts() []remote.RemoteHost
}

type DefaultOrchestrator struct {
	services       []Service
	localOrch      ComposeOrchestrator
	remoteExec     RemoteExecutor
	hostMgr        HostManager
	healthChecker  health.HealthChecker
	logger         logging.Logger
	projectDir     string
	remoteEnabled  bool
	mu             sync.Mutex
	excludePattern string
}

type Option func(*DefaultOrchestrator)

func WithLocalOrchestrator(orch ComposeOrchestrator) Option {
	return func(o *DefaultOrchestrator) { o.localOrch = orch }
}

func WithRemoteExecutor(exec RemoteExecutor) Option {
	return func(o *DefaultOrchestrator) { o.remoteExec = exec }
}

func WithHostManager(mgr HostManager) Option {
	return func(o *DefaultOrchestrator) { o.hostMgr = mgr }
}

func WithHealthChecker(hc health.HealthChecker) Option {
	return func(o *DefaultOrchestrator) { o.healthChecker = hc }
}

func WithLogger(l logging.Logger) Option {
	return func(o *DefaultOrchestrator) { o.logger = l }
}

func WithProjectDir(dir string) Option {
	return func(o *DefaultOrchestrator) { o.projectDir = dir }
}

func WithExcludePattern(pattern string) Option {
	return func(o *DefaultOrchestrator) { o.excludePattern = pattern }
}

func New(opts ...Option) *DefaultOrchestrator {
	o := &DefaultOrchestrator{
		services: make([]Service, 0),
		logger:   logging.NopLogger{},
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.projectDir == "" {
		o.projectDir, _ = os.Getwd()
	}
	o.remoteEnabled = o.hostMgr != nil && o.remoteExec != nil
	return o
}

func (o *DefaultOrchestrator) DiscoverServices(dockerDir string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	absDir := dockerDir
	if !filepath.IsAbs(dockerDir) {
		absDir = filepath.Join(o.projectDir, dockerDir)
	}

	if _, err := os.Stat(absDir); os.IsNotExist(err) {
		return fmt.Errorf("docker directory not found: %s", absDir)
	}

	return filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		name := strings.ToLower(info.Name())
		if !strings.Contains(name, "docker-compose") || !strings.HasSuffix(name, ".yml") {
			return nil
		}

		if o.excludePattern != "" {
			if matched, _ := filepath.Match(o.excludePattern, info.Name()); matched {
				return nil
			}
		}

		relPath, _ := filepath.Rel(o.projectDir, path)
		dirName := filepath.Base(filepath.Dir(path))

		for _, svc := range o.services {
			if svc.ComposeFile == relPath || svc.ComposeFile == path {
				return nil
			}
		}

		o.services = append(o.services, Service{
			Name:        dirName,
			ComposeFile: relPath,
			Description: fmt.Sprintf("Auto-discovered from %s", relPath),
		})
		return nil
	})
}

func (o *DefaultOrchestrator) AddService(svc Service) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.services = append(o.services, svc)
}

// startedService records a service StartAll successfully started, so it can be
// rolled back (compose-down) if a required service later fails mid-boot.
type startedService struct {
	svc        Service
	composeAbs string
}

func (o *DefaultOrchestrator) StartAll(ctx context.Context) error {
	// Snapshot the service list + config under the lock, then release it BEFORE
	// the blocking boot. Holding o.mu across container/SSH/compose operations
	// violated MANDATORY PRINCIPLE #2 (no blocking work inside a shared lock):
	// the whole multi-service boot stalled every other o.mu user — AddService,
	// ListServices, ServiceCount, StartService, StopAll — until it completed.
	o.mu.Lock()
	services := make([]Service, len(o.services))
	copy(services, o.services)
	remoteEnabled := o.remoteEnabled
	projectDir := o.projectDir
	o.mu.Unlock()

	o.logger.Info("orchestrator: starting %d services (remote=%v)", len(services), remoteEnabled)

	type startOutcome struct {
		svc        Service
		composeAbs string
		err        error
	}

	var wg sync.WaitGroup
	resultChan := make(chan startOutcome, len(services))

	for _, svc := range services {
		composePath := svc.ComposeFile
		if !filepath.IsAbs(composePath) {
			composePath = filepath.Join(projectDir, composePath)
		}

		if _, err := os.Stat(composePath); os.IsNotExist(err) {
			o.logger.Debug("orchestrator: skipping %s (file not found)", svc.Name)
			continue
		}

		wg.Add(1)
		go func(s Service, composeAbs string) {
			defer wg.Done()

			o.logger.Info("orchestrator: starting %s", s.Name)

			var err error
			if remoteEnabled {
				err = o.startRemote(ctx, s, composeAbs)
				if err != nil {
					o.logger.Warn("orchestrator: remote start failed for %s: %v", s.Name, err)
					err = o.startLocal(ctx, s, composeAbs)
				}
			} else {
				err = o.startLocal(ctx, s, composeAbs)
			}

			resultChan <- startOutcome{svc: s, composeAbs: composeAbs, err: err}
		}(svc, composePath)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var failures []error
	var started []startedService
	for r := range resultChan {
		if r.err != nil {
			o.logger.Warn("orchestrator: failed to start %s: %v", r.svc.Name, r.err)
			if r.svc.Required {
				failures = append(failures, fmt.Errorf("required service %s failed: %w", r.svc.Name, r.err))
			}
		} else {
			o.logger.Info("orchestrator: started %s", r.svc.Name)
			started = append(started, startedService{svc: r.svc, composeAbs: r.composeAbs})
		}
	}

	if len(failures) > 0 {
		// A required service failed: roll back the services that DID start so a
		// partial boot does not leave orphaned services running.
		o.rollback(ctx, started)
		return fmt.Errorf("orchestrator: %d service(s) failed", len(failures))
	}
	return nil
}

// rollback tears down services that StartAll had already started, used when a
// required service failed mid-boot. Best-effort local compose-down, matching
// StopAll's teardown semantics (a Down failure is logged, not surfaced — the
// StartAll error already reports the boot failure). Services started on remote
// hosts share StopAll's pre-existing local-only teardown limitation; that gap
// is tracked separately and is not regressed here.
func (o *DefaultOrchestrator) rollback(ctx context.Context, started []startedService) {
	if o.localOrch == nil {
		return
	}
	// Tear down on a context detached from the parent's cancellation: when the
	// boot failed BECAUSE ctx was canceled/timed out, reusing ctx would make
	// every rollback Down no-op on an already-dead context, defeating the
	// rollback exactly when it is most needed. WithoutCancel keeps ctx values
	// but drops its cancellation.
	downCtx := context.WithoutCancel(ctx)
	for _, s := range started {
		if err := o.localOrch.Down(downCtx, compose.ComposeProject{
			File:    s.composeAbs,
			Profile: s.svc.Profile,
		}); err != nil {
			o.logger.Warn("orchestrator: rollback down failed for %s: %v", s.svc.Name, err)
		}
	}
}

func (o *DefaultOrchestrator) startLocal(ctx context.Context, svc Service, composePath string) error {
	if o.localOrch == nil {
		return fmt.Errorf("local orchestrator not configured")
	}
	return o.localOrch.Up(ctx, compose.ComposeProject{
		File:    composePath,
		Profile: svc.Profile,
	})
}

func (o *DefaultOrchestrator) startRemote(ctx context.Context, svc Service, composePath string) error {
	if o.hostMgr == nil || o.remoteExec == nil {
		return fmt.Errorf("remote execution not configured")
	}

	hosts := o.hostMgr.ListHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no remote hosts available")
	}

	host := hosts[0]
	remoteDir := fmt.Sprintf("/home/%s/helixagent", host.User)

	mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteDir)
	result, err := o.remoteExec.Execute(ctx, host, mkdirCmd)
	if err != nil {
		return fmt.Errorf("create remote dir: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("create remote dir failed: %s", result.Stderr)
	}

	localDir := filepath.Dir(composePath)
	remoteDest := remoteDir + "/" + filepath.Base(localDir)
	if err := o.remoteExec.CopyDir(ctx, host, localDir, remoteDest); err != nil {
		return fmt.Errorf("copy to remote: %w", err)
	}

	composeCmd := fmt.Sprintf("cd %s && docker compose -f %s up -d", remoteDest, filepath.Base(composePath))
	if svc.Profile != "" {
		composeCmd = fmt.Sprintf("cd %s && docker compose -f %s --profile %s up -d", remoteDest, filepath.Base(composePath), svc.Profile)
	}

	execResult, execErr := o.remoteExec.Execute(ctx, host, composeCmd)
	if execErr != nil {
		return execErr
	}
	if execResult.ExitCode != 0 {
		return fmt.Errorf("compose up failed: %s", execResult.Stderr)
	}
	return nil
}

func (o *DefaultOrchestrator) StartService(ctx context.Context, name string) error {
	// Resolve the service + config under the lock, then release it BEFORE the
	// blocking start (MANDATORY PRINCIPLE #2 — no blocking work under o.mu).
	o.mu.Lock()
	var (
		target Service
		found  bool
	)
	for _, svc := range o.services {
		if svc.Name == name {
			target = svc
			found = true
			break
		}
	}
	remoteEnabled := o.remoteEnabled
	projectDir := o.projectDir
	o.mu.Unlock()

	if !found {
		return fmt.Errorf("service not found: %s", name)
	}

	composePath := target.ComposeFile
	if !filepath.IsAbs(composePath) {
		composePath = filepath.Join(projectDir, composePath)
	}
	if remoteEnabled {
		return o.startRemote(ctx, target, composePath)
	}
	return o.startLocal(ctx, target, composePath)
}

func (o *DefaultOrchestrator) StopAll(ctx context.Context) error {
	// Snapshot the service list + config under the lock, then release it BEFORE
	// the blocking teardown (MANDATORY PRINCIPLE #2 — no blocking work under
	// o.mu; the Down loop must not stall concurrent o.mu users).
	o.mu.Lock()
	services := make([]Service, len(o.services))
	copy(services, o.services)
	localOrch := o.localOrch
	projectDir := o.projectDir
	o.mu.Unlock()

	if localOrch == nil {
		return nil
	}

	var firstErr error
	for _, svc := range services {
		composePath := svc.ComposeFile
		if !filepath.IsAbs(composePath) {
			composePath = filepath.Join(projectDir, composePath)
		}
		if err := localOrch.Down(ctx, compose.ComposeProject{
			File:    composePath,
			Profile: svc.Profile,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (o *DefaultOrchestrator) ListServices() []Service {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]Service, len(o.services))
	copy(result, o.services)
	return result
}

func (o *DefaultOrchestrator) IsRemoteEnabled() bool {
	return o.remoteEnabled
}

func (o *DefaultOrchestrator) ServiceCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.services)
}
