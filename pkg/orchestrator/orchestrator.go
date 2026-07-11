package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// startedByName records, per service name, HOW that service was most
	// recently started (local compose-up vs SSH to a specific remote host) so
	// a LATER StopAll call — which has no other way to know a service's
	// provenance — can route its teardown correctly (OR-2). Guarded by mu.
	startedByName map[string]startedService
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

// walkDir is the directory-walk function DiscoverServices uses to find
// compose files. It is a package variable (not a hardcoded filepath.Walk
// call) so tests can substitute a blocking/instrumented walker to prove
// DiscoverServices does not hold o.mu across the walk (OR-5) without relying
// on real-filesystem walk timing.
var walkDir = filepath.Walk

func (o *DefaultOrchestrator) DiscoverServices(dockerDir string) error {
	// Snapshot config + the current service set under the lock, then release
	// it BEFORE the blocking directory walk (OR-5, MANDATORY PRINCIPLE #2 —
	// no blocking work inside a shared lock). Previously o.mu was held for the
	// ENTIRE walk, stalling every other o.mu user (AddService, ListServices,
	// ServiceCount, StartAll, StopAll, StartService) until a — potentially
	// large — directory tree finished scanning.
	o.mu.Lock()
	projectDir := o.projectDir
	excludePattern := o.excludePattern
	existing := make(map[string]bool, len(o.services))
	for _, svc := range o.services {
		existing[svc.ComposeFile] = true
	}
	o.mu.Unlock()

	absDir := dockerDir
	if !filepath.IsAbs(dockerDir) {
		absDir = filepath.Join(projectDir, dockerDir)
	}

	if _, err := os.Stat(absDir); os.IsNotExist(err) {
		return fmt.Errorf("docker directory not found: %s", absDir)
	}

	var discovered []Service
	walkErr := walkDir(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// OR-7: a per-entry walk error (e.g. an unreadable subdirectory)
			// must be logged, not silently swallowed — a partial scan must
			// never look like a complete one. filepath.Walk still continues
			// with the remaining entries after this returns nil.
			o.logger.Warn("orchestrator: DiscoverServices walk error at %s: %v", path, err)
			return nil
		}
		if info.IsDir() {
			return nil
		}

		name := strings.ToLower(info.Name())
		// OR-6: accept both .yml and .yaml — both are canonical Docker
		// Compose file extensions; only matching .yml silently missed
		// legitimately-named services.
		if !strings.Contains(name, "docker-compose") ||
			(!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			return nil
		}

		if excludePattern != "" {
			if matched, _ := filepath.Match(excludePattern, info.Name()); matched {
				return nil
			}
		}

		relPath, _ := filepath.Rel(projectDir, path)
		dirName := filepath.Base(filepath.Dir(path))

		if existing[relPath] || existing[path] {
			return nil
		}

		discovered = append(discovered, Service{
			Name:        dirName,
			ComposeFile: relPath,
			Description: fmt.Sprintf("Auto-discovered from %s", relPath),
		})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	if len(discovered) == 0 {
		return nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	// Re-check duplicates against the CURRENT o.services — it may have
	// changed concurrently since the snapshot above, since the lock was
	// released across the blocking walk — before inserting.
	seen := make(map[string]bool, len(o.services))
	for _, svc := range o.services {
		seen[svc.ComposeFile] = true
	}
	for _, svc := range discovered {
		if seen[svc.ComposeFile] {
			continue
		}
		o.services = append(o.services, svc)
		seen[svc.ComposeFile] = true
	}
	return nil
}

func (o *DefaultOrchestrator) AddService(svc Service) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.services = append(o.services, svc)
}

// startedService records a service StartAll (or StartService) successfully
// started, so it can be torn down (compose-down) correctly later — either
// immediately by rollback() if a required service fails mid-boot, or later by
// StopAll(). remote/remoteHost/remoteDest record WHERE it was started (OR-2):
// a service started via startRemote (SSH to a specific host) MUST be torn
// down on that SAME host via the remote executor, never assumed to be a
// local compose-down.
type startedService struct {
	svc        Service
	composeAbs string
	remote     bool
	remoteHost remote.RemoteHost
	remoteDest string
}

