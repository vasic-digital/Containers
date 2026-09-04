package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// NerdctlRuntime implements ContainerRuntime using the nerdctl CLI.
// nerdctl is Docker-compatible CLI for containerd, supporting all
// standard Docker commands with additional containerd-specific features.
type NerdctlRuntime struct {
	binary   string
	executor CommandExecutor
}

// NewNerdctlRuntime creates a NerdctlRuntime with the real nerdctl binary.
func NewNerdctlRuntime() *NerdctlRuntime {
	return &NerdctlRuntime{
		binary:   "nerdctl",
		executor: &defaultExecutor{},
	}
}

// NewNerdctlRuntimeWithExecutor creates a NerdctlRuntime with a custom
// executor, primarily for testing.
func NewNerdctlRuntimeWithExecutor(exec CommandExecutor) *NerdctlRuntime {
	return &NerdctlRuntime{
		binary:   "nerdctl",
		executor: exec,
	}
}

func (n *NerdctlRuntime) Name() string {
	return "nerdctl"
}

func (n *NerdctlRuntime) Version(ctx context.Context) (string, error) {
	out, err := n.executor.Execute(ctx, n.binary, "version", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("nerdctl version: %w", err)
	}
	return parseNerdctlVersion(out)
}

type nerdctlVersionJSON struct {
	Client struct {
		Version string `json:"Version"`
	} `json:"Client"`
	Server struct {
		Version string `json:"Version"`
	} `json:"Server"`
}

func parseNerdctlVersion(data []byte) (string, error) {
	var v nerdctlVersionJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return strings.TrimSpace(string(data)), nil
	}
	if v.Server.Version != "" {
		return v.Server.Version, nil
	}
	return v.Client.Version, nil
}

func (n *NerdctlRuntime) IsAvailable(ctx context.Context) bool {
	_, err := n.executor.Execute(ctx, n.binary, "info")
	return err == nil
}

func (n *NerdctlRuntime) Start(ctx context.Context, id string, opts ...StartOption) error {
	_ = applyStartOptions(opts)
	args := []string{"start", id}
	_, err := n.executor.Execute(ctx, n.binary, args...)
	if err != nil {
		return fmt.Errorf("nerdctl start %s: %w", id, err)
	}
	return nil
}

func (n *NerdctlRuntime) Stop(ctx context.Context, id string, opts ...StopOption) error {
	o := applyStopOptions(opts)
	timeoutSec := int(o.Timeout.Seconds())
	args := []string{"stop", "-t", strconv.Itoa(timeoutSec), id}
	_, err := n.executor.Execute(ctx, n.binary, args...)
	if err != nil {
		return fmt.Errorf("nerdctl stop %s: %w", id, err)
	}
	return nil
}

func (n *NerdctlRuntime) Remove(ctx context.Context, id string, opts ...RemoveOption) error {
	o := applyRemoveOptions(opts)
	args := []string{"rm"}
	if o.Force {
		args = append(args, "-f")
	}
	if o.Volumes {
		args = append(args, "-v")
	}
	args = append(args, id)
	_, err := n.executor.Execute(ctx, n.binary, args...)
	if err != nil {
		return fmt.Errorf("nerdctl rm %s: %w", id, err)
	}
	return nil
}

func (n *NerdctlRuntime) Status(ctx context.Context, id string) (*ContainerStatus, error) {
	out, err := n.executor.Execute(ctx, n.binary, "inspect", "--format", "json", id)
	if err != nil {
		return nil, fmt.Errorf("nerdctl inspect %s: %w", id, err)
	}
	return parseDockerInspectStatus(out)
}

func (n *NerdctlRuntime) List(ctx context.Context, filter ListFilter) ([]ContainerInfo, error) {
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

	out, err := n.executor.Execute(ctx, n.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("nerdctl ps: %w", err)
	}
	return parseDockerPSOutput(out)
}

func (n *NerdctlRuntime) Stats(ctx context.Context, id string) (*ContainerStats, error) {
	out, err := n.executor.Execute(ctx, n.binary, "stats", "--no-stream", "--format", "json", id)
	if err != nil {
		return nil, fmt.Errorf("nerdctl stats %s: %w", id, err)
	}
	return parseDockerStats(out)
}

func (n *NerdctlRuntime) Exec(ctx context.Context, id string, cmd []string) (*ExecResult, error) {
	args := append([]string{"exec", id}, cmd...)
	stdout, stderr, exitCode, err := n.executor.ExecuteWithStderr(ctx, n.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("nerdctl exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (n *NerdctlRuntime) Logs(ctx context.Context, id string, opts ...LogOption) (io.ReadCloser, error) {
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

	rc, err := n.executor.ExecuteStream(ctx, n.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("nerdctl logs %s: %w", id, err)
	}
	return rc, nil
}

// Run performs a one-shot `nerdctl run` — see ContainerRuntime.Run.
func (n *NerdctlRuntime) Run(
	ctx context.Context, image string, cmd []string, opts ...RunOption,
) (*ExecResult, error) {
	return runViaCLI(ctx, n.executor, n.binary, "nerdctl", image, cmd, opts)
}
