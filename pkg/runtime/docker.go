package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommandExecutor abstracts os/exec for testing.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
	ExecuteWithStderr(
		ctx context.Context, name string, args ...string,
	) ([]byte, []byte, int, error)
	ExecuteStream(
		ctx context.Context, name string, args ...string,
	) (io.ReadCloser, error)
}

// StreamCmd wraps exec.Cmd methods for streaming.
type StreamCmd interface {
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

// StreamCmdFactory creates StreamCmd instances.
type StreamCmdFactory interface {
	CommandContext(ctx context.Context, name string, args ...string) StreamCmd
}

// realStreamCmdFactory creates real exec.Cmd instances.
type realStreamCmdFactory struct{}

func (realStreamCmdFactory) CommandContext(
	ctx context.Context, name string, args ...string,
) StreamCmd {
	return exec.CommandContext(ctx, name, args...)
}

// defaultExecutor uses real os/exec commands.
type defaultExecutor struct {
	streamFactory StreamCmdFactory
}

// newDefaultExecutor creates a defaultExecutor with the real factory.
func newDefaultExecutor() *defaultExecutor {
	return &defaultExecutor{streamFactory: realStreamCmdFactory{}}
}

func (e *defaultExecutor) Execute(
	ctx context.Context, name string, args ...string,
) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		// cmd.Output() already captures the child's stderr into
		// (*exec.ExitError).Stderr internally (up to 32KiB, via Go's
		// prefixSuffixSaver) when cmd.Stderr is nil — but ExitError.Error()
		// only renders "exit status N" and never includes that captured
		// text. Every caller across all six runtimes (Start/Stop/Remove/
		// Status/List/Stats/Version) wraps this error with %w, so the real
		// CLI diagnostic ("no such container", "permission denied", "daemon
		// unreachable") was silently discarded. Surface it by appending the
		// trimmed stderr text to the returned error while still preserving
		// %w-wrapping of the original *exec.ExitError.
		if exitErr, ok := err.(*exec.ExitError); ok {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return out, fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return out, err
	}
	return out, nil
}

func (e *defaultExecutor) ExecuteWithStderr(
	ctx context.Context, name string, args ...string,
) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
		err = nil
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode, err
}

func (e *defaultExecutor) ExecuteStream(
	ctx context.Context, name string, args ...string,
) (io.ReadCloser, error) {
	factory := e.streamFactory
	if factory == nil {
		factory = realStreamCmdFactory{}
	}
	cmd := factory.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting command: %w", err)
	}
	return &streamCmdReadCloser{ReadCloser: stdout, cmd: cmd}, nil
}

// streamCmdReadCloser waits for the command to finish on Close.
type streamCmdReadCloser struct {
	io.ReadCloser
	cmd StreamCmd
}

func (c *streamCmdReadCloser) Close() error {
	_ = c.ReadCloser.Close()
	return c.cmd.Wait()
}

// DockerRuntime implements ContainerRuntime using the docker CLI.
type DockerRuntime struct {
	binary   string
	executor CommandExecutor
}

// NewDockerRuntime creates a DockerRuntime with the real docker binary.
func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{
		binary:   "docker",
		executor: newDefaultExecutor(),
	}
}

// NewDockerRuntimeWithExecutor creates a DockerRuntime with a custom
// executor, primarily for testing.
func NewDockerRuntimeWithExecutor(exec CommandExecutor) *DockerRuntime {
	return &DockerRuntime{
		binary:   "docker",
		executor: exec,
	}
}

func (d *DockerRuntime) Name() string {
	return "docker"
}

