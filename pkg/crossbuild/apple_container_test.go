package crossbuild

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAppleRunner is the injected seam for unit tests: it records the
// spec it was handed and the argv that buildRunArgs would produce,
// without ever invoking a live engine. Fakes are permitted ONLY here
// (unit test) per the no-fakes-beyond-unit-tests mandate.
type fakeAppleRunner struct {
	available       bool
	imageExists     bool
	exitCode        int
	errOnRun        error
	stdout          string
	stderr          string
	producesPath    string
	producesContent []byte
	gotSpec         appleContainerRunSpec
	gotArgs         []string
	runCalled       bool
}

func (f *fakeAppleRunner) Available() bool { return f.available }

func (f *fakeAppleRunner) ImageExists(_ context.Context, _ string) bool { return f.imageExists }

func (f *fakeAppleRunner) Run(_ context.Context, spec appleContainerRunSpec) (int, error) {
	f.runCalled = true
	f.gotSpec = spec
	f.gotArgs = buildRunArgs(spec, "virtiofs")
	if spec.Stdout != nil {
		spec.Stdout.WriteString(f.stdout)
	}
	if spec.Stderr != nil {
		spec.Stderr.WriteString(f.stderr)
	}
	if f.producesPath != "" {
		if err := os.MkdirAll(filepath.Dir(f.producesPath), 0o755); err != nil {
			return -1, err
		}
		if err := os.WriteFile(f.producesPath, f.producesContent, 0o644); err != nil {
			return -1, err
		}
	}
	if f.errOnRun != nil {
		return f.exitCode, f.errOnRun
	}
	return f.exitCode, nil
}

// TestAppleContainer_BuildRunArgs_VirtiofsMount asserts the EXACT argv
// for the explicit virtiofs mount form — the unforgeable contract a
// live `container run` is invoked with.
func TestAppleContainer_BuildRunArgs_VirtiofsMount(t *testing.T) {
	spec := appleContainerRunSpec{
		Image:       "docker.io/library/alpine:latest",
		Name:        "cb-job",
		MountSource: "/Users/dev/src",
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     "uname -s -m",
	}
	args := buildRunArgs(spec, "virtiofs")
	assert.Equal(t, []string{
		"run", "--rm",
		"--name", "cb-job",
		"--mount", "type=virtiofs,source=/Users/dev/src,target=/work/src",
		"-w", "/work/src",
		"--", // XBUILD2-2: end-of-options guard before the image positional
		"docker.io/library/alpine:latest", "sh", "-c", "uname -s -m",
	}, args)
}

// TestAppleContainer_BuildRunArgs_VolumeFallback asserts the short
// `-v src:dst` fallback form used when virtiofs is rejected.
func TestAppleContainer_BuildRunArgs_VolumeFallback(t *testing.T) {
	spec := appleContainerRunSpec{
		Image:       "alpine",
		MountSource: "/host/dir",
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     "ls",
	}
	args := buildRunArgs(spec, "volume")
	assert.Equal(t, []string{
		"run", "--rm",
		"-v", "/host/dir:/work/src",
		"-w", "/work/src",
		"--", // XBUILD2-2: end-of-options guard before the image positional
		"alpine", "sh", "-c", "ls",
	}, args)
}

// TestAppleContainer_BuildRunArgs_EnvDeterministic asserts env vars are
// emitted as sorted -e flags so argv is stable + testable.
func TestAppleContainer_BuildRunArgs_EnvDeterministic(t *testing.T) {
	spec := appleContainerRunSpec{
		Image:       "alpine",
		MountSource: "/s",
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     "env",
		Environment: map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"},
	}
	args := buildRunArgs(spec, "virtiofs")
	// -e flags appear sorted by key between -w and the image.
	assert.Equal(t, []string{
		"run", "--rm",
		"--mount", "type=virtiofs,source=/s,target=/work/src",
		"-w", "/work/src",
		"-e", "ALPHA=2",
		"-e", "MID=3",
		"-e", "ZED=1",
		"--", // XBUILD2-2: end-of-options guard before the image positional
		"alpine", "sh", "-c", "env",
	}, args)
}

