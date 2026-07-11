package crossbuild

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
)

// Selector picks a Backend for a given BuildRequest by matching the
// request's Target against each registered Backend's Capabilities.
// It is the single decision point consumers go through; the routing
// rules are documented + testable rather than buried in ad-hoc shell
// scripts.
//
// XB2-4: backends is guarded by mu — Register (append) can race with
// Choose/SupportedTargets (range) when a caller registers backends
// from one goroutine while another concurrently builds/queries. The
// mutex makes every access to backends a genuine happens-before edge
// so `go test -race` stays clean under concurrent use.
type Selector struct {
	hostOS   string
	hostArch string
	mu       sync.RWMutex
	backends []Backend
}

// NewSelector returns a Selector wired to the runtime's host OS/Arch.
// Tests inject a different (hostOS, hostArch) via NewSelectorForHost.
func NewSelector() *Selector {
	return NewSelectorForHost(runtime.GOOS, runtime.GOARCH)
}

// NewSelectorForHost is the test seam — production callers use
// NewSelector(). Lets a test pretend the host is Linux even when the
// CI runs on macOS, exercising the wine-container selection path
// without virtualisation.
func NewSelectorForHost(hostOS, hostArch string) *Selector {
	return &Selector{hostOS: hostOS, hostArch: hostArch}
}

// Register adds a Backend to the selector. Order matters: when two
// backends can both serve a target, the FIRST registered wins. This
// lets host-direct take precedence over container/QEMU paths when
// the target is host-native.
func (s *Selector) Register(b Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backends = append(s.backends, b)
}

// Choose returns the Backend that should service the given request,
// or an error if no registered backend can satisfy the target on the
// current host.
func (s *Selector) Choose(req BuildRequest) (Backend, error) {
	if req.Target.OS == "" || req.Target.Arch == "" {
		return nil, fmt.Errorf("crossbuild: BuildRequest.Target.OS + .Arch are required")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.backends {
		caps := b.Capabilities()
		if !backendSupportsTarget(caps, req.Target) {
			continue
		}
		if !backendSupportsHost(caps, s.hostOS) {
			continue
		}
		return b, nil
	}

	return nil, fmt.Errorf(
		"crossbuild: no backend registered supports target %s/%s on host %s/%s; "+
			"register a Backend whose Capabilities.SupportsTargets includes this tuple",
		req.Target.OS, req.Target.Arch, s.hostOS, s.hostArch,
	)
}

// Build is the convenience wrapper: Choose + Backend.Build in one
// call. Most callers use this; Choose is exposed for diagnostics +
// dry-run.
func (s *Selector) Build(ctx context.Context, req BuildRequest) BuildResult {
	b, err := s.Choose(req)
	if err != nil {
		return BuildResult{Target: req.Target, Error: err}
	}
	return b.Build(ctx, req)
}

// SupportedTargets returns the union of every registered backend's
// SupportsTargets, deduplicated + sorted. Useful for "list what we
// can build" diagnostics + for the Challenge script that drives
// real-stack verification.
func (s *Selector) SupportedTargets() []Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[Target]struct{})
	for _, b := range s.backends {
		for _, t := range b.Capabilities().SupportsTargets {
			seen[t] = struct{}{}
		}
	}
	out := make([]Target, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OS != out[j].OS {
			return out[i].OS < out[j].OS
		}
		return out[i].Arch < out[j].Arch
	})
	return out
}

func backendSupportsTarget(caps Capabilities, target Target) bool {
	for _, t := range caps.SupportsTargets {
		if t == target {
			return true
		}
	}
	return false
}

func backendSupportsHost(caps Capabilities, hostOS string) bool {
	if len(caps.RequiresHostOS) == 0 {
		return true
	}
	for _, h := range caps.RequiresHostOS {
		if h == hostOS {
			return true
		}
	}
	return false
}