// remoteStartInfo records the host and remote destination directory a
// service was booted into on a successful remote start (OR-2), so a later
// teardown (rollback or StopAll) can be routed to the SAME host/directory
// instead of defaulting to a local-only compose-down.
type remoteStartInfo struct {
	host remote.RemoteHost
	dest string
}

// resolveRemoteStartInfo re-derives the host + remote destination directory
// startRemote used for composeAbs's successful start. It performs NO I/O — it
// re-applies the SAME formula startRemote uses (remoteServiceDest) against
// whatever HostManager.ListHosts() reports right now, so it stays consistent
// with the host startRemote actually picked (hosts[0]) for a call that just
// succeeded. Returns nil if remote is not configured or no hosts are
// available (should not happen immediately after a successful startRemote,
// but is handled defensively rather than assumed).
func (o *DefaultOrchestrator) resolveRemoteStartInfo(composeAbs string) *remoteStartInfo {
	if o.hostMgr == nil {
		return nil
	}
	hosts := o.hostMgr.ListHosts()
	if len(hosts) == 0 {
		return nil
	}
	host := hosts[0]
	return &remoteStartInfo{host: host, dest: remoteServiceDest(host, composeAbs)}
}

// trackStarted records svc as currently running so a later StopAll call can
// route its teardown correctly (OR-2). Callers MUST hold o.mu.
func (o *DefaultOrchestrator) trackStarted(s startedService) {
	if o.startedByName == nil {
		o.startedByName = make(map[string]startedService)
	}
	o.startedByName[s.svc.Name] = s
}

// forgetStarted removes name's tracked start record after its teardown has
// been attempted, so a subsequent StopAll does not re-attempt tearing down a
// service that is already gone.
func (o *DefaultOrchestrator) forgetStarted(name string) {
	o.mu.Lock()
	delete(o.startedByName, name)
	o.mu.Unlock()
}

// shellQuote wraps s in single quotes for safe interpolation into a POSIX
// shell command run on a remote host via ssh (OR-1). Any embedded single
// quote is rendered as the canonical close-quote, backslash-escaped-quote,
// reopen-quote sequence. startRemote hands its whole composed command string
// to `ssh` as ONE argv element (pkg/remote's SSHExecutor.Execute), so the
// REMOTE LOGIN SHELL parses it — any unescaped shell metacharacter in a
// dynamic value (Service.Profile, a compose-dir basename taken from a disk
// directory name, the remote destination directory) is remote command
// execution. It always wraps (never leaves a value bare), so an empty value
// renders as an empty single-quoted argument. Mirrors pkg/distribution's
// proven shellQuote; kept local so this package stays decoupled (§11.4.28).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteHomeDir returns the per-host working directory startRemote copies a
// service's compose directory into.
func remoteHomeDir(host remote.RemoteHost) string {
	return fmt.Sprintf("/home/%s/helixagent", host.User)
}

// remoteServiceDest returns the full remote destination directory startRemote
// copies composePath's containing directory into, on host.
func remoteServiceDest(host remote.RemoteHost, composePath string) string {
	localDir := filepath.Dir(composePath)
	return remoteHomeDir(host) + "/" + filepath.Base(localDir)
}

// buildRemoteComposeCommand renders a `docker compose` command to run on a
// remote host inside remoteDest, for the given compose-file basename,
// optional profile, and action ("up -d" / "down"). remoteDest,
// composeFileBase, and profile are shell-quoted before interpolation (OR-1):
// remoteDest derives from a disk directory name, composeFileBase from a disk
// file name, and profile from Service.Profile configuration — this package
// does not control the character set of any of them, and the whole string is
// handed to the remote host's LOGIN SHELL as one argv element, so an
// unescaped shell metacharacter in any of them is remote command execution.
// action is an internal literal ("up -d"/"down" only, never user data) and is
// not quoted.
func buildRemoteComposeCommand(remoteDest, composeFileBase, profile, action string) string {
	cmd := fmt.Sprintf("cd %s && docker compose -f %s", shellQuote(remoteDest), shellQuote(composeFileBase))
	if profile != "" {
		cmd += fmt.Sprintf(" --profile %s", shellQuote(profile))
	}
	return cmd + " " + action
}