// TestAppleContainer_CapabilitiesHonest locks the honest capability
// advertisement: macOS-only host, linux/arm64 target only.
func TestAppleContainer_CapabilitiesHonest(t *testing.T) {
	a := NewAppleContainerBackend("")
	caps := a.Capabilities()
	assert.Equal(t, []string{"darwin"}, caps.RequiresHostOS,
		"apple-container MUST advertise RequiresHostOS=[darwin] (macOS-only)")
	assert.Equal(t, []Target{{OS: "linux", Arch: "arm64"}}, caps.SupportsTargets,
		"apple-container MUST advertise linux/arm64 only — NOT amd64 (Rosetta avoided)")
	assert.True(t, caps.IsolatesEnvironment)
	assert.Equal(t, "apple-container", a.Name())
}

// TestAppleContainer_HappyPath drives the artifact Build flow against
// the fake runner and asserts the build token reached the container +
// a non-zero artifact came back.
func TestAppleContainer_HappyPath(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "build/app"

	runner := &fakeAppleRunner{
		available:       true,
		imageExists:     true,
		exitCode:        0,
		producesPath:    filepath.Join(srcDir, subpath),
		producesContent: bytes.Repeat([]byte("E"), 256),
		stdout:          "BUILD OK\n",
	}
	a := newAppleContainerBackendWithRunner("docker.io/library/alpine:latest", runner)

	result := a.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "arm64"},
		SourceDir:     srcDir,
		BuildCommand:  "make app",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
		Timeout:       1 * time.Minute,
	})

	require.NoError(t, result.Error)
	assert.Equal(t, "apple-container", result.BackendName)
	assert.True(t, runner.runCalled)
	assert.Equal(t, int64(256), result.ArtifactSize)
	assert.Contains(t, runner.gotSpec.Command, "make app")
	// The container argv MUST shell out to `container run` with the
	// virtiofs mount of the source dir.
	assert.Equal(t, "run", runner.gotArgs[0])
	assert.Contains(t, runner.gotArgs, "--mount")
	assert.Contains(t, runner.gotArgs,
		"type=virtiofs,source="+srcDir+",target=/work/src")
}

// TestAppleContainer_EngineAbsentHonestError: when `container` is not
// installed, Build returns an honest error pointing at provisioning +
// the podman/docker fallback, and never invokes Run.
func TestAppleContainer_EngineAbsentHonestError(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	runner := &fakeAppleRunner{available: false}
	a := newAppleContainerBackendWithRunner("", runner)

	result := a.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "arm64"},
		SourceDir:     srcDir,
		BuildCommand:  "make",
		OutputSubpath: "out",
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "not found on PATH")
	assert.Contains(t, result.Error.Error(), "apple-container-provisioning")
	assert.False(t, runner.runCalled)
}

// TestAppleContainer_MissingImageHonestError: image not pulled →
// honest error pointing at `container image pull`, no Run.
func TestAppleContainer_MissingImageHonestError(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	runner := &fakeAppleRunner{available: true, imageExists: false}
	a := newAppleContainerBackendWithRunner("docker.io/library/ubuntu:24.04", runner)

	result := a.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "arm64"},
		SourceDir:     srcDir,
		BuildCommand:  "make",
		OutputSubpath: "out",
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "not present on host")
	assert.Contains(t, result.Error.Error(), "container image pull")
	assert.False(t, runner.runCalled)
}

