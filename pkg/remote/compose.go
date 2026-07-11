package remote

import (
	"context"
	"fmt"
	"io"
	"strings"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/logging"
)

// RemoteComposeOrchestrator implements compose.ComposeOrchestrator
// by executing compose commands on a remote host via SSH.
//
// The orchestrator uses intelligent compose command detection with the
// following priority order:
//  1. podman-compose (native Podman implementation - preferred)
//  2. docker compose (v2, plugin-based)
//  3. podman compose (may delegate to docker-compose v1)
//  4. docker-compose (v1, standalone)
//
// If detection fails, it falls back to the host's configured runtime.
type RemoteComposeOrchestrator struct {
	host       RemoteHost
	executor   RemoteExecutor
	composeCmd *ComposeCommand
	detector   *ComposeDetector
	logger     logging.Logger
}

// RemoteComposeOption configures the RemoteComposeOrchestrator.
type RemoteComposeOption func(*RemoteComposeOrchestrator)

// WithComposeCommand forces a specific compose command instead of auto-detection.
func WithComposeCommand(cmd string) RemoteComposeOption {
	return func(o *RemoteComposeOrchestrator) {
		parts := strings.SplitN(cmd, " ", 2)
		o.composeCmd = &ComposeCommand{
			Name:   cmd,
			Binary: parts[0],
		}
		if len(parts) > 1 {
			o.composeCmd.Subcommand = parts[1]
		}
	}
}

// WithComposeDetector sets a custom compose detector.
func WithComposeDetector(detector *ComposeDetector) RemoteComposeOption {
	return func(o *RemoteComposeOrchestrator) {
		o.detector = detector
	}
}

// NewRemoteComposeOrchestrator creates a compose orchestrator that
// operates on a remote host with intelligent compose command detection.
func NewRemoteComposeOrchestrator(
	host RemoteHost,
	executor RemoteExecutor,
	logger logging.Logger,
	opts ...RemoteComposeOption,
) *RemoteComposeOrchestrator {
	if logger == nil {
		logger = logging.NopLogger{}
	}

	o := &RemoteComposeOrchestrator{
		host:     host,
		executor: executor,
		logger:   logger,
	}

	// Apply options
	for _, opt := range opts {
		opt(o)
	}

	// Create default detector if not provided
	if o.detector == nil {
		o.detector = NewComposeDetector(executor, logger)
	}

	// Note: We do NOT pre-set composeCmd based on host.Runtime anymore.
	// The detector will try podman-compose first, then fall back to the
	// host's configured runtime if auto-detection fails.
	// This ensures podman-compose is preferred over "podman compose"
	// which may delegate to incompatible docker-compose v1.

	return o
}

// getComposeCommand returns the compose command to use, detecting if necessary.
//
// It does NOT freeze a low-confidence fallback guess. Detect() caches
// genuine successes permanently but never caches a failure, so once the
// host is healthy the real compose tool is picked up. Only when Detect()
// fails do we use DetectWithFallback's host.Runtime guess — and only for
// THIS call: a later call re-attempts detection. Previously a sync.Once
// froze whatever the first call produced (a genuine tool OR, if the host
// was transiently unready, an unvalidated guess) for the orchestrator's
// entire lifetime, so a host that recovered kept being driven with the
// wrong binary until a brand-new orchestrator was constructed.
func (o *RemoteComposeOrchestrator) getComposeCommand(ctx context.Context) (*ComposeCommand, error) {
	// Explicit override via WithComposeCommand is permanent by design.
	if o.composeCmd != nil {
		return o.composeCmd, nil
	}

	if cmd, err := o.detector.Detect(ctx, o.host); err == nil {
		return cmd, nil
	}

	o.logger.Warn(
		"compose auto-detection failed on %s, using configured runtime %s (will re-attempt on next call)",
		o.host.Name, o.host.Runtime,
	)
	return o.detector.DetectWithFallback(ctx, o.host), nil
}

// composeCmdString returns the compose command string for execution.
func (o *RemoteComposeOrchestrator) composeCmdString(ctx context.Context) (string, error) {
	cmd, err := o.getComposeCommand(ctx)
	if err != nil {
		return "", err
	}
	return cmd.String(), nil
}

