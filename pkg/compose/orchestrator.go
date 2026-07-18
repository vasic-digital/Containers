package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// ComposeOrchestrator defines the interface for managing services
// through Docker Compose or compatible orchestration tools.
type ComposeOrchestrator interface {
	// Up creates and starts containers for the given project.
	Up(ctx context.Context, project ComposeProject, opts ...UpOption) error
	// Down stops and removes containers for the given project.
	Down(
		ctx context.Context, project ComposeProject,
		opts ...DownOption,
	) error
	// Status returns the current status of each service in the project.
	Status(
		ctx context.Context, project ComposeProject,
	) ([]ServiceStatus, error)
	// Logs returns a reader streaming log output for the named service.
	Logs(
		ctx context.Context, project ComposeProject, service string,
	) (io.ReadCloser, error)
}

// CmdFactory creates exec.Cmd instances (for testing).
type CmdFactory interface {
	CommandContext(ctx context.Context, name string, args ...string) Cmd
}

// Cmd wraps exec.Cmd methods used by the orchestrator.
type Cmd interface {
	SetDir(dir string)
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

// realCmdFactory creates real exec.Cmd instances.
type realCmdFactory struct{}

func (realCmdFactory) CommandContext(
	ctx context.Context, name string, args ...string,
) Cmd {
	return &realCmd{cmd: exec.CommandContext(ctx, name, args...)}
}

// realCmd wraps exec.Cmd.
type realCmd struct {
	cmd *exec.Cmd
}

func (r *realCmd) SetDir(dir string) { r.cmd.Dir = dir }
func (r *realCmd) Start() error      { return r.cmd.Start() }
func (r *realCmd) Wait() error       { return r.cmd.Wait() }
func (r *realCmd) StdoutPipe() (io.ReadCloser, error) {
	return r.cmd.StdoutPipe()
}

// DefaultOrchestrator implements ComposeOrchestrator by shelling out to
// the detected compose command.
type DefaultOrchestrator struct {
	composeCmd  string
	composeArgs []string
	workDir     string
	logger      logging.Logger
	cmdFactory  CmdFactory
	// isPodmanCompose is true when the active runtime is the standalone
	// podman-compose tool, which differs from docker compose in two
	// material ways handled by this package: (1) it does NOT support the
	// `up --wait` flag, and (2) its `ps` output is podman-native JSON, not
	// the docker compose Go-template format.
	isPodmanCompose bool
}

// isPodmanComposeCmd reports whether the given compose command + args is the
// standalone podman-compose tool. `podman compose` (the docker-compose
// provider invoked via the podman CLI) is docker-compose-compatible and is
// NOT classified as podman-compose here.
func isPodmanComposeCmd(composeCmd string, _ []string) bool {
	return composeCmd == "podman-compose"
}

// podmanBannerMarkers are substrings that identify a podman-backed compose
// invocation from its `version` output, matched case-insensitively against
// the probe's combined stdout+stderr. Captured 2026-07-11 from a real
// rootless-podman host (this project's own §11.4.161 runtime) where
// `docker version` / `docker compose version` succeed by silently
// re-execing into podman / podman-compose and print:
//
//	Emulate Docker CLI using podman. Create /etc/containers/nodocker to
//	quiet msg.
//	Client:       Podman Engine
//	...
//
// and
//
//	Emulate Docker CLI using podman. Create /etc/containers/nodocker to
//	quiet msg.
//	Executing external compose provider "/usr/bin/podman-compose". ...
//	podman version 5.7.1
//	podman-compose version 1.5.0
//
// "podman" alone is a sufficiently precise marker here because it only ever
// appears in this probe's OWN version/banner output, never in genuine
// docker/docker-compose version output.
var podmanBannerMarkers = []string{"podman"}

// runVersionProbe runs `<name> [args...] version`, bounded by timeout (zero
// means unbounded, mirroring composeCmdWorks), and returns the combined
// stdout+stderr plus whether the command exited zero. args is copied before
// appending "version" so the caller's slice is never aliased (COMP-1
// discipline).
func runVersionProbe(name string, args []string, timeout time.Duration) (string, bool) {
	checkArgs := make([]string, 0, len(args)+1)
	checkArgs = append(checkArgs, args...)
	checkArgs = append(checkArgs, "version")

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, name, checkArgs...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err == nil
}

// isPodmanBackedCmd reports whether the resolved compose command actually
// delegates to podman even when composeCmd is not literally "podman-compose"
// (CO-1). detectComposeCmd tries {"docker", ["compose"]} FIRST, and on a
// podman-docker compatibility-shim host `docker compose version` exits 0 by
// silently re-execing into podman-compose -- isPodmanComposeCmd's literal
// string match never fires for that case, so Status()/Up(WithWait) take the
// docker-native code paths against a podman backend and break: the docker ps
// Go-template errors out (Status() silently returns empty while containers
// are actually running) and `--wait` (docker-native) is rejected by
// podman-compose (Up(WithWait) hard-fails instead of falling back to
// waitForServices polling).
//
// This probe runs `<composeCmd> [composeArgs...] version`, bounded by
// timeout (mirrors composeCmdWorks/COMP-3 so a wedged binary cannot block
// classification), and inspects the combined stdout+stderr for a podman
// banner marker. composeCmd values already classified by isPodmanComposeCmd
// ("podman-compose"), or that intentionally route through the
// docker-compose-compatible `podman compose` provider ("podman"), are
// skipped -- this probe exists only to catch a DIFFERENTLY-NAMED command
// silently backed by podman.
func isPodmanBackedCmd(composeCmd string, composeArgs []string, timeout time.Duration) bool {
	if composeCmd == "podman" || composeCmd == "podman-compose" {
		return false
	}

	out, ok := runVersionProbe(composeCmd, composeArgs, timeout)
	if !ok {
		return false
	}

	lower := strings.ToLower(out)
	for _, marker := range podmanBannerMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// NewDefaultOrchestrator creates a DefaultOrchestrator, auto-detecting
// the available compose command. The workDir is the directory from
// which commands are executed.
func NewDefaultOrchestrator(
	workDir string, logger logging.Logger,
) (*DefaultOrchestrator, error) {
	cmd, args, err := detectComposeCmd()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultOrchestrator{
		composeCmd:  cmd,
		composeArgs: args,
		workDir:     workDir,
		logger:      logger,
		cmdFactory:  realCmdFactory{},
		// CO-1: classify by literal name first, then -- because
		// detectComposeCmd tries {"docker", ["compose"]} FIRST and a
		// podman-docker compatibility shim answers that probe successfully
		// while actually delegating to podman -- fall back to a runtime
		// probe of the resolved command itself.
		isPodmanCompose: isPodmanComposeCmd(cmd, args) ||
			isPodmanBackedCmd(cmd, args, composeProbeTimeout),
	}, nil
}

// NewOrchestrator creates an orchestrator with an explicit compose
// command and args (useful for testing).
func NewOrchestrator(
	composeCmd string,
	composeArgs []string,
	workDir string,
	logger logging.Logger,
) *DefaultOrchestrator {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultOrchestrator{
		composeCmd:      composeCmd,
		composeArgs:     composeArgs,
		workDir:         workDir,
		logger:          logger,
		cmdFactory:      realCmdFactory{},
		isPodmanCompose: isPodmanComposeCmd(composeCmd, composeArgs),
	}
}

// NewOrchestratorWithFactory creates an orchestrator with a custom
// command factory (for testing).
func NewOrchestratorWithFactory(
	composeCmd string,
	composeArgs []string,
	workDir string,
	logger logging.Logger,
	factory CmdFactory,
) *DefaultOrchestrator {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	if factory == nil {
		factory = realCmdFactory{}
	}
	return &DefaultOrchestrator{
		composeCmd:      composeCmd,
		composeArgs:     composeArgs,
		workDir:         workDir,
		logger:          logger,
		cmdFactory:      factory,
		isPodmanCompose: isPodmanComposeCmd(composeCmd, composeArgs),
	}
}

// Up creates and starts containers.
func (o *DefaultOrchestrator) Up(
	ctx context.Context,
	project ComposeProject,
	opts ...UpOption,
) error {
	cfg := applyUpOptions(opts)
	args := o.projectArgs(project)
	args = append(args, "up")

	if cfg.Detach {
		args = append(args, "-d")
	}
	if cfg.RemoveOrphans {
		args = append(args, "--remove-orphans")
	}
	if cfg.BuildFirst {
		args = append(args, "--build")
	}
	if cfg.ForceRecreate {
		args = append(args, "--force-recreate")
	}
	if cfg.NoRecreate {
		args = append(args, "--no-recreate")
	}
	if cfg.Timeout > 0 {
		args = append(args, "--timeout",
			strconv.Itoa(cfg.Timeout))
	}
	// `--wait` is a docker compose flag; podman-compose does not support it
	// (it forwards unknown flags to `podman` and fails, or silently ignores
	// them). For podman-compose we omit the flag here and fall back to
	// host-side healthcheck polling AFTER `up` returns so the wait
	// semantics still hold for the caller.
	if cfg.Wait && !o.isPodmanCompose {
		args = append(args, "--wait")
		// COMP2-1: docker compose's native `--wait` accepts a companion
		// `--wait-timeout <seconds>` flag ("Maximum duration in seconds to
		// wait for the project to be running|healthy" --
		// docs.docker.com/reference/cli/docker/compose/up, verified
		// 2026-07-11). CO-2 added WaitTimeout as the caller's wait-deadline
		// knob and wired it to the podman host-side poll (waitForServices)
		// but left the docker native path UNBOUNDED -- WithWaitTimeout was
		// silently dropped on docker, so `up --wait` could block far past the
		// caller's requested deadline. Forward the caller's explicit timeout
		// so the wait-deadline is honoured on BOTH runtimes. When unset (0)
		// docker's `--wait` keeps its own built-in default (not this
		// package's podman-only 120s), so the flag is emitted only when the
		// caller set one.
		if cfg.WaitTimeout > 0 {
			args = append(args, "--wait-timeout",
				strconv.Itoa(cfg.WaitTimeout))
		}
	}

	args = append(args, project.Services...)

	o.logger.Info("compose up: %s %s", o.composeCmd,
		strings.Join(args, " "))
	if err := o.run(ctx, args); err != nil {
		return err
	}

	// podman-compose host-side wait fallback: poll Status() until every
	// service is running (and any service with a healthcheck is healthy),
	// or the context / timeout elapses.
	if cfg.Wait && o.isPodmanCompose {
		// CO-2: cfg.WaitTimeout is a DISTINCT knob from cfg.Timeout --
		// cfg.Timeout maps to compose's own `--timeout` graceful-shutdown
		// flag (seconds given to a container to stop during a recreate),
		// which is typically small and has nothing to do with how long the
		// host-side health-poll fallback should wait for services to become
		// ready. Reusing cfg.Timeout for both silently capped the poll
		// deadline to whatever the caller set for shutdown grace.
		return o.waitForServices(ctx, project, cfg.WaitTimeout)
	}
	return nil
}

// waitForServices polls the project's service status until every service is
// running and no service reports an unhealthy/starting health state, or the
// deadline elapses. It is the host-side equivalent of docker compose's
// `up --wait` for runtimes (podman-compose) that lack the native flag.
//
// timeoutSecs of 0 means "use the default" (defaultWaitTimeout).
func (o *DefaultOrchestrator) waitForServices(
	ctx context.Context,
	project ComposeProject,
	timeoutSecs int,
) error {
	timeout := defaultWaitTimeout
	if timeoutSecs > 0 {
		timeout = time.Duration(timeoutSecs) * time.Second
	}

	deadline := time.Now().Add(timeout)
	// COMP-2: derive a deadline-bounded context so a SINGLE hung Status/ps
	// exec is bounded by `timeout`. Without this the deadline is only checked
	// BETWEEN poll iterations; if the caller's ctx has no deadline and one
	// `ps` probe hangs, waitForServices blocks indefinitely, silently
	// ignoring `timeout`. WithDeadline never extends an earlier parent
	// deadline (it takes the min), so the caller's own timeout is preserved.
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		statuses, err := o.Status(ctx, project)
		if err != nil {
			lastErr = err
		} else {
			// CO-2: Status() reports EVERY service in the compose project,
			// but the caller may have scoped Up() to a SUBSET via
			// project.Services (e.g. Up({Services:[web]})). Requiring
			// readiness of every reported service -- including ones the
			// caller never asked to start -- makes waitForServices false-FAIL
			// whenever an unrelated service/profile in the same compose file
			// is not ready. Scope the readiness check to the requested
			// subset; an empty project.Services means "no filter", matching
			// the prior whole-project semantics.
			relevant := filterStatusesByServices(statuses, project.Services)
			if servicesReady(relevant) {
				o.logger.Debug(
					"podman-compose wait: all requested services ready (%d)",
					len(relevant),
				)
				return nil
			}
			lastErr = fmt.Errorf(
				"services not ready: %s", summarizeStatuses(relevant),
			)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"timed out after %s waiting for services to be ready: %w",
				timeout, lastErr,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"context canceled waiting for services: %w", ctx.Err(),
			)
		case <-ticker.C:
		}
	}
}

