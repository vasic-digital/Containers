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

// LXDRuntime implements ContainerRuntime using the lxc CLI.
// LXD is a system container manager that supports Linux containers
// and virtual machines with advanced features like live migration,
// snapshots, and storage management.
type LXDRuntime struct {
	binary   string
	executor CommandExecutor
}

// NewLXDRuntime creates an LXDRuntime with the real lxc binary.
func NewLXDRuntime() *LXDRuntime {
	return &LXDRuntime{
		binary:   "lxc",
		executor: &defaultExecutor{},
	}
}

// NewLXDRuntimeWithExecutor creates an LXDRuntime with a custom
// executor, primarily for testing.
func NewLXDRuntimeWithExecutor(exec CommandExecutor) *LXDRuntime {
	return &LXDRuntime{
		binary:   "lxc",
		executor: exec,
	}
}

func (l *LXDRuntime) Name() string {
	return "lxd"
}

func (l *LXDRuntime) Version(ctx context.Context) (string, error) {
	out, err := l.executor.Execute(ctx, l.binary, "version")
	if err != nil {
		return "", fmt.Errorf("lxc version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (l *LXDRuntime) IsAvailable(ctx context.Context) bool {
	_, err := l.executor.Execute(ctx, l.binary, "list", "--format", "json")
	return err == nil
}

func (l *LXDRuntime) Start(ctx context.Context, id string, opts ...StartOption) error {
	_ = applyStartOptions(opts)
	args := []string{"start", id}
	_, err := l.executor.Execute(ctx, l.binary, args...)
	if err != nil {
		return fmt.Errorf("lxc start %s: %w", id, err)
	}
	return nil
}

func (l *LXDRuntime) Stop(ctx context.Context, id string, opts ...StopOption) error {
	o := applyStopOptions(opts)
	args := []string{"stop", id}
	if o.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(int(o.Timeout.Seconds())))
	}
	_, err := l.executor.Execute(ctx, l.binary, args...)
	if err != nil {
		return fmt.Errorf("lxc stop %s: %w", id, err)
	}
	return nil
}

func (l *LXDRuntime) Remove(ctx context.Context, id string, opts ...RemoveOption) error {
	o := applyRemoveOptions(opts)
	args := []string{"delete", id}
	if o.Force {
		args = append(args, "--force")
	}
	_, err := l.executor.Execute(ctx, l.binary, args...)
	if err != nil {
		return fmt.Errorf("lxc delete %s: %w", id, err)
	}
	return nil
}

func (l *LXDRuntime) Status(ctx context.Context, id string) (*ContainerStatus, error) {
	out, err := l.executor.Execute(ctx, l.binary, "list", id, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("lxc list %s: %w", id, err)
	}
	return parseLXDStatus(out, id)
}

type lxdContainerJSON struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Status       string                 `json:"status"`
	StatusCode   int                    `json:"status_code"`
	Architecture string                 `json:"architecture"`
	Config       map[string]string      `json:"config"`
	Devices      map[string]interface{} `json:"devices"`
	CreatedAt    time.Time              `json:"created_at"`
	LastUsedAt   time.Time              `json:"last_used_at"`
	State        *lxdStateJSON          `json:"state,omitempty"`
}

type lxdStateJSON struct {
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
	CPU        struct {
		Usage int64 `json:"usage"`
	} `json:"cpu"`
	Memory struct {
		Usage     int64 `json:"usage"`
		UsagePeak int64 `json:"usage_peak"`
		Limit     int64 `json:"limit"`
	} `json:"memory"`
	Network map[string]struct {
		Addresses []struct {
			Family  string `json:"family"`
			Address string `json:"address"`
		} `json:"addresses"`
		Counters struct {
			BytesReceived int64 `json:"bytes_received"`
			BytesSent     int64 `json:"bytes_sent"`
		} `json:"counters"`
	} `json:"network"`
	Pid int `json:"pid"`
}

// parseLXDStatus selects the container whose name EXACTLY matches id. `lxc
// list <id>` is a substring/partial filter, so it can return several
// containers (e.g. "web" also matches "web-staging" and "webapp"); taking
// containers[0] blindly would report a DIFFERENT container's status than the
// one asked for (§11.4.111 — resolve by stable name, never by loose match).
func parseLXDStatus(data []byte, id string) (*ContainerStatus, error) {
	var containers []lxdContainerJSON
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("parsing lxc list output: %w", err)
	}

	c, ok := findLXDByName(containers, id)
	if !ok {
		return nil, fmt.Errorf("no container found named %q", id)
	}
	state := mapLXDStatusToState(c.Status, c.StatusCode)

	var startedAt time.Time
	if c.State != nil {
		startedAt = c.LastUsedAt
	}

	return &ContainerStatus{
		ID:        c.Name,
		Name:      c.Name,
		State:     state,
		Health:    c.Status,
		StartedAt: startedAt,
	}, nil
}

