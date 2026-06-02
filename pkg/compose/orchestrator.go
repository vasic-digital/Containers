package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		composeCmd:      cmd,
		composeArgs:     args,
		workDir:         workDir,
		logger:          logger,
		cmdFactory:      realCmdFactory{},
		isPodmanCompose: isPodmanComposeCmd(cmd, args),
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
		return o.waitForServices(ctx, project, cfg.Timeout)
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
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		statuses, err := o.Status(ctx, project)
		if err != nil {
			lastErr = err
		} else if servicesReady(statuses) {
			o.logger.Debug(
				"podman-compose wait: all services ready (%d)",
				len(statuses),
			)
			return nil
		} else {
			lastErr = fmt.Errorf(
				"services not ready: %s", summarizeStatuses(statuses),
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

	allArgs := append(o.composeArgs, args...)
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
	allArgs := append(o.composeArgs, args...)
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
	allArgs := append(o.composeArgs, args...)
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

// detectComposeCmd tries to find a working compose command, preferring
// Docker Compose v2 plugin, then standalone docker-compose, then
// podman-compose, then podman compose.
func detectComposeCmd() (string, []string, error) {
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
		checkArgs := append(c.args, "version")
		cmd := exec.Command(c.cmd, checkArgs...)
		if err := cmd.Run(); err == nil {
			return c.cmd, c.args, nil
		}
	}

	return "", nil, fmt.Errorf(
		"no compose command found: tried docker compose, " +
			"docker-compose, podman-compose, podman compose",
	)
}
