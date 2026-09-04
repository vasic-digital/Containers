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

// KubernetesRuntime implements ContainerRuntime using kubectl,
// mapping Kubernetes pods to the container abstraction.
type KubernetesRuntime struct {
	binary    string
	namespace string
	executor  CommandExecutor
}

// NewKubernetesRuntime creates a KubernetesRuntime using the default
// kubectl binary and namespace.
func NewKubernetesRuntime() *KubernetesRuntime {
	return &KubernetesRuntime{
		binary:    "kubectl",
		namespace: "default",
		executor:  &defaultExecutor{},
	}
}

// NewKubernetesRuntimeWithExecutor creates a KubernetesRuntime with a
// custom executor and namespace, primarily for testing.
func NewKubernetesRuntimeWithExecutor(
	exec CommandExecutor, namespace string,
) *KubernetesRuntime {
	ns := namespace
	if ns == "" {
		ns = "default"
	}
	return &KubernetesRuntime{
		binary:    "kubectl",
		namespace: ns,
		executor:  exec,
	}
}

func (k *KubernetesRuntime) Name() string {
	return "kubernetes"
}

func (k *KubernetesRuntime) Version(
	ctx context.Context,
) (string, error) {
	out, err := k.executor.Execute(
		ctx, k.binary, "version", "--output=json",
	)
	if err != nil {
		return "", fmt.Errorf("kubectl version: %w", err)
	}
	var ver struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal(out, &ver); err != nil {
		return "", fmt.Errorf("parsing kubectl version: %w", err)
	}
	return ver.ServerVersion.GitVersion, nil
}

func (k *KubernetesRuntime) IsAvailable(ctx context.Context) bool {
	_, err := k.executor.Execute(
		ctx, k.binary, "cluster-info",
	)
	return err == nil
}

func (k *KubernetesRuntime) Start(
	ctx context.Context, id string, opts ...StartOption,
) error {
	// Kubernetes does not have a direct "start" for pods.
	// Scale the associated deployment to 1 replica as an approximation.
	_ = applyStartOptions(opts)
	args := []string{
		"scale", "--replicas=1",
		fmt.Sprintf("deployment/%s", id),
		"-n", k.namespace,
	}
	_, err := k.executor.Execute(ctx, k.binary, args...)
	if err != nil {
		return fmt.Errorf("kubectl scale (start) %s: %w", id, err)
	}
	return nil
}

func (k *KubernetesRuntime) Stop(
	ctx context.Context, id string, opts ...StopOption,
) error {
	_ = applyStopOptions(opts)
	args := []string{
		"scale", "--replicas=0",
		fmt.Sprintf("deployment/%s", id),
		"-n", k.namespace,
	}
	_, err := k.executor.Execute(ctx, k.binary, args...)
	if err != nil {
		return fmt.Errorf("kubectl scale (stop) %s: %w", id, err)
	}
	return nil
}

func (k *KubernetesRuntime) Remove(
	ctx context.Context, id string, opts ...RemoveOption,
) error {
	o := applyRemoveOptions(opts)
	args := []string{"delete", "pod", id, "-n", k.namespace}
	if o.Force {
		args = append(args, "--force", "--grace-period=0")
	}
	_, err := k.executor.Execute(ctx, k.binary, args...)
	if err != nil {
		return fmt.Errorf("kubectl delete pod %s: %w", id, err)
	}
	return nil
}

func (k *KubernetesRuntime) Status(
	ctx context.Context, id string,
) (*ContainerStatus, error) {
	out, err := k.executor.Execute(
		ctx, k.binary,
		"get", "pod", id,
		"-n", k.namespace,
		"-o", "json",
	)
	if err != nil {
		return nil, fmt.Errorf("kubectl get pod %s: %w", id, err)
	}
	return parseKubePodStatus(out)
}