// remoteComposeDown runs `docker compose ... down` for composeAbs's service
// on host inside remoteDest, via the configured remote executor (OR-2's
// remote-teardown path, the SSH-side counterpart of localOrch.Down).
func (o *DefaultOrchestrator) remoteComposeDown(ctx context.Context, host remote.RemoteHost, remoteDest, composeAbs, profile string) error {
	cmd := buildRemoteComposeCommand(remoteDest, filepath.Base(composeAbs), profile, "down")
	result, err := o.remoteExec.Execute(ctx, host, cmd)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remote compose down failed: %s", result.Stderr)
	}
	return nil
}

// checkServiceHealth invokes o.healthChecker (if configured) against svc
// after its compose entity has come up (OR-4: WithHealthChecker's documented
// "Required services failing = boot failure" contract was never honoured —
// a healthChecker could be wired and StartAll would still report success the
// instant `docker compose up -d` exited 0, even for a container that
// immediately crash-loops). A service opts into health checking by declaring
// HealthPort > 0; HealthPort == 0 means "no health check for this service"
// and is always a pass (nil), preserving existing behaviour for every
// service that does not declare one. remoteInfo, when non-nil, targets the
// health check at the remote host the service was started on instead of
// localhost.
func (o *DefaultOrchestrator) checkServiceHealth(ctx context.Context, svc Service, remoteInfo *remoteStartInfo) error {
	if o.healthChecker == nil || svc.HealthPort <= 0 {
		return nil
	}

	hostAddr := "localhost"
	if remoteInfo != nil && remoteInfo.host.Address != "" {
		hostAddr = remoteInfo.host.Address
	}

	target := health.HealthTarget{
		Name:     svc.Name,
		Host:     hostAddr,
		Port:     strconv.Itoa(svc.HealthPort),
		Path:     svc.HealthPath,
		Type:     health.HealthTCP,
		Required: svc.Required,
	}
	if svc.HealthPath != "" {
		target.Type = health.HealthHTTP
	}

	result := o.healthChecker.Check(ctx, target)
	if result == nil {
		return fmt.Errorf("health check for %s returned no result", svc.Name)
	}
	if !result.Healthy {
		if result.Error != "" {
			return fmt.Errorf("health check failed: %s", result.Error)
		}
		return fmt.Errorf("health check failed")
	}
	return nil
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
		err        error // non-nil => counts toward Required-failure gating
		startedOK  bool  // true => the compose entity is up (track for possible rollback/StopAll)
		remoteInfo *remoteStartInfo
	}

	// nameTotal counts how many service entries share each name. A name may
	// map to several service instances (computeStartLevels documents this —
	// e.g. replicas of the same logical service), and a dependent's
	// dependency on that name must only be treated as genuinely failed once
	// EVERY instance sharing it has failed or been skipped for a failed
	// dependency of its own (ORCH-1). Without this, a single failing
	// instance among otherwise-healthy siblings marks the whole name failed
	// and starves a dependent whose real prerequisite — a healthy sibling —
	// started fine, then rollback() tears down that healthy sibling too.
	nameTotal := make(map[string]int, len(services))
	for _, s := range services {
		nameTotal[s.Name]++
	}
	nameFailedCount := make(map[string]int, len(services))

	failedNames := make(map[string]bool)
	var failures []error
	var started []startedService

	// recordNameFailure accounts for one more failed-or-skipped instance of
	// name and only flips failedNames[name] once every instance sharing that
	// name has been accounted for (see nameTotal above). It is called only
	// from this single level-processing loop (the dependency-skip branch
	// below, and the resultChan drain after each level), never concurrently,
	// so the plain map read-modify-write here needs no locking.
	recordNameFailure := func(name string) {
		nameFailedCount[name]++
		if nameFailedCount[name] >= nameTotal[name] {
			failedNames[name] = true
		}
	}

	for _, level := range levels {
		var wg sync.WaitGroup
		resultChan := make(chan startOutcome, len(level))

		for _, svc := range level {
			// A service whose dependency already failed/was skipped cannot come
			// up correctly: skip its start, and treat the skip as this service's
			// own failure both for the Required gate and for ITS dependents.
			if dependencyFailed(svc, failedNames) {
				o.logger.Warn("orchestrator: skipping %s (dependency failed)", svc.Name)
				recordNameFailure(svc.Name)
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
				// It is also removed from nameTotal (ORCH-1): a missing-file
				// instance never resolves to a success or a failure, so it must
				// not count toward "every instance of this name has failed"
				// (it would either mask a genuinely-failed sibling by never
				// letting nameFailedCount catch up, or — if it were the only
				// instance — wrongly flip failedNames true with zero real
				// failures).
				o.logger.Debug("orchestrator: skipping %s (file not found)", svc.Name)
				nameTotal[svc.Name]--
				continue
			}

			wg.Add(1)
			go func(s Service, composeAbs string) {
				defer wg.Done()

				o.logger.Info("orchestrator: starting %s", s.Name)

				var startErr error
				var remoteInfo *remoteStartInfo
				if remoteEnabled {
					startErr = o.startRemote(ctx, s, composeAbs)
					if startErr != nil {
						o.logger.Warn("orchestrator: remote start failed for %s: %v", s.Name, startErr)
						startErr = o.startLocal(ctx, s, composeAbs)
					} else {
						remoteInfo = o.resolveRemoteStartInfo(composeAbs)
					}
				} else {
					startErr = o.startLocal(ctx, s, composeAbs)
				}

				startedOK := startErr == nil
				if startedOK {
					// OR-4: the compose entity is up — if a healthChecker is
					// configured and this service opted in (HealthPort > 0),
					// confirm it is actually healthy before declaring success.
					if healthErr := o.checkServiceHealth(ctx, s, remoteInfo); healthErr != nil {
						o.logger.Warn("orchestrator: health check failed for %s: %v", s.Name, healthErr)
						if s.Required {
							startErr = healthErr
						}
						// startedOK stays true: the compose entity IS running
						// (just unhealthy) — it must still be recorded so
						// rollback/StopAll can tear it down. A Required
						// service's health failure feeds the EXISTING
						// rollback path rather than bypassing it.
					}
				}

				resultChan <- startOutcome{
					svc: s, composeAbs: composeAbs, err: startErr,
					startedOK: startedOK, remoteInfo: remoteInfo,
				}
			}(svc, composePath)
		}

		go func() {
			wg.Wait()
			close(resultChan)
		}()

		for r := range resultChan {
			if r.err != nil {
				o.logger.Warn("orchestrator: failed to start %s: %v", r.svc.Name, r.err)
				recordNameFailure(r.svc.Name)
				if r.svc.Required {
					failures = append(failures, fmt.Errorf("required service %s failed: %w", r.svc.Name, r.err))
				}
			} else {
				o.logger.Info("orchestrator: started %s", r.svc.Name)
			}
			if r.startedOK {
				ss := startedService{svc: r.svc, composeAbs: r.composeAbs}
				if r.remoteInfo != nil {
					ss.remote = true
					ss.remoteHost = r.remoteInfo.host
					ss.remoteDest = r.remoteInfo.dest
				}
				started = append(started, ss)
			}
		}
	}

	if len(failures) > 0 {
		// A required service failed: roll back the services that DID start so a
		// partial boot does not leave orphaned services running.
		o.rollback(ctx, started)
		return fmt.Errorf("orchestrator: %d service(s) failed", len(failures))
	}

	// Every service that came up is now considered "running" for a LATER
	// StopAll call's purposes (OR-2) — record how each was started (local vs
	// a specific remote host) so StopAll can route its teardown correctly.
	o.mu.Lock()
	for _, s := range started {
		o.trackStarted(s)
	}
	o.mu.Unlock()

	return nil
}