// servicesReady reports whether every service in statuses is running and no
// service is in a non-ready health state. An empty slice is NOT ready — there
// is nothing to confirm came up.
func servicesReady(statuses []ServiceStatus) bool {
	if len(statuses) == 0 {
		return false
	}
	for _, s := range statuses {
		if s.State != "running" {
			return false
		}
		// A service that declares a healthcheck must be healthy. States
		// "starting" / "unhealthy" are not ready. An empty/"none"/"healthy"
		// health value is acceptable (no healthcheck or already healthy).
		switch s.Health {
		case "starting", "unhealthy":
			return false
		}
	}
	return true
}

// filterStatusesByServices restricts statuses to the services explicitly
// requested by the caller (project.Services). An empty/nil services slice
// means "no filter" -- every reported status is in scope, preserving the
// prior whole-project wait semantics for callers that never scope Up() to a
// subset (CO-2).
func filterStatusesByServices(
	statuses []ServiceStatus, services []string,
) []ServiceStatus {
	if len(services) == 0 {
		return statuses
	}
	want := make(map[string]struct{}, len(services))
	for _, s := range services {
		want[s] = struct{}{}
	}
	filtered := make([]ServiceStatus, 0, len(statuses))
	for _, s := range statuses {
		if _, ok := want[s.Name]; ok {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// summarizeStatuses renders a compact name=state/health summary for error
// messages.
func summarizeStatuses(statuses []ServiceStatus) string {
	if len(statuses) == 0 {
		return "no services reported"
	}
	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		if s.Health != "" {
			parts = append(parts,
				fmt.Sprintf("%s=%s/%s", s.Name, s.State, s.Health))
		} else {
			parts = append(parts,
				fmt.Sprintf("%s=%s", s.Name, s.State))
		}
	}
	return strings.Join(parts, ", ")
}

// Down stops and removes containers.
func (o *DefaultOrchestrator) Down(
	ctx context.Context,
	project ComposeProject,
	opts ...DownOption,
) error {
	cfg := applyDownOptions(opts)
	args := o.projectArgs(project)
	args = append(args, "down")

	if cfg.RemoveOrphans {
		args = append(args, "--remove-orphans")
	}
	if cfg.RemoveVolumes {
		args = append(args, "--volumes")
	}
	if cfg.RemoveImages != "" {
		args = append(args, "--rmi", cfg.RemoveImages)
	}
	if cfg.Timeout > 0 {
		args = append(args, "--timeout",
			strconv.Itoa(cfg.Timeout))
	}

	o.logger.Info("compose down: %s %s", o.composeCmd,
		strings.Join(args, " "))
	return o.run(ctx, args)
}

// defaultWaitTimeout and waitPollInterval bound the host-side wait fallback
// used for runtimes that lack docker compose's native `up --wait`.
const (
	defaultWaitTimeout = 120 * time.Second
	waitPollInterval   = 1 * time.Second
	// composeProbeTimeout bounds each `<cmd> version` detection probe so a
	// wedged client binary cannot block detection (COMP-3).
	composeProbeTimeout = 5 * time.Second
)

// Status returns the status of all services in the project. For docker
// compose it parses the pipe-delimited Go-template `ps` output; for
// podman-compose it parses podman-native JSON (`ps --format json`), because
// podman-compose forwards `--format` to `podman ps`, where the docker
// compose `{{.Name}}` template fields do not exist (the type is
// containers.psReporter with capitalized `Names`, etc.) — the docker
// template path returns a template-error line that parses to ZERO services
// even while containers are running, which is exactly the bug this fixes.
func (o *DefaultOrchestrator) Status(
	ctx context.Context,
	project ComposeProject,
) ([]ServiceStatus, error) {
	if o.isPodmanCompose {
		return o.statusPodman(ctx, project)
	}

	args := o.projectArgs(project)
	args = append(args, "ps", "--format",
		"{{.Name}}|{{.State}}|{{.Health}}|{{.Ports}}|{{.ExitCode}}")

	out, err := o.output(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("compose ps failed: %w", err)
	}

	return parseStatusOutput(out), nil
}

// statusPodman returns service status by parsing podman-compose's JSON ps
// output. podman-compose `ps --format json` forwards to `podman ps -a
// --format json`, returning an array of podman container records.
func (o *DefaultOrchestrator) statusPodman(
	ctx context.Context,
	project ComposeProject,
) ([]ServiceStatus, error) {
	args := o.projectArgs(project)
	args = append(args, "ps", "--format", "json")

	out, err := o.output(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("compose ps failed: %w", err)
	}

	statuses, err := parsePodmanStatusJSON(out)
	if err != nil {
		return nil, fmt.Errorf("compose ps failed: %w", err)
	}
	return statuses, nil
}

// Logs returns a reader for the log output of the named service.
func (o *DefaultOrchestrator) Logs(
	ctx context.Context,
	project ComposeProject,
	service string,
) (io.ReadCloser, error) {
	args := o.projectArgs(project)
	args = append(args, "logs", "--no-color", service)

	// COMP-1: defensive copy — o.composeArgs is a shared, caller-supplied
	// immutable field. `append(o.composeArgs, args...)` would write INTO its
	// backing array when cap>len (reachable via NewOrchestrator*), aliasing
	// concurrent callers and corrupting argv. Copy into a fresh backing array.
	allArgs := make([]string, 0, len(o.composeArgs)+len(args))
	allArgs = append(allArgs, o.composeArgs...)
	allArgs = append(allArgs, args...)
	cmd := o.cmdFactory.CommandContext(ctx, o.composeCmd, allArgs...)
	cmd.SetDir(o.workDir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf(
			"failed to create stdout pipe: %w", err,
		)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf(
			"failed to start compose logs: %w", err,
		)
	}

	return &cmdLogReader{cmd: cmd, reader: stdout}, nil
}

// logReader wraps a command's stdout pipe and waits for the process to
// exit on Close.
type logReader struct {
	cmd    *exec.Cmd
	reader io.ReadCloser
}

func (r *logReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *logReader) Close() error {
	_ = r.reader.Close()
	return r.cmd.Wait()
}

// cmdLogReader wraps a Cmd interface's stdout pipe.
type cmdLogReader struct {
	cmd    Cmd
	reader io.ReadCloser
}

func (r *cmdLogReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *cmdLogReader) Close() error {
	_ = r.reader.Close()
	return r.cmd.Wait()
}

// projectArgs builds the common compose arguments for a project.
func (o *DefaultOrchestrator) projectArgs(
	project ComposeProject,
) []string {
	var args []string
	if project.File != "" {
		args = append(args, "-f", project.File)
	}
	if project.Name != "" {
		args = append(args, "--project-name", project.Name)
	}
	if project.Profile != "" {
		args = append(args, "--profile", project.Profile)
	}
	return args
}

// run executes the compose command and returns any error.
func (o *DefaultOrchestrator) run(
	ctx context.Context, args []string,
) error {
	// COMP-1: defensive copy (see Logs) — avoid aliasing the shared
	// o.composeArgs backing array under concurrent run/output calls.
	allArgs := make([]string, 0, len(o.composeArgs)+len(args))
	allArgs = append(allArgs, o.composeArgs...)
	allArgs = append(allArgs, args...)
	cmd := exec.CommandContext(ctx, o.composeCmd, allArgs...)
	cmd.Dir = o.workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	o.logger.Debug("executing: %s %s (dir: %s)", o.composeCmd, strings.Join(allArgs, " "), o.workDir)
	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		o.logger.Error("%s %s failed: %v\nstderr: %s",
			o.composeCmd, strings.Join(allArgs, " "),
			err, stderrStr)
		return fmt.Errorf("%s %s failed: %w\nstderr: %s",
			o.composeCmd, strings.Join(allArgs, " "),
			err, stderrStr)
	}
	o.logger.Debug("compose command completed successfully")
	return nil
}

// output executes the compose command and returns stdout.
func (o *DefaultOrchestrator) output(
	ctx context.Context, args []string,
) (string, error) {
	// COMP-1: defensive copy (see Logs) — avoid aliasing the shared
	// o.composeArgs backing array under concurrent run/output calls.
	allArgs := make([]string, 0, len(o.composeArgs)+len(args))
	allArgs = append(allArgs, o.composeArgs...)
	allArgs = append(allArgs, args...)
	cmd := exec.CommandContext(ctx, o.composeCmd, allArgs...)
	cmd.Dir = o.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s failed: %w\nstderr: %s",
			o.composeCmd, strings.Join(allArgs, " "),
			err, stderr.String())
	}
	return stdout.String(), nil
}

