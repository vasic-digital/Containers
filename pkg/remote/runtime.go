package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/runtime"
)

// RemoteRuntime implements runtime.ContainerRuntime by executing
// all container commands on a remote host via SSH.
type RemoteRuntime struct {
	host     RemoteHost
	executor RemoteExecutor
	logger   logging.Logger
}

// NewRemoteRuntime creates a RemoteRuntime for the given host.
func NewRemoteRuntime(
	host RemoteHost,
	executor RemoteExecutor,
	logger logging.Logger,
) *RemoteRuntime {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &RemoteRuntime{
		host:     host,
		executor: executor,
		logger:   logger,
	}
}

// Name returns the runtime name prefixed with "remote:".
func (r *RemoteRuntime) Name() string {
	return fmt.Sprintf("remote:%s:%s",
		r.host.Name, r.host.Runtime,
	)
}

// Version returns the container runtime version on the remote host.
func (r *RemoteRuntime) Version(
	ctx context.Context,
) (string, error) {
	result, err := r.exec(ctx, "version --format '{{.Server.Version}}'")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// IsAvailable checks whether the container runtime is usable on
// the remote host.
func (r *RemoteRuntime) IsAvailable(
	ctx context.Context,
) bool {
	result, err := r.exec(ctx, "info --format '{{.ID}}'")
	return err == nil && result.ExitCode == 0
}

// Start starts a container on the remote host.
func (r *RemoteRuntime) Start(
	ctx context.Context,
	id string,
	opts ...runtime.StartOption,
) error {
	cmd := fmt.Sprintf("start %s", shellEscape(id))
	_, err := r.exec(ctx, cmd)
	return err
}

// Stop stops a container on the remote host.
func (r *RemoteRuntime) Stop(
	ctx context.Context,
	id string,
	opts ...runtime.StopOption,
) error {
	cmd := fmt.Sprintf("stop %s", shellEscape(id))
	_, err := r.exec(ctx, cmd)
	return err
}

// Remove removes a container on the remote host.
func (r *RemoteRuntime) Remove(
	ctx context.Context,
	id string,
	opts ...runtime.RemoveOption,
) error {
	cmd := fmt.Sprintf("rm -f %s", shellEscape(id))
	_, err := r.exec(ctx, cmd)
	return err
}

// statusInspectFormat is the Go template passed to `<runtime> inspect
// --format` by Status. The health field is GUARDED with
// `{{if .State.Health}}...{{end}}`: a container created WITHOUT a
// HEALTHCHECK (the common case) has a nil .State.Health pointer (docker
// *types.Health / podman *define.HealthCheckResults). An unguarded
// `{{.State.Health.Status}}` makes the runtime abort the ENTIRE inspect
// with "nil pointer evaluating ...HealthCheckResults.Status" (exit 125),
// so Status returns that error and reports nothing at all. The guard emits
// an empty health field for a no-healthcheck container while still
// reporting a real health status when a healthcheck IS defined. Every
// field stays `|`-separated so parseRemoteStatus still sees 7 fields.
const statusInspectFormat = "{{.Id}}|{{.Name}}|{{.State.Status}}|" +
	"{{if .State.Health}}{{.State.Health.Status}}{{end}}|" +
	"{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}"

// Status returns the status of a container on the remote host.
func (r *RemoteRuntime) Status(
	ctx context.Context, id string,
) (*runtime.ContainerStatus, error) {
	cmd := fmt.Sprintf(
		"inspect --format '%s' %s",
		statusInspectFormat, shellEscape(id),
	)
	result, err := r.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}

	return parseRemoteStatus(result.Stdout)
}

// List returns containers matching the filter on the remote host.
func (r *RemoteRuntime) List(
	ctx context.Context, filter runtime.ListFilter,
) ([]runtime.ContainerInfo, error) {
	args := "ps --format json --no-trunc"
	if filter.All {
		args += " -a"
	}
	for k, v := range filter.Labels {
		args += fmt.Sprintf(" --filter label=%s=%s",
			shellEscape(k), shellEscape(v))
	}
	for _, name := range filter.Names {
		args += fmt.Sprintf(" --filter name=%s", shellEscape(name))
	}
	for _, status := range filter.Status {
		args += fmt.Sprintf(
			" --filter status=%s", shellEscape(string(status)),
		)
	}

	result, err := r.exec(ctx, args)
	if err != nil {
		return nil, err
	}

	return parseContainerList(result.Stdout)
}

