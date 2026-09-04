package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// PodmanRuntime implements ContainerRuntime using the podman CLI.
// Podman is largely command-compatible with Docker, so this
// implementation follows the same patterns with podman-specific
// adjustments.
type PodmanRuntime struct {
	binary   string
	executor CommandExecutor
}

// NewPodmanRuntime creates a PodmanRuntime with the real podman binary.
func NewPodmanRuntime() *PodmanRuntime {
	return &PodmanRuntime{
		binary:   "podman",
		executor: &defaultExecutor{},
	}
}

// NewPodmanRuntimeWithExecutor creates a PodmanRuntime with a custom
// executor, primarily for testing.
func NewPodmanRuntimeWithExecutor(exec CommandExecutor) *PodmanRuntime {
	return &PodmanRuntime{
		binary:   "podman",
		executor: exec,
	}
}

func (p *PodmanRuntime) Name() string {
	return "podman"
}

func (p *PodmanRuntime) Version(
	ctx context.Context,
) (string, error) {
	out, err := p.executor.Execute(
		ctx, p.binary, "version", "--format", "{{.Server.Version}}",
	)
	if err != nil {
		// Podman rootless may only have client version.
		out, err = p.executor.Execute(
			ctx, p.binary, "version", "--format",
			"{{.Client.Version}}",
		)
		if err != nil {
			return "", fmt.Errorf("podman version: %w", err)
		}
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *PodmanRuntime) IsAvailable(ctx context.Context) bool {
	_, err := p.executor.Execute(ctx, p.binary, "info")
	return err == nil
}

func (p *PodmanRuntime) Start(
	ctx context.Context, id string, opts ...StartOption,
) error {
	_ = applyStartOptions(opts)
	args := []string{"start", id}
	_, err := p.executor.Execute(ctx, p.binary, args...)
	if err != nil {
		return fmt.Errorf("podman start %s: %w", id, err)
	}
	return nil
}

func (p *PodmanRuntime) Stop(
	ctx context.Context, id string, opts ...StopOption,
) error {
	o := applyStopOptions(opts)
	timeoutSec := int(o.Timeout.Seconds())
	args := []string{"stop", "-t", strconv.Itoa(timeoutSec), id}
	_, err := p.executor.Execute(ctx, p.binary, args...)
	if err != nil {
		return fmt.Errorf("podman stop %s: %w", id, err)
	}
	return nil
}

func (p *PodmanRuntime) Remove(
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
	_, err := p.executor.Execute(ctx, p.binary, args...)
	if err != nil {
		return fmt.Errorf("podman rm %s: %w", id, err)
	}
	return nil
}

func (p *PodmanRuntime) Status(
	ctx context.Context, id string,
) (*ContainerStatus, error) {
	out, err := p.executor.Execute(
		ctx, p.binary, "inspect", "--format", "json", id,
	)
	if err != nil {
		return nil, fmt.Errorf("podman inspect %s: %w", id, err)
	}
	return parseDockerInspectStatus(out)
}

func (p *PodmanRuntime) List(
	ctx context.Context, filter ListFilter,
) ([]ContainerInfo, error) {
	args := []string{"ps", "--format", "json", "--no-trunc"}
	if filter.All {
		args = append(args, "-a")
	}
	for k, v := range filter.Labels {
		args = append(args,
			"--filter", fmt.Sprintf("label=%s=%s", k, v),
		)
	}
	for _, name := range filter.Names {
		args = append(args, "--filter", fmt.Sprintf("name=%s", name))
	}
	for _, status := range filter.Status {
		args = append(args,
			"--filter", fmt.Sprintf("status=%s", status),
		)
	}

	out, err := p.executor.Execute(ctx, p.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	return parsePodmanPSOutput(out)
}

// podmanPSJSON models podman ps --format json output.
// Podman returns a JSON array unlike Docker's newline-delimited JSON.
type podmanPSJSON struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created interface{}       `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

func parsePodmanPSOutput(data []byte) ([]ContainerInfo, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}

	var items []podmanPSJSON
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		return nil, fmt.Errorf("parsing podman ps output: %w", err)
	}

	var containers []ContainerInfo
	for _, item := range items {
		name := ""
		if len(item.Names) > 0 {
			name = item.Names[0]
		}
		labels := item.Labels
		if labels == nil {
			labels = make(map[string]string)
		}
		containers = append(containers, ContainerInfo{
			ID:      item.ID,
			Name:    name,
			Image:   item.Image,
			ImageID: item.ImageID,
			State:   mapContainerState(item.State),
			Status:  item.Status,
			Created: parsePodmanCreated(item.Created),
			Labels:  labels,
		})
	}
	return containers, nil
}

// parsePodmanCreated normalises podman ps --format json's polymorphic
// Created field. Real podman emits a unix-SECONDS number — captured evidence:
// pkg/ctop/wave18_real_runtime_output_test.go's realPodmanPS fixture carries
// `"Created": 1783192758`, taken from real `podman ps -a --format json`
// output on this project's own rootless-podman build host (§11.4.161). A
// JSON number decoded into an `interface{}` field always arrives as float64,
// never int64, so that is the primary case handled here. Some podman-
// compatible tooling instead emits an RFC3339 string, so that shape is
// tolerated too. Any other shape (or a non-positive timestamp) yields the
// zero time rather than a decode failure — Created is best-effort listing
// metadata, never a hard List() error.
func parsePodmanCreated(raw interface{}) time.Time {
	switch v := raw.(type) {
	case float64:
		if v <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(v), 0).UTC()
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

func (p *PodmanRuntime) Stats(
	ctx context.Context, id string,
) (*ContainerStats, error) {
	out, err := p.executor.Execute(
		ctx, p.binary, "stats", "--no-stream", "--format", "json", id,
	)
	if err != nil {
		return nil, fmt.Errorf("podman stats %s: %w", id, err)
	}
	return parsePodmanStats(out)
}

// podmanStatsJSON models the REAL `podman stats --no-stream --format json`
// output: a JSON array of objects whose every field is a STRING, with combined
// `net_io`/`block_io` "rx / tx" mappings and a combined `mem_usage` "used /
// limit". This is what real podman emits; the earlier numeric-field struct
// (separate net_input/net_output, uint64 mem, float cpu) matched NOTHING
// podman produces, so every real container's stats unmarshal failed and fell
// through to parseDockerStats — which then failed on the leading `[`, leaving
// PodmanRuntime.Stats returning an error for every real container.
type podmanStatsJSON struct {
	CPUPercent string `json:"cpu_percent"`
	MemPercent string `json:"mem_percent"`
	MemUsage   string `json:"mem_usage"`
	NetIO      string `json:"net_io"`
	BlockIO    string `json:"block_io"`
	PIDs       string `json:"pids"`
}

func parsePodmanStats(data []byte) (*ContainerStats, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("empty stats output")
	}

	// Real podman emits a JSON array of string-valued objects. Decode it and
	// string-parse each field with the same helpers docker.go uses.
	if trimmed[0] == '[' {
		var items []podmanStatsJSON
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, fmt.Errorf("parsing podman stats: %w", err)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("no stats data returned")
		}
		return podmanStatsToContainerStats(items[0]), nil
	}

	// Not an array → a docker-shim single object / first-line docker shape.
	return parseDockerStats(data)
}

// podmanStatsToContainerStats string-parses one real podman stats record,
// reusing docker.go's percentage / mem / IO-pair string helpers.
func podmanStatsToContainerStats(s podmanStatsJSON) *ContainerStats {
	memUsage, memLimit := parseMemUsage(s.MemUsage)
	netRx, netTx := parseIOPair(s.NetIO)
	blockR, blockW := parseIOPair(s.BlockIO)
	pids, _ := strconv.Atoi(strings.TrimSpace(s.PIDs))
	return &ContainerStats{
		CPUPercent:    parsePercentage(s.CPUPercent),
		MemoryPercent: parsePercentage(s.MemPercent),
		MemoryUsage:   memUsage,
		MemoryLimit:   memLimit,
		NetworkRx:     netRx,
		NetworkTx:     netTx,
		BlockRead:     blockR,
		BlockWrite:    blockW,
		PIDs:          pids,
	}
}

func (p *PodmanRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*ExecResult, error) {
	args := append([]string{"exec", id}, cmd...)
	stdout, stderr, exitCode, err := p.executor.ExecuteWithStderr(
		ctx, p.binary, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("podman exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (p *PodmanRuntime) Logs(
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
	// Podman's --tail is an INTEGER flag (default -1 = "all lines"); it does
	// NOT accept Docker's "all" string sentinel, which is exactly what
	// defaultLogOptions() supplies. Measured on this project's build host with
	// podman 5.7.1:
	//
	//	$ podman logs --tail all workshop-curriculum_platform_1
	//	Error: invalid argument "all" for "--tail" flag:
	//	       strconv.ParseInt: parsing "all": invalid syntax   (exit 125)
	//
	// Every DEFAULT-OPTION call to PodmanRuntime.Logs therefore ran a command
	// that produced no stdout at all. Guard it the way crio.go and lxd.go
	// already guard their integer --tail flags: pass a genuinely-numeric Tail
	// through, and OMIT the flag entirely for "all" (or anything non-numeric)
	// — podman's own default already means every line, so omitting it is the
	// requested semantics rather than an approximation of them.
	if o.Tail != "" {
		if n, err := strconv.Atoi(o.Tail); err == nil {
			args = append(args, "--tail", strconv.Itoa(n))
		}
	}
	args = append(args, id)

	rc, err := p.executor.ExecuteStream(ctx, p.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("podman logs %s: %w", id, err)
	}
	return rc, nil
}

// Run performs a one-shot `podman run` — see ContainerRuntime.Run.
func (p *PodmanRuntime) Run(
	ctx context.Context, image string, cmd []string, opts ...RunOption,
) (*ExecResult, error) {
	return runViaCLI(ctx, p.executor, p.binary, "podman", image, cmd, opts)
}