// parseStatusOutput parses the pipe-delimited output from compose ps.
func parseStatusOutput(output string) []ServiceStatus {
	var statuses []ServiceStatus
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) < 5 {
			continue
		}

		exitCode := 0
		if ec, err := strconv.Atoi(
			strings.TrimSpace(parts[4]),
		); err == nil {
			exitCode = ec
		}

		ports := parsePorts(parts[3])

		statuses = append(statuses, ServiceStatus{
			Name:     strings.TrimSpace(parts[0]),
			State:    strings.TrimSpace(parts[1]),
			Health:   strings.TrimSpace(parts[2]),
			Ports:    ports,
			ExitCode: exitCode,
		})
	}
	return statuses
}

// podmanContainer is the subset of podman's `ps --format json` record that
// maps onto ServiceStatus. Field names + the `Names` array follow podman's
// JSON schema (containers/podman) which differs from docker compose ps.
type podmanContainer struct {
	Names    []string          `json:"Names"`
	State    string            `json:"State"`
	Health   string            `json:"Health"`
	ExitCode int               `json:"ExitCode"`
	Exited   bool              `json:"Exited"`
	Labels   map[string]string `json:"Labels"`
	Ports    []podmanPort      `json:"Ports"`
}

// podmanPort is podman's JSON port-mapping record.
type podmanPort struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// parsePodmanStatusJSON parses podman's `ps --format json` array into
// ServiceStatus records. The service NAME prefers the compose service label
// (`com.docker.compose.service` / `io.podman.compose.service`) when present —
// falling back to the container name — so callers keying off the logical
// service name match what they used at `up` time. Empty / whitespace-only
// input yields no statuses (not an error), mirroring the docker path.
func parsePodmanStatusJSON(output string) ([]ServiceStatus, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, nil
	}

	var containers []podmanContainer
	if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
		return nil, fmt.Errorf(
			"failed to parse podman ps json: %w", err,
		)
	}

	statuses := make([]ServiceStatus, 0, len(containers))
	for _, c := range containers {
		name := podmanServiceName(c)
		if name == "" {
			continue
		}

		state := c.State
		if state == "" && c.Exited {
			state = "exited"
		}

		statuses = append(statuses, ServiceStatus{
			Name:     name,
			State:    state,
			Health:   c.Health,
			Ports:    podmanPortStrings(c.Ports),
			ExitCode: c.ExitCode,
		})
	}
	return statuses, nil
}

