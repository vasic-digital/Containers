package runtime

import (
	"context"
	"fmt"
	"sync"
)

// RuntimeFactory creates container runtimes. This allows dependency injection
// for testing.
type RuntimeFactory func() []ContainerRuntime

// RuntimePriority defines the order of runtime detection.
// Podman is preferred over Docker for its rootless capabilities.
// Containerd/nerdctl is preferred for Kubernetes-native environments.
//
// Access it through GetRuntimePriority / SetRuntimePriority, which serialise
// reads and writes under priorityMu. AutoDetect consults the current value, so
// SetRuntimePriority genuinely reorders subsequent AutoDetect calls.
var (
	priorityMu      sync.RWMutex
	RuntimePriority = []string{
		"podman",
		"docker",
		"nerdctl",
		"cri-o",
		"lxd",
		"kubernetes",
	}
)

// detectFactory produces the runtime set the public detection path
// (AutoDetect / AutoDetectWithPriority) considers. It defaults to
// defaultRuntimeFactory; tests swap it to inject fakes so the priority-honouring
// path is unit-testable without a real container runtime on the host.
var detectFactory RuntimeFactory = defaultRuntimeFactory

// defaultRuntimeFactory creates the standard set of container runtimes
// in priority order: Podman → Docker → nerdctl → CRI-O → LXD → Kubernetes
func defaultRuntimeFactory() []ContainerRuntime {
	return []ContainerRuntime{
		NewPodmanRuntime(),
		NewDockerRuntime(),
		NewNerdctlRuntime(),
		NewCRIORuntime(),
		NewLXDRuntime(),
		NewKubernetesRuntime(),
	}
}

// autoDetectWith performs auto-detection using the provided runtimes.
func autoDetectWith(
	ctx context.Context,
	runtimes []ContainerRuntime,
) (ContainerRuntime, error) {
	for _, rt := range runtimes {
		if rt.IsAvailable(ctx) {
			return rt, nil
		}
	}
	return nil, fmt.Errorf(
		"no container runtime detected: " +
			"tried podman, docker, nerdctl, cri-o, lxd, kubernetes",
	)
}

// detectAllWith returns all available runtimes from the provided list.
func detectAllWith(
	ctx context.Context,
	runtimes []ContainerRuntime,
) []ContainerRuntime {
	var available []ContainerRuntime
	for _, rt := range runtimes {
		if rt.IsAvailable(ctx) {
			available = append(available, rt)
		}
	}
	return available
}

// AutoDetect tries runtimes in the current RuntimePriority order (default:
// Podman → Docker → nerdctl → CRI-O → LXD → Kubernetes) and returns the first
// available container runtime. It honours SetRuntimePriority — previously it
// ignored RuntimePriority and always used the hardcoded factory order, so
// SetRuntimePriority had no effect on AutoDetect despite documenting that it
// would.
func AutoDetect(ctx context.Context) (ContainerRuntime, error) {
	return AutoDetectWithPriority(ctx, GetRuntimePriority())
}

// AutoDetectWithPriority tries runtimes in the specified priority order.
// If a runtime is not in the priority list, it's tried last.
func AutoDetectWithPriority(ctx context.Context, priority []string) (ContainerRuntime, error) {
	runtimes := detectFactory()

	// Reorder runtimes based on priority
	ordered := make([]ContainerRuntime, 0, len(runtimes))
	seen := make(map[string]bool)

	for _, name := range priority {
		for _, rt := range runtimes {
			if !seen[rt.Name()] && rt.Name() == name {
				ordered = append(ordered, rt)
				seen[rt.Name()] = true
			}
		}
	}

	// Add remaining runtimes
	for _, rt := range runtimes {
		if !seen[rt.Name()] {
			ordered = append(ordered, rt)
			seen[rt.Name()] = true
		}
	}

	return autoDetectWith(ctx, ordered)
}

// DetectAll returns all available container runtimes on the system.
func DetectAll(ctx context.Context) []ContainerRuntime {
	return detectAllWith(ctx, defaultRuntimeFactory())
}

// DetectByPriority returns all available runtimes, sorted by priority.
// The first runtime in the result is the highest priority available.
func DetectByPriority(ctx context.Context, priority []string) []ContainerRuntime {
	runtimes := defaultRuntimeFactory()

	// Reorder based on priority
	ordered := make([]ContainerRuntime, 0, len(runtimes))
	seen := make(map[string]bool)

	for _, name := range priority {
		for _, rt := range runtimes {
			if !seen[rt.Name()] && rt.Name() == name && rt.IsAvailable(ctx) {
				ordered = append(ordered, rt)
				seen[rt.Name()] = true
			}
		}
	}

	// Add remaining available runtimes
	for _, rt := range runtimes {
		if !seen[rt.Name()] && rt.IsAvailable(ctx) {
			ordered = append(ordered, rt)
		}
	}

	return ordered
}

// GetRuntimePriority returns a copy of the current runtime priority list.
func GetRuntimePriority() []string {
	priorityMu.RLock()
	defer priorityMu.RUnlock()
	return append([]string{}, RuntimePriority...)
}

// SetRuntimePriority sets a custom runtime priority order.
// This affects all subsequent AutoDetect calls.
func SetRuntimePriority(priority []string) {
	priorityMu.Lock()
	defer priorityMu.Unlock()
	RuntimePriority = append([]string{}, priority...)
}