// Up creates and starts containers on the remote host.
func (o *RemoteComposeOrchestrator) Up(
	ctx context.Context,
	project compose.ComposeProject,
	opts ...compose.UpOption,
) error {
	cmdStr, err := o.composeCmdString(ctx)
	if err != nil {
		return err
	}

	args := o.projectArgs(project)
	// `--build` forces compose to rebuild images whose Dockerfile or
	// build context changed since the last deploy. Without it,
	// `compose up -d` reuses the cached image even when the Dockerfile
	// reference in the compose file points at a NEW Dockerfile —
	// silently masking any Dockerfile fix the orchestrator just shipped.
	// Layer caches in BuildKit / podman keep the cost low when nothing
	// actually changed.
	args = append(args, "up", "-d", "--build")
	if project.Services != nil {
		// RM2-1: service names are caller-controlled and reach the remote
		// login shell (see projectArgs) — escape each so a name like
		// "svc; rm -rf /" cannot inject a second command.
		for _, svc := range project.Services {
			args = append(args, shellEscape(svc))
		}
	}

	cmd := fmt.Sprintf("%s %s",
		cmdStr, strings.Join(args, " "),
	)
	o.logger.Info("remote compose up on %s: %s",
		o.host.Name, cmd,
	)

	result, err := o.executor.Execute(ctx, o.host, cmd)
	if err != nil {
		return fmt.Errorf(
			"remote compose up on %s: %w", o.host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"remote compose up on %s: exit %d: %s",
			o.host.Name, result.ExitCode, result.Stderr,
		)
	}
	return nil
}

// Down stops and removes containers on the remote host.
func (o *RemoteComposeOrchestrator) Down(
	ctx context.Context,
	project compose.ComposeProject,
	opts ...compose.DownOption,
) error {
	cmdStr, err := o.composeCmdString(ctx)
	if err != nil {
		return err
	}

	args := o.projectArgs(project)
	args = append(args, "down")

	cmd := fmt.Sprintf("%s %s",
		cmdStr, strings.Join(args, " "),
	)
	o.logger.Info("remote compose down on %s: %s",
		o.host.Name, cmd,
	)

	result, err := o.executor.Execute(ctx, o.host, cmd)
	if err != nil {
		return fmt.Errorf(
			"remote compose down on %s: %w", o.host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"remote compose down on %s: exit %d: %s",
			o.host.Name, result.ExitCode, result.Stderr,
		)
	}
	return nil
}

// Status returns the status of services on the remote host.
func (o *RemoteComposeOrchestrator) Status(
	ctx context.Context,
	project compose.ComposeProject,
) ([]compose.ServiceStatus, error) {
	cmd, err := o.getComposeCommand(ctx)
	if err != nil {
		return nil, err
	}

	var cmdStr string

	// podman-compose doesn't support --format flag for ps command
	// Use the container runtime directly (podman or docker) with --format
	if cmd.Binary == "podman-compose" {
		// Use podman ps with label filter for podman-compose projects
		labelFilter := ""
		if project.Name != "" {
			labelFilter = fmt.Sprintf("--filter label=com.docker.compose.project=%s", shellEscape(project.Name))
		}
		cmdStr = fmt.Sprintf("podman ps -a %s --format '{{.Names}}|{{.State}}|{{.Status}}'", labelFilter)
	} else if cmd.Binary == "docker-compose" || (cmd.Binary == "docker" && cmd.Subcommand == "compose") {
		// docker compose and docker-compose support --format
		args := o.projectArgs(project)
		args = append(args, "ps", "-a", "--format",
			"'{{.Name}}|{{.State}}|{{.Status}}'",
		)
		cmdStr = fmt.Sprintf("%s %s", cmd.String(), strings.Join(args, " "))
	} else if cmd.Binary == "podman" && cmd.Subcommand == "compose" {
		// podman compose might delegate to docker-compose, use podman ps directly
		labelFilter := ""
		if project.Name != "" {
			labelFilter = fmt.Sprintf("--filter label=com.docker.compose.project=%s", shellEscape(project.Name))
		}
		cmdStr = fmt.Sprintf("podman ps -a %s --format '{{.Names}}|{{.State}}|{{.Status}}'", labelFilter)
	} else {
		// Fallback: try compose ps with format
		args := o.projectArgs(project)
		args = append(args, "ps", "-a", "--format",
			"'{{.Name}}|{{.State}}|{{.Status}}'",
		)
		cmdStr = fmt.Sprintf("%s %s", cmd.String(), strings.Join(args, " "))
	}

	result, err := o.executor.Execute(ctx, o.host, cmdStr)
	if err != nil {
		return nil, fmt.Errorf(
			"remote compose status on %s: %w", o.host.Name, err,
		)
	}

	return parseRemoteComposeStatus(result.Stdout), nil
}