// kubePodJSON models relevant fields from kubectl get pod -o json.
type kubePodJSON struct {
	Metadata struct {
		Name      string            `json:"name"`
		UID       string            `json:"uid"`
		Labels    map[string]string `json:"labels"`
		Namespace string            `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
			Ports []struct {
				ContainerPort int    `json:"containerPort"`
				Protocol      string `json:"protocol"`
			} `json:"ports"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		StartTime         string `json:"startTime"`
		ContainerStatuses []struct {
			ContainerID string `json:"containerID"`
			State       struct {
				Running *struct {
					StartedAt string `json:"startedAt"`
				} `json:"running"`
				Terminated *struct {
					ExitCode   int    `json:"exitCode"`
					FinishedAt string `json:"finishedAt"`
				} `json:"terminated"`
			} `json:"state"`
			Ready bool `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func parseKubePodStatus(data []byte) (*ContainerStatus, error) {
	var pod kubePodJSON
	if err := json.Unmarshal(data, &pod); err != nil {
		return nil, fmt.Errorf("parsing pod JSON: %w", err)
	}

	state := mapKubePhaseToState(pod.Status.Phase)
	health := pod.Status.Phase

	var startedAt, finishedAt time.Time
	exitCode := 0

	if len(pod.Status.ContainerStatuses) > 0 {
		cs := pod.Status.ContainerStatuses[0]
		if cs.State.Running != nil {
			startedAt, _ = time.Parse(
				time.RFC3339, cs.State.Running.StartedAt,
			)
		}
		if cs.State.Terminated != nil {
			exitCode = cs.State.Terminated.ExitCode
			finishedAt, _ = time.Parse(
				time.RFC3339, cs.State.Terminated.FinishedAt,
			)
			state = StateStopped
		}
		if cs.Ready {
			health = "healthy"
		}
	}

	var ports []PortMapping
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			ports = append(ports, PortMapping{
				ContainerPort: strconv.Itoa(p.ContainerPort),
				Protocol:      strings.ToLower(p.Protocol),
			})
		}
	}

	return &ContainerStatus{
		ID:         pod.Metadata.UID,
		Name:       pod.Metadata.Name,
		State:      state,
		Health:     health,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		ExitCode:   exitCode,
		Ports:      ports,
	}, nil
}

func mapKubePhaseToState(phase string) ContainerState {
	switch strings.ToLower(phase) {
	case "running":
		return StateRunning
	case "succeeded", "failed":
		return StateStopped
	case "pending":
		return StateCreated
	default:
		return ContainerState(phase)
	}
}

func (k *KubernetesRuntime) List(
	ctx context.Context, filter ListFilter,
) ([]ContainerInfo, error) {
	args := []string{
		"get", "pods", "-n", k.namespace, "-o", "json",
	}

	// Build label selector from filter labels.
	var selectors []string
	for key, val := range filter.Labels {
		selectors = append(selectors,
			fmt.Sprintf("%s=%s", key, val),
		)
	}
	if len(selectors) > 0 {
		args = append(args, "-l", strings.Join(selectors, ","))
	}

	out, err := k.executor.Execute(ctx, k.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods: %w", err)
	}

	var podList struct {
		Items []kubePodJSON `json:"items"`
	}
	if err := json.Unmarshal(out, &podList); err != nil {
		return nil, fmt.Errorf("parsing pod list: %w", err)
	}

	var containers []ContainerInfo
	for _, pod := range podList.Items {
		state := mapKubePhaseToState(pod.Status.Phase)

		// Apply name filter.
		if len(filter.Names) > 0 {
			matched := false
			for _, name := range filter.Names {
				if strings.Contains(pod.Metadata.Name, name) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// Apply status filter.
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

		image := ""
		if len(pod.Spec.Containers) > 0 {
			image = pod.Spec.Containers[0].Image
		}

		created, _ := time.Parse(
			time.RFC3339, pod.Status.StartTime,
		)

		containers = append(containers, ContainerInfo{
			ID:      pod.Metadata.UID,
			Name:    pod.Metadata.Name,
			Image:   image,
			State:   state,
			Status:  pod.Status.Phase,
			Created: created,
			Labels:  pod.Metadata.Labels,
		})
	}

	return containers, nil
}

func (k *KubernetesRuntime) Stats(
	ctx context.Context, id string,
) (*ContainerStats, error) {
	out, err := k.executor.Execute(
		ctx, k.binary,
		"top", "pod", id,
		"-n", k.namespace,
		"--no-headers",
	)
	if err != nil {
		return nil, fmt.Errorf("kubectl top pod %s: %w", id, err)
	}
	return parseKubeTopOutput(out)
}

func parseKubeTopOutput(data []byte) (*ContainerStats, error) {
	line := strings.TrimSpace(string(data))
	if line == "" {
		return nil, fmt.Errorf("empty top output")
	}
	// Take first line only.
	if idx := strings.Index(line, "\n"); idx >= 0 {
		line = line[:idx]
	}
	// Format: NAME CPU(cores) MEMORY(bytes)
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return nil, fmt.Errorf(
			"unexpected top output format: %s", line,
		)
	}

	cpuStr := fields[1]
	memStr := fields[2]

	cpuMillis := parseKubeCPU(cpuStr)
	memBytes := parseSizeToBytes(memStr)

	return &ContainerStats{
		CPUPercent:  float64(cpuMillis) / 10.0,
		MemoryUsage: memBytes,
	}, nil
}

func parseKubeCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		v, _ := strconv.ParseInt(
			strings.TrimSuffix(s, "m"), 10, 64,
		)
		return v
	}
	// Whole cores.
	v, _ := strconv.ParseInt(s, 10, 64)
	return v * 1000
}

func (k *KubernetesRuntime) Exec(
	ctx context.Context, id string, cmd []string,
) (*ExecResult, error) {
	args := append(
		[]string{"exec", id, "-n", k.namespace, "--"},
		cmd...,
	)
	stdout, stderr, exitCode, err := k.executor.ExecuteWithStderr(
		ctx, k.binary, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("kubectl exec %s: %w", id, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}

func (k *KubernetesRuntime) Logs(
	ctx context.Context, id string, opts ...LogOption,
) (io.ReadCloser, error) {
	o := applyLogOptions(opts)
	args := []string{"logs", id, "-n", k.namespace}
	if o.Follow {
		args = append(args, "-f")
	}
	if o.Since != "" {
		args = append(args, "--since", o.Since)
	}
	if o.Tail != "" && o.Tail != "all" {
		args = append(args, "--tail", o.Tail)
	}

	rc, err := k.executor.ExecuteStream(ctx, k.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("kubectl logs %s: %w", id, err)
	}
	return rc, nil
}

// Run reports ErrRunUnsupported.
//
// `kubectl run --rm -i --restart=Never` is a real one-shot form, and this is
// the one place where the honest answer is "not verified rather than not
// possible" (§11.4.6). It is NOT implemented here because it differs from
// Run's contract in ways that cannot be papered over blind: it requires a
// caller-supplied pod NAME as a positional argument (Run takes only an image),
// it interleaves pod-lifecycle messages such as `pod "x" deleted` into the
// captured stdout, and its exit code reports kubectl's own status rather than
// the container's in several documented cases. No cluster is reachable from
// this repository, so none of that could be measured. Shipping an unverified
// implementation whose output silently mixes kubectl chatter with the
// container's own bytes is the same silent-failure class this package has just
// been repaired for; the sentinel says so out loud instead.
func (k *KubernetesRuntime) Run(
	ctx context.Context, image string, cmd []string, opts ...RunOption,
) (*ExecResult, error) {
	return nil, fmt.Errorf(
		"kubectl run %s: %w (kubectl run needs an explicit pod name and "+
			"mixes pod-lifecycle output into stdout; not implemented "+
			"unverified)", image, ErrRunUnsupported,
	)
}
