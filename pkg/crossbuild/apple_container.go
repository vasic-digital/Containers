package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AppleContainerBackend cross-builds Linux artifacts on a macOS /
// Apple-Silicon host by executing the BuildCommand inside a lightweight
// Linux virtual machine managed by Apple's `container` CLI
// (github.com/apple/container). Each `container run` boots one
// micro-VM per container through the Virtualization framework with a
// minimal Linux guest userspace; it consumes + produces standard OCI
// images, so vanilla alpine/ubuntu/debian arm64 images run natively
// (no Rosetta / x86 emulation).
//
// This backend is the Apple-Silicon-native sibling of
// LinuxContainerBackend. The differences:
//
//  1. RequiresHostOS is ["darwin"] — Apple `container` is macOS-only.
//     This is the honest restriction the Selector routes on; on a
//     non-darwin host the backend is never Chosen and the existing
//     podman/docker LinuxContainerBackend serves the target instead.
//  2. SupportsTargets is restricted to {linux, arm64}. amd64 is
//     intentionally NOT advertised — x86 Linux on Apple `container`
//     depends on Rosetta, which has documented kernel-version
//     regressions. Native arm64 Linux is what an Apple-Silicon host
//     wants anyway.
//  3. The runner shells out to `container run` (not podman/docker)
//     with an explicit `--mount type=virtiofs,...` host-dir mount,
//     falling back to `-v src:dst` if the installed `container`
//     version rejects the explicit mount form.
//
// The same containerRunner-shaped seam (appleContainerRunner) is used
// so tests inject a fake and production uses realAppleContainerRunner.
// The anti-bluff post-conditions (non-zero artifact assertion,
// zero-byte-is-bluff error, missing-image honest error) are identical
// to LinuxContainerBackend.
type AppleContainerBackend struct {
	imageRef string
	arch     string // always "arm64" for this backend; field kept for symmetry
	runner   appleContainerRunner
}

// NewAppleContainerBackend returns the production backend. imageRef
// defaults to "docker.io/library/alpine:latest" (a small OCI base that
// Apple `container` pulls natively for arm64) when empty. The arch is
// fixed to "arm64" — Apple `container` runs on Apple Silicon and this
// backend deliberately does not offer amd64 (see type doc).
func NewAppleContainerBackend(imageRef string) *AppleContainerBackend {
	if imageRef == "" {
		imageRef = "docker.io/library/alpine:latest"
	}
	return &AppleContainerBackend{
		imageRef: imageRef,
		arch:     "arm64",
		runner:   realAppleContainerRunner{},
	}
}

// newAppleContainerBackendWithRunner is the test seam.
func newAppleContainerBackendWithRunner(imageRef string, r appleContainerRunner) *AppleContainerBackend {
	if imageRef == "" {
		imageRef = "docker.io/library/alpine:latest"
	}
	return &AppleContainerBackend{imageRef: imageRef, arch: "arm64", runner: r}
}

func (a *AppleContainerBackend) Name() string { return "apple-container" }

func (a *AppleContainerBackend) Capabilities() Capabilities {
	return Capabilities{
		SupportsTargets: []Target{
			{OS: "linux", Arch: "arm64"},
		},
		// Apple `container` is macOS-only. This is the honest
		// restriction the Selector routes on.
		RequiresHostOS:      []string{"darwin"},
		IsolatesEnvironment: true,
		ArtifactNotes:       "Linux arm64 build inside an Apple-container lightweight VM (Virtualization framework)",
	}
}