// rollbackTimeout bounds the best-effort teardown of already-started services
// when a required service fails mid-boot (ORCH-2). It is a single shared
// budget for the WHOLE rollback loop (every started service's Down call),
// not a per-service timeout: WithoutCancel correctly detaches the parent's
// cancellation (see rollback below) but, on its own, leaves the teardown
// context with NO deadline at all — a single wedged Down call would then hang
// rollback, and therefore StartAll (which calls rollback synchronously on a
// required-service failure), forever.
const rollbackTimeout = 30 * time.Second

// rollback tears down services that StartAll had already started, used when a
// required service failed mid-boot. Best-effort teardown, matching StopAll's
// semantics (a Down failure is logged, not surfaced — the StartAll error
// already reports the boot failure). Routes each entry to the teardown path
// matching HOW it was started (OR-2): a remote-started service is torn down
// via the remote executor on the SAME host it was started on; a
// local-started service via localOrch.Down. Previously this bailed out
// entirely (silent, zero log) whenever o.localOrch was nil — a supported
// remote-only deployment shape — orphaning every remote-started service
// forever. Now each entry is handled on its own merits, and a genuinely
// unwireable teardown (no remote executor for a remote-started entry, or no
// local orchestrator for a local-started entry) is surfaced with a loud
// error log rather than silently skipped.
func (o *DefaultOrchestrator) rollback(ctx context.Context, started []startedService) {
	if len(started) == 0 {
		return
	}
	// Tear down on a context detached from the parent's cancellation: when the
	// boot failed BECAUSE ctx was canceled/timed out, reusing ctx would make
	// every rollback Down no-op on an already-dead context, defeating the
	// rollback exactly when it is most needed. WithoutCancel keeps ctx values
	// but drops its cancellation — and is then bounded by rollbackTimeout
	// (ORCH-2) so a wedged Down cannot hang rollback (and StartAll) forever.
	downCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	for _, s := range started {
		if s.remote {
			if o.remoteExec == nil {
				o.logger.Error("orchestrator: cannot roll back remote-started service %s on host %s: no remote executor configured — service left running", s.svc.Name, s.remoteHost.Name)
				continue
			}
			if err := o.remoteComposeDown(downCtx, s.remoteHost, s.remoteDest, s.composeAbs, s.svc.Profile); err != nil {
				o.logger.Warn("orchestrator: rollback remote down failed for %s: %v", s.svc.Name, err)
				continue
			}
			o.forgetStarted(s.svc.Name)
			continue
		}

		if o.localOrch == nil {
			o.logger.Error("orchestrator: cannot roll back local-started service %s: no local orchestrator configured — service left running", s.svc.Name)
			continue
		}
		if err := o.localOrch.Down(downCtx, compose.ComposeProject{
			File:    s.composeAbs,
			Profile: s.svc.Profile,
		}); err != nil {
			o.logger.Warn("orchestrator: rollback down failed for %s: %v", s.svc.Name, err)
			continue
		}
		o.forgetStarted(s.svc.Name)
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

// remoteCopyDirTimeout bounds the directory copy to a remote host performed by
// startRemote (ORCH-3). Unlike SSHExecutor.Execute/IsReachable, pkg/remote's
// CopyDir has no internal timeout of its own — called on a raw, unbounded ctx
// a stalled scp would hang that service's goroutine forever, and since
// StartAll drains resultChan only after wg.Wait() completes, a single stalled
// CopyDir hangs StartAll for EVERY service in the level, not just this one.
const remoteCopyDirTimeout = 30 * time.Second

func (o *DefaultOrchestrator) startRemote(ctx context.Context, svc Service, composePath string) error {
	if o.hostMgr == nil || o.remoteExec == nil {
		return fmt.Errorf("remote execution not configured")
	}

	hosts := o.hostMgr.ListHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no remote hosts available")
	}

	host := hosts[0]
	remoteDir := remoteHomeDir(host)

	// OR-1 (HIGH SECURITY / RCE): remoteDir is shell-quoted before
	// interpolation — it embeds host.User, and the whole command string is
	// handed to `ssh` as one argv element that the remote LOGIN SHELL parses
	// (see buildRemoteComposeCommand's doc comment for the full mandate).
	mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(remoteDir))
	result, err := o.remoteExec.Execute(ctx, host, mkdirCmd)
	if err != nil {
		return fmt.Errorf("create remote dir: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("create remote dir failed: %s", result.Stderr)
	}

	localDir := filepath.Dir(composePath)
	remoteDest := remoteServiceDest(host, composePath)
	copyCtx, cancel := context.WithTimeout(ctx, remoteCopyDirTimeout)
	defer cancel()
	if err := o.remoteExec.CopyDir(copyCtx, host, localDir, remoteDest); err != nil {
		return fmt.Errorf("copy to remote: %w", err)
	}

	// OR-1: remoteDest derives from a disk directory name and
	// filepath.Base(composePath) from a disk file name — neither is
	// controlled by this package — and svc.Profile is Service configuration;
	// all three are shell-quoted by buildRemoteComposeCommand before
	// interpolation. Without this, a shell metacharacter in any of them
	// (e.g. Profile: "core; touch /tmp/pwned #") is remote command execution.
	composeCmd := buildRemoteComposeCommand(remoteDest, filepath.Base(composePath), svc.Profile, "up -d")

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
		if err := o.startRemote(ctx, target, composePath); err != nil {
			return err
		}
		info := o.resolveRemoteStartInfo(composePath)
		if err := o.checkServiceHealth(ctx, target, info); err != nil {
			return err
		}
		o.mu.Lock()
		ss := startedService{svc: target, composeAbs: composePath}
		if info != nil {
			ss.remote = true
			ss.remoteHost = info.host
			ss.remoteDest = info.dest
		}
		o.trackStarted(ss)
		o.mu.Unlock()
		return nil
	}

	if err := o.startLocal(ctx, target, composePath); err != nil {
		return err
	}
	if err := o.checkServiceHealth(ctx, target, nil); err != nil {
		return err
	}
	o.mu.Lock()
	o.trackStarted(startedService{svc: target, composeAbs: composePath})
	o.mu.Unlock()
	return nil
}