func (d *DockerRuntime) Version(
	ctx context.Context,
) (string, error) {
	out, err := d.executor.Execute(
		ctx, d.binary, "version", "--format", "{{.Server.Version}}",
	)
	if err != nil {
		return "", fmt.Errorf("docker version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *DockerRuntime) IsAvailable(ctx context.Context) bool {
	_, err := d.executor.Execute(ctx, d.binary, "info")
	return err == nil
}

func (d *DockerRuntime) Start(
	ctx context.Context, id string, opts ...StartOption,
) error {
	_ = applyStartOptions(opts)
	args := []string{"start", id}
	_, err := d.executor.Execute(ctx, d.binary, args...)
	if err != nil {
		return fmt.Errorf("docker start %s: %w", id, err)
	}
	return nil
}

func (d *DockerRuntime) Stop(
	ctx context.Context, id string, opts ...StopOption,
) error {
	o := applyStopOptions(opts)
	timeoutSec := int(o.Timeout.Seconds())
	args := []string{"stop", "-t", strconv.Itoa(timeoutSec), id}
	_, err := d.executor.Execute(ctx, d.binary, args...)
	if err != nil {
		return fmt.Errorf("docker stop %s: %w", id, err)
	}
	return nil
}

func (d *DockerRuntime) Remove(
	ctx context.Context, id string, opts ...RemoveOption,
) error {
	o := applyRemoveOptions(opts)
	args := []string{"rm"}
	if o.Force {
		args = append(args, "-f")
	}
	if o.Volumes {
		args = append(args, "-v")
	}
	args = append(args, id)
	_, err := d.executor.Execute(ctx, d.binary, args...)
	if err != nil {
		return fmt.Errorf("docker rm %s: %w", id, err)
	}
	return nil
}

func (d *DockerRuntime) Status(
	ctx context.Context, id string,
) (*ContainerStatus, error) {
	out, err := d.executor.Execute(
		ctx, d.binary, "inspect", "--format", "json", id,
	)
	if err != nil {
		return nil, fmt.Errorf("docker inspect %s: %w", id, err)
	}
	return parseDockerInspectStatus(out)
}

// dockerInspectJSON models the relevant fields from docker inspect output.
type dockerInspectJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
	Image           string `json:"Image"`
	Created         string `json:"Created"`
	NetworkSettings struct {
		Networks map[string]interface{} `json:"Networks"`
		Ports    map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func parseDockerInspectStatus(data []byte) (*ContainerStatus, error) {
	var inspects []dockerInspectJSON
	if err := json.Unmarshal(data, &inspects); err != nil {
		return nil, fmt.Errorf("parsing inspect output: %w", err)
	}
	if len(inspects) == 0 {
		return nil, fmt.Errorf("no container found in inspect output")
	}
	ins := inspects[0]

	startedAt, _ := time.Parse(time.RFC3339Nano, ins.State.StartedAt)
	finishedAt, _ := time.Parse(time.RFC3339Nano, ins.State.FinishedAt)

	ports := parseInspectPorts(ins)

	return &ContainerStatus{
		ID:         ins.ID,
		Name:       strings.TrimPrefix(ins.Name, "/"),
		State:      mapContainerState(ins.State.Status),
		Health:     ins.State.Status,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		ExitCode:   ins.State.ExitCode,
		Ports:      ports,
	}, nil
}

func parseInspectPorts(ins dockerInspectJSON) []PortMapping {
	var ports []PortMapping
	for containerPort, bindings := range ins.NetworkSettings.Ports {
		parts := strings.SplitN(containerPort, "/", 2)
		port := parts[0]
		proto := "tcp"
		if len(parts) == 2 {
			proto = parts[1]
		}
		for _, b := range bindings {
			ports = append(ports, PortMapping{
				HostIP:        b.HostIP,
				HostPort:      b.HostPort,
				ContainerPort: port,
				Protocol:      proto,
			})
		}
	}
	return ports
}

func (d *DockerRuntime) List(
	ctx context.Context, filter ListFilter,
) ([]ContainerInfo, error) {
	args := []string{"ps", "--format", "json", "--no-trunc"}
	if filter.All {
		args = append(args, "-a")
	}
	for k, v := range filter.Labels {
		args = append(args, "--filter", fmt.Sprintf("label=%s=%s", k, v))
	}
	for _, name := range filter.Names {
		args = append(args, "--filter", fmt.Sprintf("name=%s", name))
	}
	for _, status := range filter.Status {
		args = append(args, "--filter", fmt.Sprintf("status=%s", status))
	}

	out, err := d.executor.Execute(ctx, d.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	return parseDockerPSOutput(out)
}

// dockerPSJSON models one line of `docker ps --format json` output.
type dockerPSJSON struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Created string `json:"CreatedAt"`
	Labels  string `json:"Labels"`
	Ports   string `json:"Ports"`
}

func parseDockerPSOutput(data []byte) ([]ContainerInfo, error) {
	var containers []ContainerInfo
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ps dockerPSJSON
		if err := json.Unmarshal([]byte(line), &ps); err != nil {
			continue
		}
		labels := parseLabelsString(ps.Labels)
		containers = append(containers, ContainerInfo{
			ID:     ps.ID,
			Name:   ps.Names,
			Image:  ps.Image,
			State:  mapContainerState(ps.State),
			Status: ps.Status,
			Labels: labels,
		})
	}
	return containers, nil
}

// parseLabelsString parses `docker ps --format json`'s "Labels" field, a
// flat comma-joined "k1=v1,k2=v2" string.
//
// RT-LABEL-1 (§11.4.6 honest documentation, not a fix): a label VALUE
// containing a literal comma (e.g. `description=hello, world`) is silently
// truncated/split by this parser, because Docker's own `docker ps` JSON
// formatter joins labels with "," and provides NO escaping mechanism for a
// comma embedded in a value — this is a genuine upstream format limitation,
// not a bug introduced here (confirmed: moby/moby#30575, "Unable to use
// docker ps --filter when a label value has a comma", the same unescaped-
// comma-join format). There is no `docker ps --format` variant that emits
// Labels as a structured map instead of this flat string; obtaining
// unambiguous label values would require a separate `docker inspect
// --format '{{json .Config.Labels}}'` call per container, which changes
// List()'s round-trip cost/contract and is out of scope for a surgical fix.
// See wave20_rthard_test.go's RT-LABEL-1 case for a test that documents
// (rather than "fixes") this known truncation behavior.
func parseLabelsString(s string) map[string]string {
	labels := make(map[string]string)
	if s == "" {
		return labels
	}
	pairs := strings.Split(s, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			labels[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return labels
}

func (d *DockerRuntime) Stats(
	ctx context.Context, id string,
) (*ContainerStats, error) {
	out, err := d.executor.Execute(
		ctx, d.binary, "stats", "--no-stream", "--format", "json", id,
	)
	if err != nil {
		return nil, fmt.Errorf("docker stats %s: %w", id, err)
	}
	return parseDockerStats(out)
}

// dockerStatsJSON models `docker stats --format json` output.
type dockerStatsJSON struct {
	CPUPerc  string `json:"CPUPerc"`
	MemPerc  string `json:"MemPerc"`
	MemUsage string `json:"MemUsage"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
	PIDs     string `json:"PIDs"`
}

func parseDockerStats(data []byte) (*ContainerStats, error) {
	line := strings.TrimSpace(string(data))
	if line == "" {
		return nil, fmt.Errorf("empty stats output")
	}
	// Take only the first line if multiple.
	if idx := strings.Index(line, "\n"); idx >= 0 {
		line = line[:idx]
	}

	var s dockerStatsJSON
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return nil, fmt.Errorf("parsing stats: %w", err)
	}

	cpuPct := parsePercentage(s.CPUPerc)
	memPct := parsePercentage(s.MemPerc)
	memUsage, memLimit := parseMemUsage(s.MemUsage)
	netRx, netTx := parseIOPair(s.NetIO)
	blockR, blockW := parseIOPair(s.BlockIO)
	pids, _ := strconv.Atoi(strings.TrimSpace(s.PIDs))

	return &ContainerStats{
		CPUPercent:    cpuPct,
		MemoryPercent: memPct,
		MemoryUsage:   memUsage,
		MemoryLimit:   memLimit,
		NetworkRx:     netRx,
		NetworkTx:     netTx,
		BlockRead:     blockR,
		BlockWrite:    blockW,
		PIDs:          pids,
	}, nil
}

func parsePercentage(s string) float64 {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func parseMemUsage(s string) (usage, limit uint64) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		usage = parseSizeToBytes(strings.TrimSpace(parts[0]))
		limit = parseSizeToBytes(strings.TrimSpace(parts[1]))
	}
	return
}

func parseIOPair(s string) (uint64, uint64) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		return parseSizeToBytes(strings.TrimSpace(parts[0])),
			parseSizeToBytes(strings.TrimSpace(parts[1]))
	}
	return 0, 0
}

// sizeSuffix pairs a unit suffix with its byte multiplier.
type sizeSuffix struct {
	suffix string
	mult   uint64
}

// sizeSuffixes ordered longest-first to avoid partial matches.
var sizeSuffixes = []sizeSuffix{
	{"TiB", 1024 * 1024 * 1024 * 1024},
	{"GiB", 1024 * 1024 * 1024},
	{"MiB", 1024 * 1024},
	{"KiB", 1024},
	{"TB", 1000 * 1000 * 1000 * 1000},
	{"GB", 1000 * 1000 * 1000},
	{"MB", 1000 * 1000},
	{"Mi", 1024 * 1024},
	{"kB", 1000},
	{"KB", 1024},
	{"B", 1},
}

func parseSizeToBytes(s string) uint64 {
	s = strings.TrimSpace(s)
	for _, ss := range sizeSuffixes {
		if strings.HasSuffix(s, ss.suffix) {
			numStr := strings.TrimSpace(
				strings.TrimSuffix(s, ss.suffix),
			)
			v, _ := strconv.ParseFloat(numStr, 64)
			return uint64(v * float64(ss.mult))
		}
	}
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func (d *DockerRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*ExecResult, error) {
	args := append([]string{"exec", id}, cmd...)
	stdout, stderr, exitCode, err := d.executor.ExecuteWithStderr(
		ctx, d.binary, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("docker exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (d *DockerRuntime) Logs(
	ctx context.Context, id string, opts ...LogOption,
) (io.ReadCloser, error) {
	o := applyLogOptions(opts)
	args := []string{"logs"}
	if o.Follow {
		args = append(args, "-f")
	}
	if o.Since != "" {
		args = append(args, "--since", o.Since)
	}
	if o.Until != "" {
		args = append(args, "--until", o.Until)
	}
	if o.Tail != "" {
		args = append(args, "--tail", o.Tail)
	}
	args = append(args, id)

	rc, err := d.executor.ExecuteStream(ctx, d.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("docker logs %s: %w", id, err)
	}
	return rc, nil
}

// mapContainerState converts a docker state string to ContainerState.
func mapContainerState(state string) ContainerState {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return StateRunning
	case "exited", "stopped":
		return StateStopped
	case "created":
		return StateCreated
	case "paused":
		return StatePaused
	case "restarting":
		return StateRestarting
	case "removing":
		return StateRemoving
	case "dead":
		return StateDead
	default:
		return ContainerState(state)
	}
}
