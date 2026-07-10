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

// computeStartLevels orders services into dependency waves: level 0 holds the
// services with no dependency, level N holds services all of whose declared
// dependencies live in earlier levels. Services within a level are independent
// and boot concurrently. It fails fast — BEFORE any service is started — on an
// unknown dependency name or a dependency cycle (a self-dependency is a
// one-node cycle), so a cycle can never reach the goroutine boot, where a
// dependent waiting on its prerequisite would deadlock instead of erroring.
// A name may map to several services; a dependency on that name must precede
// ALL of them. With no dependencies declared, every service lands in level 0 —
// identical to an unordered concurrent boot.
func computeStartLevels(services []Service) ([][]Service, error) {
	n := len(services)
	byName := make(map[string][]int, n)
	for i, s := range services {
		byName[s.Name] = append(byName[s.Name], i)
	}

	indeg := make([]int, n)
	dependents := make([][]int, n) // prerequisite index -> services depending on it
	for i, s := range services {
		for _, dep := range s.Dependencies {
			prereqs, ok := byName[dep]
			if !ok {
				return nil, fmt.Errorf("service %q depends on unknown service %q", s.Name, dep)
			}
			for _, p := range prereqs {
				dependents[p] = append(dependents[p], i)
				indeg[i]++
			}
		}
	}

	placed := make([]bool, n)
	var levels [][]Service
	remaining := n
	for remaining > 0 {
		var level []Service
		var levelIdx []int
		for i := 0; i < n; i++ {
			if !placed[i] && indeg[i] == 0 {
				level = append(level, services[i])
				levelIdx = append(levelIdx, i)
			}
		}
		if len(level) == 0 {
			var unresolved []string
			for i := 0; i < n; i++ {
				if !placed[i] {
					unresolved = append(unresolved, services[i].Name)
				}
			}
			return nil, fmt.Errorf("dependency cycle among services: %v", unresolved)
		}
		for _, i := range levelIdx {
			placed[i] = true
			remaining--
		}
		for _, i := range levelIdx {
			for _, d := range dependents[i] {
				indeg[d]--
			}
		}
		levels = append(levels, level)
	}
	return levels, nil
}

// dependencyFailed reports whether any of svc's declared dependencies already
// failed to start (or was itself skipped for a failed dependency).
func dependencyFailed(svc Service, failed map[string]bool) bool {
	for _, dep := range svc.Dependencies {
		if failed[dep] {
			return true
		}
	}
	return false
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

	// Order into dependency waves and fail fast on an unknown dependency or a
	// cycle before starting anything (a cycle must never reach the goroutine
	// boot, where it would deadlock rather than error).
	levels, err := computeStartLevels(services)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	type startOutcome struct {
		svc        Service
		composeAbs string
		err        error
	}

	failedNames := make(map[string]bool)
	var failures []error
	var started []startedService

	for _, level := range levels {
		var wg sync.WaitGroup
		resultChan := make(chan startOutcome, len(level))

		for _, svc := range level {
			// A service whose dependency already failed/was skipped cannot come
			// up correctly: skip its start, and treat the skip as this service's
			// own failure both for the Required gate and for ITS dependents.
			if dependencyFailed(svc, failedNames) {
				o.logger.Warn("orchestrator: skipping %s (dependency failed)", svc.Name)
				failedNames[svc.Name] = true
				if svc.Required {
					failures = append(failures, fmt.Errorf("required service %s skipped: dependency failed", svc.Name))
				}
				continue
			}

			composePath := svc.ComposeFile
			if !filepath.IsAbs(composePath) {
				composePath = filepath.Join(projectDir, composePath)
			}

			if _, statErr := os.Stat(composePath); os.IsNotExist(statErr) {
				// A missing compose file is a benign "not configured here" skip
				// (unchanged from the pre-dependency boot): it is intentionally
				// NOT recorded in failedNames, so a service depending on a
				// missing-file service still starts — a missing file means "this
				// service isn't part of this deployment", not "its dependency
				// failed". A dependency whose Up genuinely FAILS does cascade.
				o.logger.Debug("orchestrator: skipping %s (file not found)", svc.Name)
				continue
			}

			wg.Add(1)
			go func(s Service, composeAbs string) {
				defer wg.Done()

				o.logger.Info("orchestrator: starting %s", s.Name)

				var startErr error
				if remoteEnabled {
					startErr = o.startRemote(ctx, s, composeAbs)
					if startErr != nil {
						o.logger.Warn("orchestrator: remote start failed for %s: %v", s.Name, startErr)
						startErr = o.startLocal(ctx, s, composeAbs)
					}
				} else {
					startErr = o.startLocal(ctx, s, composeAbs)
				}

				resultChan <- startOutcome{svc: s, composeAbs: composeAbs, err: startErr}
			}(svc, composePath)
		}

		go func() {
			wg.Wait()
			close(resultChan)
		}()

		for r := range resultChan {
			if r.err != nil {
				o.logger.Warn("orchestrator: failed to start %s: %v", r.svc.Name, r.err)
				failedNames[r.svc.Name] = true
				if r.svc.Required {
					failures = append(failures, fmt.Errorf("required service %s failed: %w", r.svc.Name, r.err))
				}
			} else {
				o.logger.Info("orchestrator: started %s", r.svc.Name)
				started = append(started, startedService{svc: r.svc, composeAbs: r.composeAbs})
			}
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