// Logs returns a reader for service log output on the remote host.
func (o *RemoteComposeOrchestrator) Logs(
	ctx context.Context,
	project compose.ComposeProject,
	service string,
) (io.ReadCloser, error) {
	cmdStr, err := o.composeCmdString(ctx)
	if err != nil {
		return nil, err
	}

	args := o.projectArgs(project)
	// RM2-1: the service name is caller-controlled and reaches the remote
	// login shell (see projectArgs) — escape it to close the injection path.
	args = append(args, "logs", "--no-color", shellEscape(service))

	cmd := fmt.Sprintf("%s %s",
		cmdStr, strings.Join(args, " "),
	)

	return o.executor.ExecuteStream(ctx, o.host, cmd)
}

// Host returns the remote host this orchestrator targets.
func (o *RemoteComposeOrchestrator) Host() RemoteHost {
	return o.host
}

// ComposeCommand returns the detected or configured compose command.
func (o *RemoteComposeOrchestrator) ComposeCommand(ctx context.Context) (*ComposeCommand, error) {
	return o.getComposeCommand(ctx)
}

func (o *RemoteComposeOrchestrator) projectArgs(
	project compose.ComposeProject,
) []string {
	// RM2-1: File/Name/Profile are caller-controlled and get spliced into
	// the command STRING that Up/Down/Status/Logs hand to executor.Execute,
	// which runs `ssh <host> <cmd>` — the REMOTE login shell then re-parses
	// that string. A value with a shell metacharacter (space, `;`, `$()`,
	// backtick, quote, newline) would inject a second command running with
	// the SSH user's privileges. shellEscape (single-quote wrap, no-op for
	// the common `[A-Za-z0-9_/.-]`-only paths/names) closes this the same
	// way the sibling runtime.go escapes container ids + list filters.
	var args []string
	if project.File != "" {
		args = append(args, "-f", shellEscape(project.File))
	}
	if project.Name != "" {
		args = append(args, "--project-name", shellEscape(project.Name))
	}
	if project.Profile != "" {
		args = append(args, "--profile", shellEscape(project.Profile))
	}
	return args
}

func parseRemoteComposeStatus(
	output string,
) []compose.ServiceStatus {
	var statuses []compose.ServiceStatus
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "'\"")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}

		name := strings.TrimSpace(parts[0])
		state := strings.TrimSpace(parts[1])
		health := ""

		// Status might contain health info like "running (healthy)"
		if len(parts) > 2 {
			statusPart := strings.TrimSpace(parts[2])
			// Extract health from status if present
			if strings.Contains(statusPart, "(healthy)") {
				health = "healthy"
			} else if strings.Contains(statusPart, "(unhealthy)") {
				health = "unhealthy"
			} else if strings.Contains(statusPart, "(health: starting)") {
				health = "starting"
			}
		}

		// Normalize state
		state = strings.ToLower(state)
		if strings.Contains(state, "running") {
			state = "running"
		} else if strings.Contains(state, "exited") || strings.Contains(state, "stopped") {
			state = "exited"
		} else if strings.Contains(state, "paused") {
			state = "paused"
		}

		statuses = append(statuses, compose.ServiceStatus{
			Name:   name,
			State:  state,
			Health: health,
		})
	}
	return statuses
}
