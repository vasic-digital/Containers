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
//
// RT-RACE-1 (§11.4.6 honest documentation, flagged not fixed): reading or
// assigning this exported var DIRECTLY (`runtime.RuntimePriority`) bypasses
// priorityMu and is a genuine data race against concurrent
// GetRuntimePriority/SetRuntimePriority calls from another goroutine — `go
// test -race` on a synthetic direct-read-during-SetRuntimePriority scenario
// reproduces it. There is no non-breaking fix: Go cannot make a plain
// exported `[]string` package var itself race-safe against direct access
// without either changing its type (e.g. to an atomic.Pointer[[]string],
// which breaks every existing `for _, x := range runtime.RuntimePriority`-
// style caller) or unexporting it (RuntimePriority -> runtimePriority, which
// removes a public symbol outright, §11.4.122). Both are breaking public-API
// changes and are NOT applied here unilaterally; this is an operator
// decision item. Recommendation: unexport to `runtimePriority` and keep
// Get/SetRuntimePriority as the sole accessors, in a deliberate, announced
// API-break window (major-version bump / CHANGELOG entry), NOT a silent
// patch-level edit.
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
