package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// HostDirectBackend executes the BuildCommand on the host directly, no
// container or VM. It is the fast path for host-native targets
// (Linux→Linux, Darwin→Darwin, etc.) where virtualisation would only
// add overhead.
//
// Anti-bluff: this backend is the OPERATIONAL one — it actually runs
// the build. Tests do NOT mock os/exec; they assert orchestration
// via the processRunner seam below.
type HostDirectBackend struct {
	runner processRunner
}

// NewHostDirectBackend returns the production backend.
func NewHostDirectBackend() *HostDirectBackend {
	return &HostDirectBackend{runner: realRunner{}}
}

// newHostDirectBackendWithRunner is the test seam.
func newHostDirectBackendWithRunner(r processRunner) *HostDirectBackend {
	return &HostDirectBackend{runner: r}
}

func (h *HostDirectBackend) Name() string { return "host-direct" }

func (h *HostDirectBackend) Capabilities() Capabilities {
	return Capabilities{
		// Host-direct supports the current host's GOOS/GOARCH only.
		// The Selector relies on this list being accurate.
		SupportsTargets: []Target{
			{OS: runtime.GOOS, Arch: runtime.GOARCH},
		},
		RequiresHostOS:      nil, // works on every host
		IsolatesEnvironment: false,
		ArtifactNotes:       "produces native artifact for host OS/arch (no virtualisation)",
	}
}

func (h *HostDirectBackend) Build(ctx context.Context, req BuildRequest) BuildResult {
	start := time.Now()
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := validateRequest(req); err != nil {
		return BuildResult{Target: req.Target, BackendName: h.Name(), Error: err, Duration: time.Since(start)}
	}

	// Snapshot the declared output path BEFORE the build runs so a
	// leftover artifact from a prior invocation can be told apart from
	// one this invocation genuinely produced (see verifyFreshArtifact).
	produced := filepath.Join(req.SourceDir, req.OutputSubpath)
	preExisting := statIfExists(produced)

	var stdout, stderr bytes.Buffer
	exitCode, err := h.runner.Run(ctx, req.SourceDir, req.BuildCommand, req.Environment, &stdout, &stderr)

	result := BuildResult{
		Target:      req.Target,
		BackendName: h.Name(),
		StdoutTail:  tailString(stdout.String(), 4096),
		StderrTail:  tailString(stderr.String(), 4096),
		Duration:    time.Since(start),
	}
	if err != nil {
		result.Error = fmt.Errorf("build command failed (exit=%d): %w", exitCode, err)
		return result
	}
	if exitCode != 0 {
		result.Error = fmt.Errorf("build command exited %d", exitCode)
		return result
	}

	stat, err := verifyFreshArtifact(produced, preExisting)
	if err != nil {
		result.Error = err
		return result
	}

	// Copy from SourceDir/OutputSubpath to HostOutputDir/<basename>.
	dst := filepath.Join(req.HostOutputDir, filepath.Base(produced))
	if err := copyFile(produced, dst); err != nil {
		result.Error = fmt.Errorf("copying artifact to HostOutputDir: %w", err)
		return result
	}
	result.ArtifactPath = dst
	result.ArtifactSize = stat.Size()
	return result
}

// processRunner is the seam for tests. Production uses realRunner.
type processRunner interface {
	Run(ctx context.Context, dir, command string, env map[string]string,
		stdout, stderr *bytes.Buffer) (exitCode int, err error)
}

type realRunner struct{}

func (realRunner) Run(ctx context.Context, dir, command string, env map[string]string,
	stdout, stderr *bytes.Buffer) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	if env != nil {
		envSlice := os.Environ()
		for k, v := range env {
			envSlice = append(envSlice, k+"="+v)
		}
		cmd.Env = envSlice
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, err
}

func validateRequest(req BuildRequest) error {
	if req.SourceDir == "" {
		return fmt.Errorf("crossbuild: BuildRequest.SourceDir is required")
	}
	if !filepath.IsAbs(req.SourceDir) {
		return fmt.Errorf("crossbuild: BuildRequest.SourceDir must be absolute, got %q", req.SourceDir)
	}
	if _, err := os.Stat(req.SourceDir); err != nil {
		return fmt.Errorf("crossbuild: BuildRequest.SourceDir not accessible: %w", err)
	}
	if req.BuildCommand == "" {
		return fmt.Errorf("crossbuild: BuildRequest.BuildCommand is required")
	}
	if req.OutputSubpath == "" {
		return fmt.Errorf("crossbuild: BuildRequest.OutputSubpath is required")
	}
	if filepath.IsAbs(req.OutputSubpath) {
		return fmt.Errorf(
			"crossbuild: BuildRequest.OutputSubpath must be relative to SourceDir, got absolute path %q",
			req.OutputSubpath)
	}
	if !isWithinDir(req.SourceDir, filepath.Join(req.SourceDir, req.OutputSubpath)) {
		return fmt.Errorf(
			"crossbuild: BuildRequest.OutputSubpath %q escapes SourceDir %q "+
				"(anti-bluff: the artifact-fetch step MUST NOT read files outside the project root)",
			req.OutputSubpath, req.SourceDir)
	}
	if filepath.Clean(req.OutputSubpath) == "." {
		return fmt.Errorf(
			"crossbuild: BuildRequest.OutputSubpath %q resolves to SourceDir itself; it MUST name "+
				"an artifact within SourceDir, not the whole source tree",
			req.OutputSubpath)
	}
	if req.HostOutputDir == "" {
		return fmt.Errorf("crossbuild: BuildRequest.HostOutputDir is required")
	}
	if err := os.MkdirAll(req.HostOutputDir, 0o755); err != nil {
		return fmt.Errorf("crossbuild: HostOutputDir not creatable: %w", err)
	}
	return nil
}