func (a *AppleContainerBackend) Build(ctx context.Context, req BuildRequest) BuildResult {
	start := time.Now()
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := validateRequest(req); err != nil {
		return BuildResult{Target: req.Target, BackendName: a.Name(), Error: err, Duration: time.Since(start)}
	}

	// Engine presence is the first honest gate: if `container` is not
	// installed, Build cannot proceed and the operator is pointed at
	// the provisioning path + a SKIP-OK ticket.
	if !a.runner.Available() {
		return BuildResult{
			Target:      req.Target,
			BackendName: a.Name(),
			Duration:    time.Since(start),
			Error: fmt.Errorf(
				"apple `container` CLI not found on PATH; install per " +
					"https://github.com/apple/container then `container system start` " +
					"(SKIP-OK: #apple-container-provisioning if intentional — " +
					"the podman/docker linux-container backend serves the same target)"),
		}
	}

	if !a.runner.ImageExists(ctx, a.imageRef) {
		return BuildResult{
			Target:      req.Target,
			BackendName: a.Name(),
			Duration:    time.Since(start),
			Error: fmt.Errorf(
				"apple-container image %q not present on host; "+
					"provision with `container image pull %s` "+
					"(SKIP-OK: #apple-container-image-provisioning if intentional)",
				a.imageRef, a.imageRef),
		}
	}

	// Snapshot the declared output path BEFORE the container runs so a
	// leftover artifact from a prior invocation can be told apart from
	// one this invocation genuinely produced (see verifyFreshArtifact).
	produced := filepath.Join(req.SourceDir, req.OutputSubpath)
	preExisting := statIfExists(produced)

	var stdout, stderr bytes.Buffer
	exitCode, err := a.runner.Run(ctx, appleContainerRunSpec{
		Image:       a.imageRef,
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
		BackendName: a.Name(),
		StdoutTail:  tailString(stdout.String(), 4096),
		StderrTail:  tailString(stderr.String(), 4096),
		Duration:    time.Since(start),
	}
	if err != nil {
		result.Error = fmt.Errorf("apple-container build failed (exit=%d): %w", exitCode, err)
		return result
	}
	if exitCode != 0 {
		result.Error = fmt.Errorf("apple-container build exited %d", exitCode)
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

// appleContainerRunner is the seam for invoking Apple's `container`
// CLI. Production uses realAppleContainerRunner; tests inject a fake to
// assert the exact argv without a live engine.
type appleContainerRunner interface {
	// Available reports whether the `container` binary is on PATH.
	Available() bool
	// ImageExists reports whether imageRef is already pulled.
	ImageExists(ctx context.Context, imageRef string) bool
	// Run executes a single command in a fresh `--rm` container with
	// the host dir mounted, capturing stdout/stderr + exit code.
	Run(ctx context.Context, spec appleContainerRunSpec) (exitCode int, err error)
}

// appleContainerRunSpec is the minimum surface the Apple-container
// backend needs. MountMode selects the host-dir mount flavour; when
// empty the runner uses "virtiofs" first and falls back to the short
// "-v src:dst" form if the installed version rejects it.
type appleContainerRunSpec struct {
	Image       string
	Name        string // optional explicit container name; generated if empty
	MountSource string
	MountTarget string
	MountMode   string // "virtiofs" | "volume" | "" (auto: virtiofs then volume)
	WorkDir     string
	Command     string
	Environment map[string]string
	Stdout      *bytes.Buffer
	Stderr      *bytes.Buffer
}

// appleContainerBinary is the name looked up on PATH. Kept as a var so
// tests can confirm the lookup target without hardcoding it twice.
const appleContainerBinary = "container"

// buildRunArgs constructs the exact `container run ...` argv for a spec
// using the given mount mode. It is a pure function so unit tests can
// assert the argv shape without a live engine. mode MUST be "virtiofs"
// or "volume".
func buildRunArgs(spec appleContainerRunSpec, mode string) []string {
	args := []string{"run", "--rm"}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	// A mount flag is emitted ONLY when a host source is supplied; an
	// empty source means "run against the image filesystem" and must
	// not produce a degenerate `source=,target=` flag.
	if spec.MountSource != "" {
		switch mode {
		case "volume":
			// Short bind form: -v <host>:<container>
			args = append(args, "-v", spec.MountSource+":"+spec.MountTarget)
		default: // "virtiofs"
			// Explicit, unambiguous form.
			args = append(args, "--mount",
				"type=virtiofs,source="+spec.MountSource+",target="+spec.MountTarget)
		}
	}
	args = append(args, "-w", spec.WorkDir)
	// Deterministic env ordering so argv is stable + testable.
	for _, kv := range sortedEnv(spec.Environment) {
		args = append(args, "-e", kv)
	}
	args = append(args, spec.Image, "sh", "-c", spec.Command)
	return args
}

// sortedEnv returns "k=v" entries sorted by key for deterministic argv.
func sortedEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	// simple insertion sort to avoid importing sort here is overkill;
	// use a small selection to keep deterministic without extra deps.
	for i := 0; i < len(keys); i++ {
		min := i
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[min] {
				min = j
			}
		}
		keys[i], keys[min] = keys[min], keys[i]
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

type realAppleContainerRunner struct{}

func (realAppleContainerRunner) Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath(appleContainerBinary)
	return err == nil
}

func (realAppleContainerRunner) ImageExists(ctx context.Context, imageRef string) bool {
	if _, err := exec.LookPath(appleContainerBinary); err != nil {
		return false
	}
	// `container image list` prints NAME/TAG columns. Match the
	// repo+tag substring of the ref against the listing.
	cmd := exec.CommandContext(ctx, appleContainerBinary, "image", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	return imageListed(out.String(), imageRef)
}

// imageListed reports whether the `container image list` output names
// the image referenced by ref. Apple `container` lists NAME + TAG in
// separate columns (e.g. "alpine  latest"), so we match the repo's
// final path segment and, when present, its tag.
func imageListed(listing, ref string) bool {
	repo, tag := splitImageRef(ref)
	repoLeaf := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repoLeaf = repo[i+1:]
	}
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != repoLeaf {
			continue
		}
		if tag == "" {
			return true
		}
		// NAME is column 0; TAG (when present) is column 1.
		if len(fields) >= 2 && fields[1] == tag {
			return true
		}
		// Some versions print "repo:tag" in a single column.
		if strings.Contains(fields[0], ":"+tag) {
			return true
		}
	}
	return false
}

// splitImageRef splits "repo:tag" into (repo, tag). A digest ref
// ("repo@sha256:...") yields tag "". When no tag is present tag is "".
func splitImageRef(ref string) (repo, tag string) {
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[:at], ""
	}
	// Find the last ':' that is part of a tag (not a registry port). A
	// tag ':' appears after the last '/'.
	lastSlash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > lastSlash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

func (r realAppleContainerRunner) Run(ctx context.Context, spec appleContainerRunSpec) (int, error) {
	if _, err := exec.LookPath(appleContainerBinary); err != nil {
		return -1, fmt.Errorf("apple `container` CLI not found on PATH: %w", err)
	}
	modes := []string{"virtiofs", "volume"}
	if spec.MountMode == "virtiofs" || spec.MountMode == "volume" {
		modes = []string{spec.MountMode}
	}

	var lastExit int = -1
	var lastErr error
	for _, mode := range modes {
		args := buildRunArgs(spec, mode)
		cmd := exec.CommandContext(ctx, appleContainerBinary, args...)
		// XB2-1: bound Wait()'s pipe-drain and kill the whole process
		// group (not just this `container run` invocation) on ctx
		// cancellation — identical rationale to container_runner.go's
		// realContainerRunner.Run.
		cmd.WaitDelay = subprocessWaitDelay
		configureProcessGroup(cmd)
		cmd.Stdout = spec.Stdout
		cmd.Stderr = spec.Stderr
		err := cmd.Run()
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		if err == nil {
			return exitCode, nil
		}
		lastExit, lastErr = exitCode, err
		// Only fall through to the next mount mode when the failure
		// looks like a mount-flag rejection AND the command produced
		// no useful output. ctx cancellation/timeout must not retry.
		if ctx.Err() != nil {
			break
		}
		if !mountFlagRejected(spec.Stderr) {
			break
		}
		// Reset buffers before retrying with the fallback mount form.
		spec.Stdout.Reset()
		spec.Stderr.Reset()
	}
	return lastExit, lastErr
}

// mountFlagRejected heuristically detects that the installed
// `container` version rejected the explicit --mount form, so the
// runner should retry with the short -v form. Per §11.4.6 this is the
// ONLY fallback trigger — a genuine in-container command failure must
// not be masked by a retry.
func mountFlagRejected(stderr *bytes.Buffer) bool {
	if stderr == nil {
		return false
	}
	s := strings.ToLower(stderr.String())
	return strings.Contains(s, "mount") &&
		(strings.Contains(s, "unknown") ||
			strings.Contains(s, "unsupported") ||
			strings.Contains(s, "invalid") ||
			strings.Contains(s, "unrecognized") ||
			strings.Contains(s, "unexpected"))
}