// TestAppleContainer_ZeroByteArtifactIsBluff: anti-bluff invariant.
func TestAppleContainer_ZeroByteArtifactIsBluff(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "build/empty"
	runner := &fakeAppleRunner{
		available:       true,
		imageExists:     true,
		exitCode:        0,
		producesPath:    filepath.Join(srcDir, subpath),
		producesContent: []byte{},
	}
	a := newAppleContainerBackendWithRunner("", runner)

	result := a.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "arm64"},
		SourceDir:     srcDir,
		BuildCommand:  "true",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "zero-byte artifact")
}

// TestAppleContainer_ExitCodePropagation: a non-zero in-container exit
// surfaces as a Build error citing the exit code.
func TestAppleContainer_ExitCodePropagation(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	runner := &fakeAppleRunner{available: true, imageExists: true, exitCode: 42}
	a := newAppleContainerBackendWithRunner("", runner)

	result := a.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "arm64"},
		SourceDir:     srcDir,
		BuildCommand:  "exit 42",
		OutputSubpath: "out",
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "exited 42")
	assert.True(t, runner.runCalled)
}

// --- Generic RunInLinuxContainer primitive tests ---

// TestRunInLinuxContainer_CapturesStdoutAndExit asserts the generic
// runner captures stdout + exit code and builds the right mount argv.
func TestRunInLinuxContainer_CapturesStdoutAndExit(t *testing.T) {
	srcDir := t.TempDir()
	runner := &fakeAppleRunner{
		available: true,
		exitCode:  7,
		stdout:    "Linux aarch64\n",
		stderr:    "warn\n",
	}

	res := runLinuxContainerWithRunner(context.Background(), LinuxRunRequest{
		Image:       "docker.io/library/alpine:latest",
		Command:     "uname -s -m",
		MountSource: srcDir,
	}, runner)

	require.NoError(t, res.Error)
	assert.Equal(t, "Linux aarch64\n", res.Stdout)
	assert.Equal(t, "warn\n", res.Stderr)
	assert.Equal(t, 7, res.ExitCode)
	assert.Equal(t, "apple-container", res.BackendName)
	// Mount defaults applied: MountTarget=/work/src, WorkDir=/work/src.
	assert.Equal(t, "/work/src", runner.gotSpec.MountTarget)
	assert.Equal(t, "/work/src", runner.gotSpec.WorkDir)
	assert.Contains(t, runner.gotArgs,
		"type=virtiofs,source="+srcDir+",target=/work/src")
}

// TestRunInLinuxContainer_NoMount runs against the image filesystem
// (no mount) and asserts no mount flag is emitted, WorkDir defaults /.
func TestRunInLinuxContainer_NoMount(t *testing.T) {
	runner := &fakeAppleRunner{available: true, exitCode: 0, stdout: "ok\n"}
	res := runLinuxContainerWithRunner(context.Background(), LinuxRunRequest{
		Image:   "alpine",
		Command: "echo ok",
	}, runner)

	require.NoError(t, res.Error)
	assert.Equal(t, "ok\n", res.Stdout)
	assert.NotContains(t, runner.gotArgs, "--mount")
	assert.NotContains(t, runner.gotArgs, "-v")
	assert.Equal(t, "/", runner.gotSpec.WorkDir)
}

// TestRunInLinuxContainer_EngineAbsent: honest error when no engine.
func TestRunInLinuxContainer_EngineAbsent(t *testing.T) {
	runner := &fakeAppleRunner{available: false}
	res := runLinuxContainerWithRunner(context.Background(), LinuxRunRequest{
		Image:   "alpine",
		Command: "true",
	}, runner)

	require.Error(t, res.Error)
	assert.Contains(t, res.Error.Error(), "not found on PATH")
	assert.Equal(t, -1, res.ExitCode)
	assert.False(t, runner.runCalled)
}

