package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// WineContainerBackend cross-builds Windows artifacts by executing the
// BuildCommand inside a Linux container that has Wine + JDK + Gradle
// pre-installed. This is faster than the QEMU full-system path
// (no VM boot overhead) but limited to artifacts Wine can produce
// successfully (jpackage's .msi target works under recent Wine
// releases — see docs/crossbuild/windows-image-provisioning.md).
//
// Anti-bluff: this backend currently ships in SKELETON state. The
// container image is operator-provisioned per the documented
// procedure; the orchestration code + tests are in place; the real-
// stack Challenge skips with `SKIP-OK:
// #crossbuild-windows-image-provisioning` until the operator
// completes provisioning. The skeleton is NOT a bluff because:
//
//  1. The Capabilities() honestly advertises only windows/amd64 +
//     RequiresHostOS: linux (Wine in macOS containers is unstable).
//  2. The Build() honestly returns an error when the configured
//     container image does not exist on the host.
//  3. The orchestration test exercises the FULL code path with an
//     injected ContainerRunner fake — proving Selector→Backend→
//     ContainerRunner wiring works.
//
// Operator provisioning steps:
//
//	# On a Linux host with rootless podman:
//	cd Submodules/Containers/pkg/crossbuild
//	podman build -t ghcr.io/vasic-digital/crossbuild-wine:latest \
//	    -f windows_wine.Containerfile .
//
//	# Verify:
//	podman run --rm ghcr.io/vasic-digital/crossbuild-wine:latest wine --version
//
// Once the image exists, this Backend's Build() succeeds end-to-end.
type WineContainerBackend struct {
	imageRef string
	runner   containerRunner
}

// NewWineContainerBackend returns the production backend. imageRef
// defaults to "ghcr.io/vasic-digital/crossbuild-wine:latest" when
// empty.
func NewWineContainerBackend(imageRef string) *WineContainerBackend {
	if imageRef == "" {
		imageRef = "ghcr.io/vasic-digital/crossbuild-wine:latest"
	}
	return &WineContainerBackend{
		imageRef: imageRef,
		runner:   realContainerRunner{},
	}
}

// newWineContainerBackendWithRunner is the test seam.
func newWineContainerBackendWithRunner(imageRef string, r containerRunner) *WineContainerBackend {
	if imageRef == "" {
		imageRef = "ghcr.io/vasic-digital/crossbuild-wine:latest"
	}
	return &WineContainerBackend{imageRef: imageRef, runner: r}
}

func (w *WineContainerBackend) Name() string { return "wine-container" }

func (w *WineContainerBackend) Capabilities() Capabilities {
	return Capabilities{
		SupportsTargets: []Target{
			{OS: "windows", Arch: "amd64"},
		},
		// Wine-in-Docker reliably runs only on Linux hosts. macOS
		// QEMU-User translates badly with Wine. Restrict honestly.
		RequiresHostOS:      []string{"linux"},
		IsolatesEnvironment: true,
		ArtifactNotes:       "Windows .msi/.exe via Wine in rootless container",
	}
}

func (w *WineContainerBackend) Build(ctx context.Context, req BuildRequest) BuildResult {
	start := time.Now()
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := validateRequest(req); err != nil {
		return BuildResult{Target: req.Target, BackendName: w.Name(), Error: err, Duration: time.Since(start)}
	}

	// Verify image exists; honest failure if not provisioned.
	if !w.runner.ImageExists(ctx, w.imageRef) {
		return BuildResult{
			Target:      req.Target,
			BackendName: w.Name(),
			Duration:    time.Since(start),
			Error: fmt.Errorf(
				"crossbuild-wine image %q not present on host; "+
					"provision per docs/crossbuild/windows-image-provisioning.md "+
					"(SKIP-OK: #crossbuild-windows-image-provisioning if intentional)",
				w.imageRef),
		}
	}

	// Artifact lives on the bind-mounted source dir; rsync-equivalent to
	// the host output. Snapshot BEFORE the container runs so a leftover
	// artifact from a prior invocation can be told apart from one this
	// invocation genuinely produced (see verifyFreshArtifact).
	produced := filepath.Join(req.SourceDir, req.OutputSubpath)
	preExisting := statIfExists(produced)

	var stdout, stderr bytes.Buffer
	exitCode, err := w.runner.Run(ctx, containerRunSpec{
		Image:       w.imageRef,
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
		BackendName: w.Name(),
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

	stat, err := verifyFreshArtifact(produced, preExisting)
	if err != nil {
		result.Error = err
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