// isWithinDir reports whether target (the result of filepath.Join(root,
// subpath)) resolves to a path inside root. filepath.Join already
// cleans ".." segments syntactically, so this is a pure string/prefix
// check via filepath.Rel — no filesystem access, no symlink resolution
// (SourceDir's existence is already verified by the caller). A
// symlink-based escape — the build dropping a symlink AT an in-SourceDir
// OutputSubpath that points outside — is a separate vector handled at the
// artifact-copy choke-point (copyFile refuses to follow a top-level
// symlink artifact), not here.
func isWithinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// statIfExists returns the FileInfo for path if it currently exists, or
// nil otherwise. Any stat error (not-exist, permission, …) is treated
// as "no pre-existing snapshot" — a genuine problem at that path
// surfaces from the authoritative os.Stat call in verifyFreshArtifact
// after the build runs.
func statIfExists(path string) os.FileInfo {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// verifyFreshArtifact confirms a build's declared output (a) exists,
// (b) is non-empty, and (c) was actually produced/modified by THIS
// build invocation rather than being a leftover artifact from a PRIOR
// successful build. pre is the statIfExists snapshot captured
// immediately before the backend invoked its runner; nil means nothing
// existed at that path yet.
//
// Without check (c), a build command that reports success (exit 0)
// without regenerating its declared output — a broken script, a
// container whose bind mount silently failed to apply, a build tool
// that no-ops because it thinks the target is already up to date — is
// indistinguishable from a genuine fresh build: os.Stat still finds a
// non-empty file (the stale one) and the backend reports SUCCESS with
// that stale artifact's bytes. That is exactly the "BUILD SUCCESSFUL
// but nothing really built" bluff this package's anti-bluff posture
// (see doc.go) exists to catch.
//
// Detection is purely filesystem-native (identical mtime AND size to
// the pre-run snapshot) rather than comparing against a wall-clock
// "build start" timestamp, so it carries no clock-resolution/skew
// flakiness risk.
//
// Honest boundary (§11.4.6): for a DIRECTORY artifact (e.g. a jpackage
// app-image), pre/post are the directory entry's own metadata — a
// build that rewrites a file already present inside the directory with
// identical name+size may not change the directory's own mtime, so
// this check's guarantee for directory artifacts is narrower than for
// single-file artifacts. It still catches the common case (directory
// freshly created, or entries added/removed) and never produces a
// false failure for a genuinely fresh build.
func verifyFreshArtifact(produced string, pre os.FileInfo) (os.FileInfo, error) {
	stat, err := os.Stat(produced)
	if err != nil {
		return nil, fmt.Errorf(
			"build command succeeded but artifact missing at %s: %w "+
				"(anti-bluff: a 'BUILD SUCCESSFUL' without a real artifact is a bluff)",
			produced, err)
	}
	if stat.Size() == 0 {
		return nil, fmt.Errorf(
			"build produced a zero-byte artifact at %s "+
				"(anti-bluff: empty artifact == bluff)", produced)
	}
	if pre != nil && stat.ModTime().Equal(pre.ModTime()) && stat.Size() == pre.Size() {
		return nil, fmt.Errorf(
			"build command succeeded but artifact at %s was NOT modified by this build "+
				"(identical mtime %s + size %d bytes as before the build ran) — reporting a "+
				"stale, leftover artifact from a prior build as this build's fresh output is a "+
				"bluff (anti-bluff)",
			produced, stat.ModTime().Format(time.RFC3339Nano), stat.Size())
	}
	return stat, nil
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// copyFile copies a produced artifact to its host-output destination.
// Regular files are copied byte-for-byte. Directory artifacts (e.g. a
// jpackage app-image / runtime image — see each backend's
// Capabilities().ArtifactNotes) are copied recursively: a real, non-empty
// directory app-image is a valid artifact and MUST NOT be reported as a
// build failure (§11.4.1 — a successful build with a directory artifact
// reported as FAIL is a FAIL-bluff).
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	// Security (artifact-fetch boundary): refuse to follow a TOP-LEVEL
	// artifact that is itself a symlink. validateRequest keeps OutputSubpath
	// within SourceDir by string math (isWithinDir), but the file the build
	// drops AT that path can be a freshly-created symlink pointing OUTSIDE
	// SourceDir — copyRegularFile's os.Open would follow it and exfiltrate an
	// arbitrary host file (e.g. an SSH key) into HostOutputDir. copyDir
	// already refuses to follow symlinks INSIDE a directory artifact; this
	// gives the top-level produced path the same no-follow treatment. Fail
	// closed — a build whose declared artifact is a symlink is refused, not
	// silently dereferenced.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"crossbuild: refusing to copy artifact %q: it is a symlink "+
				"(anti-bluff/security: the artifact-fetch step MUST NOT follow a link "+
				"that may resolve outside the project root)", src)
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	return copyRegularFile(src, dst, info.Mode())
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()
	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := dstFile.ReadFrom(srcFile); err != nil {
		dstFile.Close()
		return err
	}
	return dstFile.Close()
}

// copyDir recursively copies the directory tree at src to dst, preserving
// the relative structure. Symlinks are copied as symlinks (not followed)
// so a malicious or accidental link inside the artifact cannot make the
// orchestrator read outside the tree.
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, srcInfo.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		info, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dstPath); err != nil {
				return err
			}
		case info.IsDir():
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		default:
			if err := copyRegularFile(srcPath, dstPath, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}
