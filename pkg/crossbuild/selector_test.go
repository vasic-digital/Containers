package crossbuild

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend lets tests assert which Backend the Selector picks
// without invoking real Build logic.
type fakeBackend struct {
	name   string
	caps   Capabilities
	called bool
	gotReq BuildRequest
	result BuildResult
}

func (f *fakeBackend) Name() string               { return f.name }
func (f *fakeBackend) Capabilities() Capabilities { return f.caps }
func (f *fakeBackend) Build(ctx context.Context, req BuildRequest) BuildResult {
	f.called = true
	f.gotReq = req
	return f.result
}

// TestSelector_ChoosesFirstMatchingBackend verifies that when two
// backends both support the target, the FIRST registered wins. This
// is the load-bearing rule that lets host-direct take precedence
// over container/QEMU paths.
func TestSelector_ChoosesFirstMatchingBackend(t *testing.T) {
	s := NewSelectorForHost("linux", "amd64")
	first := &fakeBackend{
		name: "first",
		caps: Capabilities{SupportsTargets: []Target{{OS: "linux", Arch: "amd64"}}},
	}
	second := &fakeBackend{
		name: "second",
		caps: Capabilities{SupportsTargets: []Target{{OS: "linux", Arch: "amd64"}}},
	}
	s.Register(first)
	s.Register(second)

	chosen, err := s.Choose(BuildRequest{Target: Target{OS: "linux", Arch: "amd64"}})
	require.NoError(t, err)
	assert.Equal(t, "first", chosen.Name(),
		"Selector MUST prefer the first registered backend that supports the target")
	assert.False(t, second.called, "second backend must not be called")
}

// TestSelector_SkipsBackendsRequiringDifferentHost verifies the
// RequiresHostOS filter: a wine-container backend declared
// RequiresHostOS:[linux] must NOT be chosen on a darwin host.
func TestSelector_SkipsBackendsRequiringDifferentHost(t *testing.T) {
	wine := &fakeBackend{
		name: "wine-container",
		caps: Capabilities{
			SupportsTargets: []Target{{OS: "windows", Arch: "amd64"}},
			RequiresHostOS:  []string{"linux"},
		},
	}
	s := NewSelectorForHost("darwin", "arm64")
	s.Register(wine)

	_, err := s.Choose(BuildRequest{Target: Target{OS: "windows", Arch: "amd64"}})
	require.Error(t, err,
		"a backend declaring RequiresHostOS=[linux] must NOT be chosen on darwin")
	assert.Contains(t, err.Error(), "no backend registered supports target")
}

// TestSelector_HostNativeRoutesToHostDirect verifies that when a
// real HostDirectBackend is registered, the Selector routes a host-
// native target to it. This is the operational happy path.
func TestSelector_HostNativeRoutesToHostDirect(t *testing.T) {
	s := NewSelectorForHost(runtime.GOOS, runtime.GOARCH)
	hd := NewHostDirectBackend()
	s.Register(hd)

	chosen, err := s.Choose(BuildRequest{
		Target: Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
	})
	require.NoError(t, err)
	assert.Equal(t, "host-direct", chosen.Name())
}

// TestSelector_RejectsEmptyTarget verifies the input-validation
// path. A consumer that forgets to set Target.OS gets a clear error,
// not a silent fallback.
func TestSelector_RejectsEmptyTarget(t *testing.T) {
	s := NewSelectorForHost("linux", "amd64")
	s.Register(NewHostDirectBackend())

	_, err := s.Choose(BuildRequest{Target: Target{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Target.OS + .Arch are required")
}

// TestSelector_SupportedTargetsDedupesAndSorts verifies the
// diagnostic listing collapses duplicate target tuples + sorts.
func TestSelector_SupportedTargetsDedupesAndSorts(t *testing.T) {
	s := NewSelectorForHost("linux", "amd64")
	s.Register(&fakeBackend{name: "a", caps: Capabilities{SupportsTargets: []Target{
		{OS: "linux", Arch: "amd64"},
		{OS: "darwin", Arch: "amd64"},
	}}})
	s.Register(&fakeBackend{name: "b", caps: Capabilities{SupportsTargets: []Target{
		{OS: "linux", Arch: "amd64"}, // duplicate
		{OS: "windows", Arch: "amd64"},
	}}})

	targets := s.SupportedTargets()
	assert.Equal(t, []Target{
		{OS: "darwin", Arch: "amd64"},
		{OS: "linux", Arch: "amd64"},
		{OS: "windows", Arch: "amd64"},
	}, targets, "SupportedTargets must dedupe + sort")
}

// TestTarget_IsHostNative covers the helper that short-circuits the
// host-direct decision. Positive + negative cases both exercised.
func TestTarget_IsHostNative(t *testing.T) {
	t1 := Target{OS: "linux", Arch: "amd64"}
	assert.True(t, t1.IsHostNative("linux", "amd64"))
	assert.False(t, t1.IsHostNative("darwin", "amd64"))
	assert.False(t, t1.IsHostNative("linux", "arm64"))
}
