package crossbuild

import (
	"context"
	"time"
)

// Target identifies the GOOS/GOARCH the build is targeting.
// Mirrors Go's runtime.GOOS / runtime.GOARCH conventions so consumers
// can pass `runtime.GOOS, runtime.GOARCH` directly when the build is
// host-native.
type Target struct {
	OS   string // "linux", "darwin", "windows", "freebsd", …
	Arch string // "amd64", "arm64", "riscv64", …
}

// IsHostNative reports whether the target matches the current process's
// runtime.GOOS / runtime.GOARCH. Used by the BackendSelector to short-
// circuit to the host-direct path when no virtualisation is needed.
func (t Target) IsHostNative(hostOS, hostArch string) bool {
	return t.OS == hostOS && t.Arch == hostArch
}

// BuildRequest is the consumer-facing input to the orchestrator.
// All fields are required except Environment, which may be nil.
type BuildRequest struct {
	// Target is the GOOS/GOARCH the artifact will run on.
	Target Target

	// SourceDir is the absolute path of the project root on the
	// host. The orchestrator copies this into the build environment
	// (volume mount for containers, scp upload for QEMU) before
	// invoking BuildCommand.
	SourceDir string

	// BuildCommand is the shell command to execute inside the build
	// environment, with SourceDir as the working directory. The
	// command MUST produce its artifact under OutputSubpath relative
	// to the build environment's view of SourceDir.
	//
	// Examples:
	//   - "./gradlew :desktopApp:packageReleaseDeb"
	//   - "./gradlew :desktopApp:packageReleaseMsi"
	//   - "go build -o bin/app ./cmd/app"
	BuildCommand string

	// OutputSubpath is the artifact path RELATIVE to SourceDir. The
	// orchestrator downloads this back to HostOutputDir after
	// BuildCommand exits 0.
	OutputSubpath string

	// HostOutputDir is the absolute host path where the produced
	// artifact lands. Must exist + be writable.
	HostOutputDir string

	// Environment is additional env vars to set inside the build
	// environment. Sensitive credentials (FIREBASE_TOKEN etc.) MUST
	// NOT be passed here unless the backend documents how it isolates
	// them — see Backend.Capabilities().
	Environment map[string]string

	// Timeout caps the total wall-clock of the build. Default is
	// 30 minutes if zero.
	Timeout time.Duration
}

// BuildResult describes the outcome of a BuildRequest.
type BuildResult struct {
	// Target echoes the input Target for caller correlation.
	Target Target

	// ArtifactPath is the absolute path on the host where the
	// produced artifact lives. Empty if Error is non-nil.
	ArtifactPath string

	// ArtifactSize is the size in bytes of ArtifactPath. Zero
	// indicates either the build failed or the orchestrator could
	// not stat the file. Useful for anti-bluff assertions (a
	// "BUILD SUCCESSFUL" without a non-zero artifact is a bluff
	// per the consuming project's Sixth Law clause 6.B equivalent).
	ArtifactSize int64

	// BackendName identifies which Backend executed this build.
	BackendName string

	// Duration is wall-clock from Backend.Build invocation to return.
	Duration time.Duration

	// StdoutTail is up to 4 KB of the build's stdout, for
	// diagnostics. Sensitive output may have been redacted by the
	// backend.
	StdoutTail string

	// StderrTail is up to 4 KB of the build's stderr.
	StderrTail string

	// Error is non-nil if the build failed for any reason.
	Error error
}

// Backend is the strategy interface implemented by each platform-
// specific build environment. The Selector chooses which Backend to
// invoke based on the BuildRequest's Target and the host's
// capabilities.
type Backend interface {
	// Name returns a short identifier ("host-direct", "wine-container",
	// "qemu-windows-vm", …) used in BuildResult.BackendName + logs.
	Name() string

	// Capabilities returns what this backend can build and how it
	// handles credentials/env.
	Capabilities() Capabilities

	// Build executes the BuildRequest synchronously. Implementations
	// MUST honour ctx cancellation + the request's Timeout.
	Build(ctx context.Context, req BuildRequest) BuildResult
}

// Capabilities documents what a Backend supports. Used by Selector +
// for documentation generation.
type Capabilities struct {
	// SupportsTargets is the list of (OS, Arch) tuples this Backend
	// can produce artifacts for.
	SupportsTargets []Target

	// RequiresHostOS, if non-empty, restricts this Backend to the
	// listed host OSes (e.g. wine-container needs Linux host with
	// /dev/kvm or similar).
	RequiresHostOS []string

	// IsolatesEnvironment reports whether environment variables
	// passed in BuildRequest.Environment are isolated from the
	// outside world (true for container/VM backends; false for
	// host-direct, which inherits the parent process's env).
	IsolatesEnvironment bool

	// ArtifactNotes is free-text describing artifact peculiarities
	// (e.g. "produces .msi on Windows", "produces .deb on Linux").
	ArtifactNotes string
}