// Stats returns resource usage for a container on the remote host.
func (r *RemoteRuntime) Stats(
	ctx context.Context, id string,
) (*runtime.ContainerStats, error) {
	cmd := fmt.Sprintf(
		"stats --no-stream --format "+
			"'{{.CPUPerc}}|{{.MemPerc}}|{{.MemUsage}}|"+
			"{{.NetIO}}|{{.BlockIO}}|{{.PIDs}}' %s",
		shellEscape(id),
	)
	result, err := r.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}

	return parseRemoteStats(result.Stdout)
}

// Exec runs a command inside a container on the remote host.
func (r *RemoteRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*runtime.ExecResult, error) {
	escapedCmd := make([]string, len(cmd))
	for i, c := range cmd {
		escapedCmd[i] = fmt.Sprintf("'%s'",
			strings.ReplaceAll(c, "'", "'\\''"),
		)
	}
	command := fmt.Sprintf(
		"exec %s %s", shellEscape(id), strings.Join(escapedCmd, " "),
	)

	result, err := r.exec(ctx, command)
	if err != nil {
		return nil, err
	}

	return &runtime.ExecResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

// Logs returns a reader for container log output on the remote host.
func (r *RemoteRuntime) Logs(
	ctx context.Context,
	id string,
	opts ...runtime.LogOption,
) (io.ReadCloser, error) {
	cmd := fmt.Sprintf("logs %s", shellEscape(id))
	return r.executor.ExecuteStream(
		ctx, r.host, r.runtimeCmd(cmd),
	)
}

// Host returns the remote host this runtime targets.
func (r *RemoteRuntime) Host() RemoteHost {
	return r.host
}

func (r *RemoteRuntime) exec(
	ctx context.Context, args string,
) (*CommandResult, error) {
	return r.executor.Execute(
		ctx, r.host, r.runtimeCmd(args),
	)
}

func (r *RemoteRuntime) runtimeCmd(args string) string {
	rt := r.host.Runtime
	if rt == "" {
		rt = "docker"
	}
	// RM3 (SECURITY): host.Runtime is verbatim env config
	// (CONTAINERS_REMOTE_HOST_N_RUNTIME) spliced as the LEADING token of the
	// remote `ssh <host> <cmd>` string the remote login shell re-parses —
	// while the container-id / filter args in the SAME command are already
	// shellEscape'd (Start/Stop/Remove/Status/List/Stats/Exec/Logs). Escape rt
	// too so a value like "docker;evilcmd" cannot inject a second remote
	// command; shellEscape is a no-op for the ordinary `[A-Za-z0-9_/.-]`
	// runtime names (docker/podman), so legitimate configs are unchanged.
	return fmt.Sprintf("%s %s", shellEscape(rt), args)
}

func parseRemoteStatus(
	output string,
) (*runtime.ContainerStatus, error) {
	output = strings.TrimSpace(output)
	parts := strings.SplitN(output, "|", 7)
	if len(parts) < 7 {
		return nil, fmt.Errorf(
			"unexpected status output: %s", output,
		)
	}

	exitCode, _ := strconv.Atoi(strings.TrimSpace(parts[6]))
	startedAt, _ := time.Parse(time.RFC3339Nano,
		strings.TrimSpace(parts[4]),
	)
	finishedAt, _ := time.Parse(time.RFC3339Nano,
		strings.TrimSpace(parts[5]),
	)

	return &runtime.ContainerStatus{
		ID:         strings.TrimSpace(parts[0]),
		Name:       strings.TrimPrefix(strings.TrimSpace(parts[1]), "/"),
		State:      runtime.ContainerState(strings.TrimSpace(parts[2])),
		Health:     strings.TrimSpace(parts[3]),
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		ExitCode:   exitCode,
	}, nil
}

func parseContainerList(
	output string,
) ([]runtime.ContainerInfo, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}

	var containers []runtime.ContainerInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		info := runtime.ContainerInfo{
			ID:    getString(raw, "ID"),
			Name:  getString(raw, "Names"),
			Image: getString(raw, "Image"),
			State: runtime.ContainerState(
				getString(raw, "State"),
			),
			Status: getString(raw, "Status"),
		}
		containers = append(containers, info)
	}
	return containers, nil
}

func parseRemoteStats(
	output string,
) (*runtime.ContainerStats, error) {
	output = strings.TrimSpace(output)
	parts := strings.SplitN(output, "|", 6)
	if len(parts) < 6 {
		return nil, fmt.Errorf(
			"unexpected stats output: %s", output,
		)
	}

	return &runtime.ContainerStats{
		CPUPercent:    parsePercent(parts[0]),
		MemoryPercent: parsePercent(parts[1]),
		PIDs:          parseInt(parts[5]),
	}, nil
}

func parsePercent(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "%")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	v, _ := strconv.Atoi(s)
	return v
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