func (o *DefaultOrchestrator) StopAll(ctx context.Context) error {
	// Snapshot the service list + config under the lock, then release it BEFORE
	// the blocking teardown (MANDATORY PRINCIPLE #2 — no blocking work under
	// o.mu; the Down loop must not stall concurrent o.mu users).
	o.mu.Lock()
	services := make([]Service, len(o.services))
	copy(services, o.services)
	localOrch := o.localOrch
	remoteExec := o.remoteExec
	projectDir := o.projectDir
	started := make(map[string]startedService, len(o.startedByName))
	for k, v := range o.startedByName {
		started[k] = v
	}
	o.mu.Unlock()

	if localOrch == nil && remoteExec == nil {
		// Nothing is wired for EITHER local or remote teardown (and, absent a
		// remote executor, nothing could ever have been remote-started
		// either) — the long-standing "not configured" no-op.
		return nil
	}

	// Bound the WHOLE teardown pass on a context detached from the caller's
	// cancellation, mirroring rollback's rollbackTimeout budget (OR-3): a
	// single wedged Down (docker daemon hang, dead SSH session) must not
	// block shutdown forever, AND it must not prevent every OTHER service
	// from at least being attempted. Previously this loop ran sequentially on
	// the caller's raw, unbounded ctx — a single blocked Down call hung
	// StopAll forever and the remaining services were never even attempted.
	downCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	var firstErr error
	for _, svc := range services {
		rec, tracked := started[svc.Name]
		if tracked && rec.remote {
			// OR-2: this service was started via SSH on a specific remote
			// host — tear it down there, never via localOrch.Down (which
			// would silently no-op or, at best, act on the wrong target).
			if remoteExec == nil {
				o.logger.Error("orchestrator: cannot stop remote-started service %s on host %s: no remote executor configured — service left running", svc.Name, rec.remoteHost.Name)
				if firstErr == nil {
					firstErr = fmt.Errorf("service %s was started remotely but no remote executor is configured to stop it", svc.Name)
				}
				continue
			}
			if err := o.remoteComposeDown(downCtx, rec.remoteHost, rec.remoteDest, rec.composeAbs, svc.Profile); err != nil {
				o.logger.Warn("orchestrator: stop remote down failed for %s: %v", svc.Name, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			o.forgetStarted(svc.Name)
			continue
		}

		if localOrch == nil {
			// No local orchestrator AND no remote-start record for this
			// specific service: nothing here to honestly tear down (it was
			// never started via THIS orchestrator instance, e.g. only ever
			// registered via AddService/DiscoverServices) — preserves the
			// long-standing per-service "not configured for local teardown"
			// no-op.
			continue
		}

		composePath := svc.ComposeFile
		if !filepath.IsAbs(composePath) {
			composePath = filepath.Join(projectDir, composePath)
		}
		if err := localOrch.Down(downCtx, compose.ComposeProject{
			File:    composePath,
			Profile: svc.Profile,
		}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if tracked {
			o.forgetStarted(svc.Name)
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