// findLXDByName returns the container whose name exactly equals id.
func findLXDByName(
	containers []lxdContainerJSON, id string,
) (lxdContainerJSON, bool) {
	for _, c := range containers {
		if c.Name == id {
			return c, true
		}
	}
	return lxdContainerJSON{}, false
}

func mapLXDStatusToState(status string, code int) ContainerState {
	switch {
	case code == 103 || strings.ToLower(status) == "running":
		return StateRunning
	case code == 102 || strings.ToLower(status) == "stopped":
		return StateStopped
	case code == 101 || strings.ToLower(status) == "frozen":
		return StatePaused
	case code == 106 || strings.ToLower(status) == "starting":
		return StateRestarting
	default:
		return ContainerState(status)
	}
}

func (l *LXDRuntime) List(ctx context.Context, filter ListFilter) ([]ContainerInfo, error) {
	args := []string{"list", "--format", "json"}

	out, err := l.executor.Execute(ctx, l.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("lxc list: %w", err)
	}

	return parseLXDList(out, filter)
}

func parseLXDList(data []byte, filter ListFilter) ([]ContainerInfo, error) {
	var containers []lxdContainerJSON
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("parsing lxc list output: %w", err)
	}

	var result []ContainerInfo
	for _, c := range containers {
		state := mapLXDStatusToState(c.Status, c.StatusCode)

		if len(filter.Status) > 0 {
			matched := false
			for _, s := range filter.Status {
				if s == state {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		if len(filter.Names) > 0 {
			matched := false
			for _, name := range filter.Names {
				if strings.Contains(c.Name, name) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		labels := make(map[string]string)
		for k, v := range c.Config {
			if strings.HasPrefix(k, "user.") {
				labels[strings.TrimPrefix(k, "user.")] = v
			}
		}

		image := c.Config["image.alias"]
		if image == "" {
			image = c.Description
		}

		result = append(result, ContainerInfo{
			ID:      c.Name,
			Name:    c.Name,
			Image:   image,
			State:   state,
			Status:  c.Status,
			Created: c.CreatedAt,
			Labels:  labels,
		})
	}

	return result, nil
}

func (l *LXDRuntime) Stats(ctx context.Context, id string) (*ContainerStats, error) {
	out, err := l.executor.Execute(ctx, l.binary, "info", id, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("lxc info %s: %w", id, err)
	}
	return parseLXDStats(out)
}

func parseLXDStats(data []byte) (*ContainerStats, error) {
	var info struct {
		State *lxdStateJSON `json:"state,omitempty"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parsing lxc info output: %w", err)
	}

	if info.State == nil {
		return &ContainerStats{}, nil
	}

	s := info.State
	memUsage := uint64(s.Memory.Usage)
	memLimit := uint64(s.Memory.Limit)
	memPct := 0.0
	if memLimit > 0 {
		memPct = float64(memUsage) / float64(memLimit) * 100
	}

	var netRx, netTx uint64
	for _, net := range s.Network {
		netRx += uint64(net.Counters.BytesReceived)
		netTx += uint64(net.Counters.BytesSent)
	}

	return &ContainerStats{
		MemoryUsage:   memUsage,
		MemoryLimit:   memLimit,
		MemoryPercent: memPct,
		NetworkRx:     netRx,
		NetworkTx:     netTx,
		PIDs:          s.Pid,
	}, nil
}

func (l *LXDRuntime) Exec(ctx context.Context, id string, cmd []string) (*ExecResult, error) {
	args := append([]string{"exec", id, "--"}, cmd...)
	stdout, stderr, exitCode, err := l.executor.ExecuteWithStderr(ctx, l.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("lxc exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (l *LXDRuntime) Logs(ctx context.Context, id string, opts ...LogOption) (io.ReadCloser, error) {
	o := applyLogOptions(opts)
	args := []string{"logs", id}
	if o.Tail != "" {
		if n, err := strconv.Atoi(o.Tail); err == nil {
			args = append(args, "--tail", strconv.Itoa(n))
		}
	}

	rc, err := l.executor.ExecuteStream(ctx, l.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("lxc logs %s: %w", id, err)
	}
	return rc, nil
}

// Run reports ErrRunUnsupported: LXD has no one-shot ephemeral-run form.
//
// `lxc launch` starts a long-lived instance and returns immediately without a
// command, exit code or captured output; getting Run's semantics would mean
// launch → exec → collect → delete, which is a lifecycle this adapter would be
// inventing rather than delegating. The sentinel keeps that visible instead of
// shipping a multi-step emulation that fails differently from every other
// runtime.
func (l *LXDRuntime) Run(
	ctx context.Context, image string, cmd []string, opts ...RunOption,
) (*ExecResult, error) {
	return nil, fmt.Errorf(
		"lxc run %s: %w (lxc launch starts a persistent instance and "+
			"captures no exit code or output)", image, ErrRunUnsupported,
	)
}
