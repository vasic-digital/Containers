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

// CRIORuntime implements ContainerRuntime using the crictl CLI.
// CRI-O is a lightweight container runtime for Kubernetes, implementing
// the Kubernetes Container Runtime Interface (CRI).
type CRIORuntime struct {
	binary   string
	executor CommandExecutor
}

// NewCRIORuntime creates a CRIORuntime with the real crictl binary.
func NewCRIORuntime() *CRIORuntime {
	return &CRIORuntime{
		binary:   "crictl",
		executor: &defaultExecutor{},
	}
}

// NewCRIORuntimeWithExecutor creates a CRIORuntime with a custom executor.
func NewCRIORuntimeWithExecutor(exec CommandExecutor) *CRIORuntime {
	return &CRIORuntime{
		binary:   "crictl",
		executor: exec,
	}
}

func (c *CRIORuntime) Name() string {
	return "cri-o"
}

func (c *CRIORuntime) Version(ctx context.Context) (string, error) {
	out, err := c.executor.Execute(ctx, c.binary, "version", "--output", "json")
	if err != nil {
		return "", fmt.Errorf("crictl version: %w", err)
	}
	return parseCrioVersion(out)
}

type crioVersionJSON struct {
	RuntimeName    string `json:"runtimeName"`
	RuntimeVersion string `json:"runtimeVersion"`
}

func parseCrioVersion(data []byte) (string, error) {
	var v crioVersionJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return strings.TrimSpace(string(data)), nil
	}
	return v.RuntimeVersion, nil
}

func (c *CRIORuntime) IsAvailable(ctx context.Context) bool {
	_, err := c.executor.Execute(ctx, c.binary, "info")
	return err == nil
}

func (c *CRIORuntime) Start(ctx context.Context, id string, opts ...StartOption) error {
	_ = applyStartOptions(opts)
	_, err := c.executor.Execute(ctx, c.binary, "start", id)
	if err != nil {
		return fmt.Errorf("crictl start %s: %w", id, err)
	}
	return nil
}

func (c *CRIORuntime) Stop(ctx context.Context, id string, opts ...StopOption) error {
	o := applyStopOptions(opts)
	timeoutSec := int(o.Timeout.Seconds())
	args := []string{"stop", "-t", strconv.Itoa(timeoutSec), id}
	_, err := c.executor.Execute(ctx, c.binary, args...)
	if err != nil {
		return fmt.Errorf("crictl stop %s: %w", id, err)
	}
	return nil
}

func (c *CRIORuntime) Remove(ctx context.Context, id string, opts ...RemoveOption) error {
	o := applyRemoveOptions(opts)
	args := []string{"rm"}
	if o.Force {
		args = append(args, "-f")
	}
	args = append(args, id)
	_, err := c.executor.Execute(ctx, c.binary, args...)
	if err != nil {
		return fmt.Errorf("crictl rm %s: %w", id, err)
	}
	return nil
}

func (c *CRIORuntime) Status(ctx context.Context, id string) (*ContainerStatus, error) {
	out, err := c.executor.Execute(ctx, c.binary, "inspect", id, "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("crictl inspect %s: %w", id, err)
	}
	return parseCrioInspect(out)
}

type crioContainerInfo struct {
	Status struct {
		State     string `json:"state"`
		StartedAt string `json:"startedAt"`
	} `json:"status"`
	Info struct {
		RuntimeSpec struct {
			Process struct {
				Args []string `json:"args"`
			} `json:"process"`
		} `json:"runtimeSpec"`
	} `json:"info"`
}

func parseCrioInspect(data []byte) (*ContainerStatus, error) {
	var info crioContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parsing crictl inspect output: %w", err)
	}

	state := mapCrioState(info.Status.State)
	startedAt, _ := time.Parse(time.RFC3339, info.Status.StartedAt)

	return &ContainerStatus{
		State:     state,
		Health:    info.Status.State,
		StartedAt: startedAt,
	}, nil
}

func mapCrioState(s string) ContainerState {
	switch strings.ToLower(s) {
	case "running":
		return StateRunning
	case "stopped", "exited":
		return StateStopped
	case "paused":
		return StatePaused
	case "created":
		return StateCreated
	default:
		return ContainerState(s)
	}
}

func (c *CRIORuntime) List(ctx context.Context, filter ListFilter) ([]ContainerInfo, error) {
	args := []string{"pods", "--output", "json"}
	if filter.All {
		args = append(args, "-a")
	}

	out, err := c.executor.Execute(ctx, c.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("crictl pods: %w", err)
	}
	return parseCrioPods(out, filter)
}

type crioPod struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels"`
	CreatedAt int64             `json:"createdAt"`
}

// parseCrioPods converts crictl pod JSON into ContainerInfo, applying the
// caller's ListFilter. Previously it ignored filter.Names / filter.Labels /
// filter.Status entirely and returned EVERY pod, so a caller asking for one
// service by name got the whole cluster's pods back — the sibling LXD/Docker
// runtimes honour these filters. Name matching is substring (like parseLXDList)
// and label matching requires every filter label to be present with an equal
// value.
func parseCrioPods(data []byte, filter ListFilter) ([]ContainerInfo, error) {
	var pods []crioPod
	if err := json.Unmarshal(data, &pods); err != nil {
		return nil, fmt.Errorf("parsing crictl pods output: %w", err)
	}

	var result []ContainerInfo
	for _, pod := range pods {
		state := mapCrioState(pod.State)

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
				if strings.Contains(pod.Name, name) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		if len(filter.Labels) > 0 {
			matched := true
			for k, v := range filter.Labels {
				if pod.Labels[k] != v {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
		}

		result = append(result, ContainerInfo{
			ID:      pod.ID,
			Name:    pod.Name,
			State:   state,
			Status:  pod.State,
			Labels:  pod.Labels,
			Created: time.Unix(0, pod.CreatedAt),
		})
	}
	return result, nil
}

func (c *CRIORuntime) Stats(ctx context.Context, id string) (*ContainerStats, error) {
	args := []string{"stats", id, "--output", "json"}
	out, err := c.executor.Execute(ctx, c.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("crictl stats %s: %w", id, err)
	}
	return parseCrioStats(out)
}

type crioStats struct {
	Stats []struct {
		CPU    string `json:"cpu"`
		Memory struct {
			WorkingSetBytes string `json:"workingSetBytes"`
		} `json:"memory"`
	} `json:"stats"`
}

func parseCrioStats(data []byte) (*ContainerStats, error) {
	var stats crioStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return &ContainerStats{}, nil
	}
	if len(stats.Stats) == 0 {
		return &ContainerStats{}, nil
	}
	mem, _ := strconv.ParseUint(stats.Stats[0].Memory.WorkingSetBytes, 10, 64)
	return &ContainerStats{
		MemoryUsage: mem,
	}, nil
}

func (c *CRIORuntime) Exec(ctx context.Context, id string, cmd []string) (*ExecResult, error) {
	args := append([]string{"exec", id}, cmd...)
	stdout, stderr, exitCode, err := c.executor.ExecuteWithStderr(ctx, c.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("crictl exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (c *CRIORuntime) Logs(ctx context.Context, id string, opts ...LogOption) (io.ReadCloser, error) {
	o := applyLogOptions(opts)
	args := []string{"logs"}
	if o.Follow {
		args = append(args, "-f")
	}
	if o.Tail != "" {
		args = append(args, "--tail", o.Tail)
	}
	args = append(args, id)

	rc, err := c.executor.ExecuteStream(ctx, c.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("crictl logs %s: %w", id, err)
	}
	return rc, nil
}