// podmanServiceName resolves the logical compose service name for a podman
// container, preferring the compose service label and falling back to the
// first container name.
func podmanServiceName(c podmanContainer) string {
	for _, key := range []string{
		"com.docker.compose.service",
		"io.podman.compose.service",
	} {
		if v, ok := c.Labels[key]; ok && v != "" {
			return v
		}
	}
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return ""
}

// podmanPortStrings renders podman's structured port records into the
// docker-style "host_ip:host_port->container_port/proto" string form used by
// ServiceStatus.Ports.
func podmanPortStrings(ports []podmanPort) []string {
	if len(ports) == 0 {
		return nil
	}
	result := make([]string, 0, len(ports))
	for _, p := range ports {
		host := p.HostIP
		if host == "" {
			host = "0.0.0.0"
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		result = append(result, fmt.Sprintf(
			"%s:%d->%d/%s",
			host, p.HostPort, p.ContainerPort, proto,
		))
	}
	return result
}

// parsePorts splits a comma-separated list of port mappings.
func parsePorts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// composeEnvVar is the env var a consumer sets to pin the compose command,
// decoupled from any auto-detection order. Value is a whitespace-separated
// command line, e.g. "podman compose", "podman-compose", or "docker compose".
// When set and the pinned command verifies, auto-detection is bypassed
// entirely. This lets a consumer with a rootless-podman-only policy force
// podman without the library baking any runtime preference into its generic
// docker-first default.
//
// Motivation (R1): on a host with a genuine independent docker AND a genuine
// independent podman, detectComposeCmd's docker-first candidate order picks
// docker, and isPodmanBackedCmd does NOT reclassify it (a genuine docker's
// `docker compose version` banner has no "podman" marker), so a consumer
// wanting podman silently gets docker. This pin gives the consumer a
// decoupled override without changing the library's generic default.
const composeEnvVar = "CONTAINERS_COMPOSE_CMD"

// composeCmdProbe reports whether `<name> [args...] version` runs. It is a
// package var (defaulting to composeCmdWorks bounded by composeProbeTimeout)
// so tests can inject a fake, keeping the env-pin + fallback logic in
// detectComposeCmdWithProbe hermetically unit-testable without a real
// docker/podman on the test host.
var composeCmdProbe = func(name string, args []string) bool {
	return composeCmdWorks(name, args, composeProbeTimeout)
}

// detectComposeCmd resolves the compose command. Resolution order:
//  1. CONTAINERS_COMPOSE_CMD env pin (decoupled, consumer-supplied) --
//     bypasses auto-detection; a pin that does not verify is a HARD error,
//     never a silent fall-through to the docker-first auto-detect.
//  2. Auto-detect, preferring Docker Compose v2 plugin, then standalone
//     docker-compose, then podman-compose, then podman compose.
func detectComposeCmd() (string, []string, error) {
	return detectComposeCmdWithProbe(os.Getenv(composeEnvVar), composeCmdProbe)
}

// detectComposeCmdWithProbe is the testable core of detectComposeCmd: pinned
// is the raw env-pin value (empty = none) and probe reports whether a
// candidate `<name> [args...] version` runs. Split out so the pin + fallback
// logic is hermetically unit-testable; the real detectComposeCmd wires
// composeCmdProbe (which delegates to composeCmdWorks) as probe.
func detectComposeCmdWithProbe(
	pinned string, probe func(name string, args []string) bool,
) (string, []string, error) {
	// 1. Explicit consumer pin -- bypass auto-detection, fail closed.
	if fields := strings.Fields(pinned); len(fields) > 0 {
		cmd, args := fields[0], fields[1:]
		if probe(cmd, args) {
			return cmd, args, nil
		}
		return "", nil, fmt.Errorf(
			"%s pinned compose command %q is not runnable",
			composeEnvVar, pinned,
		)
	}

	// 2. Auto-detect, preferring Docker Compose v2 plugin, then standalone
	// docker-compose, then podman-compose, then podman compose. Order
	// preserved for generic (unpinned) consumers.
	candidates := []struct {
		cmd  string
		args []string
	}{
		{"docker", []string{"compose"}},
		{"docker-compose", nil},
		{"podman-compose", nil},
		{"podman", []string{"compose"}},
	}

	for _, c := range candidates {
		if probe(c.cmd, c.args) {
			return c.cmd, c.args, nil
		}
	}

	return "", nil, fmt.Errorf(
		"no compose command found: tried docker compose, " +
			"docker-compose, podman-compose, podman compose " +
			"(set " + composeEnvVar + " to pin one)",
	)
}

// composeCmdWorks reports whether `<name> [args...] version` runs successfully
// within the given timeout. COMP-3: the probe is bounded by an internal
// deadline-derived context (exec.CommandContext) so a wedged client binary
// cannot block detection (and NewDefaultOrchestrator) indefinitely. A zero
// timeout means "no bound". The args are copied before appending "version" so
// the caller's slice is never aliased (COMP-1 discipline).
func composeCmdWorks(name string, args []string, timeout time.Duration) bool {
	checkArgs := make([]string, 0, len(args)+1)
	checkArgs = append(checkArgs, args...)
	checkArgs = append(checkArgs, "version")

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return exec.CommandContext(ctx, name, checkArgs...).Run() == nil
}
