package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// LinuxContainerBackend cross-builds Linux artifacts by executing the
// BuildCommand inside a Linux container that has JDK 17 + Gradle
// pre-installed. Unlike WineContainerBackend, this backend needs no
// Wine translation: the container's userspace IS Linux, so produced
// artifacts (.deb, .rpm, native Linux .jar runtimes, etc.) are
// directly usable.
//
// Use this Backend when:
//
//   - Host is macOS or Windows + you need a Linux .deb / .rpm /
//     Linux jpackage runtime image.
//   - Host is Linux but the package set is wrong (e.g. nezha.local
//     ALT Linux openjdk-21 lacks jmods; ticket
//     #nezha-jdk-jmods-bootstrap in the consuming project's KNOWN_DEFECTS).
//   - The dedicated Linux build host is temporarily unreachable
//     (operator mandate, iter-54 of the consuming project, 2026-05-13:
//     "If nezha.local gets unaccessible … do building using
//     Container or Qemu like the Windows build!").
//
// Architecturally identical to WineContainerBackend (same Selector,
// same containerRunner seam, same anti-bluff post-conditions on the
// produced artifact). The differences from the Wine path:
//
//   1. Target restricts to {linux, amd64} / {linux, arm64}. Wine is
//      not involved.
//   2. RequiresHostOS is empty — Linux containers run on every host
//      OS where rootless podman or docker is present (macOS via
//      Podman Machine on Apple Silicon works; Linux native works;
//      Windows via Docker Desktop or podman-on-WSL works).
//   3. Image reference defaults to a vanilla JDK 17 + Gradle image,
//      not the heavier Wine layer.
type LinuxContainerBackend struct {
	imageRef string
	arch     string // "amd64" or "arm64" — picks which image tag to pull
	runner   containerRunner
}

// NewLinuxContainerBackend returns the production backend. imageRef
// defaults to "ghcr.io/vasic-digital/crossbuild-linux:jdk17-<arch>"
// when empty. arch defaults to runtime.GOARCH (so on Apple-Silicon
// hosts the default tag is "arm64" and matches Podman Machine's
// native arch).
func NewLinuxContainerBackend(imageRef, arch string) *LinuxContainerBackend {
	if arch == "" {
		arch = runtime.GOARCH
	}
	if imageRef == "" {
		imageRef = fmt.Sprintf("ghcr.io/vasic-digital/crossbuild-linux:jdk17-%s", arch)
	}
	return &LinuxContainerBackend{
		imageRef: imageRef,
		arch:     arch,
		runner:   realContainerRunner{},
	}
}

// newLinuxContainerBackendWithRunner is the test seam.
func newLinuxContainerBackendWithRunner(imageRef, arch string, r containerRunner) *LinuxContainerBackend {
	if arch == "" {
		arch = runtime.GOARCH
	}
	if imageRef == "" {
		imageRef = fmt.Sprintf("ghcr.io/vasic-digital/crossbuild-linux:jdk17-%s", arch)
	}
	return &LinuxContainerBackend{imageRef: imageRef, arch: arch, runner: r}
}

func (l *LinuxContainerBackend) Name() string { return "linux-container" }

func (l *LinuxContainerBackend) Capabilities() Capabilities {
	return Capabilities{
		SupportsTargets: []Target{
			{OS: "linux", Arch: l.arch},
		},
		// Linux-in-Docker works on every host OS where the runtime is
		// available. Honest empty list.
		RequiresHostOS:      nil,
		IsolatesEnvironment: true,
		ArtifactNotes:       "Linux .deb/.rpm/jpackage via JDK 17 + Gradle in rootless container",
	}
}

func (l *LinuxContainerBackend) Build(ctx context.Context, req BuildRequest) BuildResult {
	start := time.Now()
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := validateRequest(req); err != nil {
		return BuildResult{Target: req.Target, BackendName: l.Name(), Error: err, Duration: time.Since(start)}
	}

	if !l.runner.ImageExists(ctx, l.imageRef) {
		return BuildResult{
			Target:      req.Target,
			BackendName: l.Name(),
			Duration:    time.Since(start),
			Error: fmt.Errorf(
				"crossbuild-linux image %q not present on host; "+
					"provision per docs/crossbuild/linux-image-provisioning.md "+
					"(SKIP-OK: #crossbuild-linux-image-provisioning if intentional)",
				l.imageRef),
		}
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := l.runner.Run(ctx, containerRunSpec{
		Image:       l.imageRef,
		MountSource: req.SourceDir,
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     req.BuildCommand,
		Environment: req.Environment,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})

	result := BuildResult{
		Target:      req.Target,
		BackendName: l.Name(),
		StdoutTail:  tailString(stdout.String(), 4096),
		StderrTail:  tailString(stderr.String(), 4096),
		Duration:    time.Since(start),
	}
	if err != nil {
		result.Error = fmt.Errorf("container build failed (exit=%d): %w", exitCode, err)
		return result
	}
	if exitCode != 0 {
		result.Error = fmt.Errorf("container build exited %d", exitCode)
		return result
	}

	produced := filepath.Join(req.SourceDir, req.OutputSubpath)
	stat, err := os.Stat(produced)
	if err != nil {
		result.Error = fmt.Errorf(
			"container build succeeded but artifact missing at %s: %w "+
				"(anti-bluff: 'BUILD SUCCESSFUL' without real artifact == bluff)",
			produced, err)
		return result
	}
	if stat.Size() == 0 {
		result.Error = fmt.Errorf(
			"container build produced zero-byte artifact at %s (bluff)", produced)
		return result
	}

	dst := filepath.Join(req.HostOutputDir, filepath.Base(produced))
	if err := copyFile(produced, dst); err != nil {
		result.Error = fmt.Errorf("copying artifact to HostOutputDir: %w", err)
		return result
	}
	result.ArtifactPath = dst
	result.ArtifactSize = stat.Size()
	return result
}