// TestRunInLinuxContainer_Validation rejects missing image / command /
// relative mount source.
func TestRunInLinuxContainer_Validation(t *testing.T) {
	runner := &fakeAppleRunner{available: true}
	cases := []struct {
		name string
		req  LinuxRunRequest
		want string
	}{
		{"no-image", LinuxRunRequest{Command: "true"}, "Image is required"},
		{"no-command", LinuxRunRequest{Image: "alpine"}, "Command is required"},
		{"relative-mount", LinuxRunRequest{Image: "alpine", Command: "true", MountSource: "rel/path"}, "must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runLinuxContainerWithRunner(context.Background(), tc.req, runner)
			require.Error(t, res.Error)
			assert.Contains(t, res.Error.Error(), tc.want)
			assert.False(t, runner.runCalled)
		})
	}
}

// --- imageListed / splitImageRef unit coverage ---

func TestImageListed(t *testing.T) {
	listing := "NAME    TAG     DIGEST\nalpine  latest  a2d49ea686c2\n"
	assert.True(t, imageListed(listing, "docker.io/library/alpine:latest"))
	assert.True(t, imageListed(listing, "alpine:latest"))
	assert.False(t, imageListed(listing, "ubuntu:24.04"))
	assert.False(t, imageListed(listing, "alpine:3.19"),
		"tag mismatch must not match")
	// No-tag ref matches by repo leaf.
	assert.True(t, imageListed(listing, "alpine"))
}

func TestSplitImageRef(t *testing.T) {
	repo, tag := splitImageRef("docker.io/library/alpine:latest")
	assert.Equal(t, "docker.io/library/alpine", repo)
	assert.Equal(t, "latest", tag)

	repo, tag = splitImageRef("registry:5000/team/img:v1")
	assert.Equal(t, "registry:5000/team/img", repo)
	assert.Equal(t, "v1", tag)

	repo, tag = splitImageRef("alpine")
	assert.Equal(t, "alpine", repo)
	assert.Equal(t, "", tag)

	repo, tag = splitImageRef("img@sha256:abc")
	assert.Equal(t, "img", repo)
	assert.Equal(t, "", tag)
}

func TestMountFlagRejected(t *testing.T) {
	assert.True(t, mountFlagRejected(bytes.NewBufferString("Error: unknown flag --mount")))
	assert.True(t, mountFlagRejected(bytes.NewBufferString("unsupported mount type")))
	assert.False(t, mountFlagRejected(bytes.NewBufferString("make: *** No rule to make target")))
	assert.False(t, mountFlagRejected(nil))
}

// --- Selector routing: apple-container preferred on darwin/arm64 ---

// TestSelector_AppleContainerPreferredOnDarwin proves that when the
// Apple-container backend is registered BEFORE the podman/docker
// LinuxContainerBackend, a darwin host targeting linux/arm64 routes to
// apple-container. On a linux host the same registration routes to the
// podman/docker backend (apple-container's RequiresHostOS=[darwin]
// filters it out) — no existing selection is broken.
func TestSelector_AppleContainerPreferredOnDarwin(t *testing.T) {
	target := BuildRequest{Target: Target{OS: "linux", Arch: "arm64"}}

	t.Run("darwin-host-prefers-apple-container", func(t *testing.T) {
		s := NewSelectorForHost("darwin", "arm64")
		s.Register(NewAppleContainerBackend(""))
		s.Register(NewLinuxContainerBackend("", "arm64"))
		chosen, err := s.Choose(target)
		require.NoError(t, err)
		assert.Equal(t, "apple-container", chosen.Name(),
			"on darwin, apple-container (registered first) MUST win for linux/arm64")
	})

	t.Run("linux-host-falls-back-to-podman-docker", func(t *testing.T) {
		s := NewSelectorForHost("linux", "arm64")
		s.Register(NewAppleContainerBackend(""))
		s.Register(NewLinuxContainerBackend("", "arm64"))
		chosen, err := s.Choose(target)
		require.NoError(t, err)
		assert.Equal(t, "linux-container", chosen.Name(),
			"on linux, apple-container is filtered (RequiresHostOS=[darwin]); podman/docker serves")
	})
}
