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
	if err := copyFile(req.SourceDir, req.OutputSubpath, dst); err != nil {
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

// subprocessWaitDelay bounds how long Cmd.Wait() spends waiting for a
// command's stdout/stderr pipes to reach EOF after the process has
// exited (or after ctx cancellation/Cancel has fired), before giving
// up on pipe forwarding and returning anyway (XB2-1). Without this
// (WaitDelay's zero value means "wait forever"), Run() can hang PAST
// the caller's declared ctx deadline whenever a backgrounded/detached
// descendant of the build command (a Gradle daemon, Wine, a `cmd &
// disown` shell job) inherited the same stdout/stderr file
// descriptors: those descriptors stay open — and pipe EOF never
// arrives — until every process holding them exits, which may be
// never. Mirrors the identical guard already present in
// pkg/runtime/docker.go's defaultExecutor.
const subprocessWaitDelay = 2 * time.Second

// maxCapturedOutputBytes bounds how many bytes of a build command's
// stdout/stderr are RETAINED in memory at once by boundedBufferWriter
// (XB3-5, §11.4.85 stress mandate). A build can run for 30-45 minutes;
// attaching a plain, uncapped bytes.Buffer as Cmd.Stdout/Stderr means a
// flooding or misbehaving build tool (an infinite log loop, a runaway
// verbose flag, a fork-bombing test harness echoing to its own stdout)
// can grow that buffer without bound for the ENTIRE run — exhausting
// host memory long before the existing tailString(…, 4096) truncation
// ever gets a chance to run, because that truncation happens exactly
// ONCE, at the very end, after the full uncapped buffer has already
// been allocated. 1 MiB comfortably exceeds the 4 KiB tail that
// BuildResult.StdoutTail/StderrTail actually surface, while remaining a
// small, fixed ceiling regardless of how much output the command
// produces.
const maxCapturedOutputBytes = 1 << 20 // 1 MiB

// boundedBufferWriter wraps a *bytes.Buffer and caps its RETAINED
// content to budget bytes, always keeping the TAIL (most recently
// written bytes) — the same "only the tail matters" principle
// tailString already applies once at the end of a build — instead of
// letting the underlying bytes.Buffer grow without bound for the whole
// lifetime of a long-running build (XB3-5). Every real subprocess
// runner in this package (realRunner, realContainerRunner,
// realAppleContainerRunner) attaches this wrapper — never the raw
// *bytes.Buffer — as Cmd.Stdout/Cmd.Stderr, so the caller-owned
// *bytes.Buffer keeps working exactly as before (.String(), .Reset(),
// .Len() all still read/mutate the SAME underlying buffer) while the
// amount of memory it can grow to is bounded.
//
// bytes.Buffer.Next(extra) advances the buffer's internal read offset
// past the oldest `extra` bytes rather than copying/reallocating the
// retained tail; bytes.Buffer's own Write path opportunistically slides
// the remaining unread bytes down to the front of the backing array
// when it needs more room, so the backing array does not grow linearly
// with total bytes ever written — capacity stays bounded to a small
// multiple of budget across an arbitrarily long-running, flooding
// command (verified empirically: 2 MiB written in 4 KiB chunks against
// a 1000-byte budget peaked at under 10 KiB of backing capacity).
type boundedBufferWriter struct {
	buf    *bytes.Buffer
	budget int
}

func (w *boundedBufferWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	if extra := w.buf.Len() - w.budget; extra > 0 {
		_ = w.buf.Next(extra)
	}
	return n, nil
}

type realRunner struct{}

func (realRunner) Run(ctx context.Context, dir, command string, env map[string]string,
	stdout, stderr *bytes.Buffer) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.WaitDelay = subprocessWaitDelay
	configureProcessGroup(cmd)
	if env != nil {
		envSlice := os.Environ()
		for k, v := range env {
			envSlice = append(envSlice, k+"="+v)
		}
		cmd.Env = envSlice
	}
	// XB3-5: bound retained memory via boundedBufferWriter rather than
	// handing the command's own process a direct, uncapped pointer into
	// stdout/stderr.
	cmd.Stdout = &boundedBufferWriter{buf: stdout, budget: maxCapturedOutputBytes}
	cmd.Stderr = &boundedBufferWriter{buf: stderr, budget: maxCapturedOutputBytes}
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
// symlink-based escape — the build dropping a symlink AT (or beneath, at
// any INTERMEDIATE path segment of) an in-SourceDir OutputSubpath that
// points outside — is a separate vector handled at the artifact-copy
// choke-point: copyFile resolves OutputSubpath against SourceDir via
// os.Root (XB3-1), which refuses to follow ANY path segment — final OR
// intermediate — whose symlink target would resolve outside SourceDir.
// Not here — this function never touches the filesystem.
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
	// XBUILD2-1 (§11.4.108/§11.4.1): the stat.Size()==0 check above is
	// meaningful only for a SINGLE-FILE artifact. For a DIRECTORY artifact
	// (a jpackage app-image / runtime image), os.Stat reports the
	// directory INODE's own size — non-zero whenever the directory holds
	// any entries at all (empirically 40-80 bytes here even for an empty
	// dir), so the zero-byte check silently PASSES a build that reported
	// success but dropped an EMPTY directory, or one whose only entries
	// are zero-byte files / empty subdirectories — i.e. produced NOTHING
	// usable. That is the exact "BUILD SUCCESSFUL but nothing really
	// built" bluff this file's anti-bluff posture exists to catch, just
	// shifted from the single-file shape to the directory shape. Walking
	// the tree and requiring at least one byte of REAL regular-file
	// content closes the hole deterministically, independent of the
	// filesystem's directory-inode size accounting.
	if stat.IsDir() && !directoryArtifactHasContent(produced) {
		return nil, fmt.Errorf(
			"build produced a directory artifact at %s holding zero bytes of real file content "+
				"(empty directory, or only zero-byte/special-file entries) — reporting an empty "+
				"directory as this build's fresh artifact is a bluff (anti-bluff §11.4.108)",
			produced)
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

// directoryArtifactHasContent reports whether a DIRECTORY artifact holds
// at least one byte of real regular-file content anywhere in its tree.
// It is the directory-shaped counterpart of verifyFreshArtifact's
// single-file `stat.Size()==0` anti-bluff check (XBUILD2-1,
// §11.4.108/§11.4.1): only real regular-file bytes count — a symlink,
// FIFO, socket, device, or empty subdirectory is NOT artifact "content",
// so a directory whose only entries are those (or zero-byte files) is a
// bluff even though its own inode size is non-zero.
func directoryArtifactHasContent(dir string) bool {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total > 0
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// copyFile copies a produced artifact (SourceDir/OutputSubpath) to its
// host-output destination. Regular files are copied byte-for-byte.
// Directory artifacts (e.g. a jpackage app-image / runtime image — see
// each backend's Capabilities().ArtifactNotes) are copied recursively: a
// real, non-empty directory app-image is a valid artifact and MUST NOT be
// reported as a build failure (§11.4.1 — a successful build with a
// directory artifact reported as FAIL is a FAIL-bluff).
//
// Security (XB3-1, artifact-fetch boundary): sourceDir + outputSubpath —
// not a pre-joined path — are taken deliberately so this function can
// resolve the produced artifact via os.OpenRoot(sourceDir) +
// Root.Lstat(outputSubpath). validateRequest's isWithinDir only proves
// OutputSubpath does not escape SourceDir by STRING math (no filesystem
// access); it cannot see a build that drops a FRESH symlink at an
// INTERMEDIATE path segment beneath SourceDir (e.g.
// "desktopApp/build/linked" -> /outside/dir) whose target resolves
// outside SourceDir — a plain os.Lstat/os.Open on the joined path
// transparently follows that intermediate symlink and would exfiltrate
// an arbitrary host file (e.g. an SSH key or "/outside/dir/secret.txt")
// into HostOutputDir even though the FINAL path component is an
// ordinary, non-symlink file. os.Root refuses to resolve ANY path
// segment — intermediate or final — whose symlink target would land
// outside the root, so Root.Lstat is the single choke-point that closes
// both the pre-existing top-level-symlink-artifact vector (XBUILD-F1,
// final component IS the symlink) AND the intermediate-segment vector
// (XB3-1) in one mechanism.
func copyFile(sourceDir, outputSubpath, dst string) error {
	src := filepath.Join(sourceDir, outputSubpath)

	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return fmt.Errorf("crossbuild: opening SourceDir %q as a root: %w", sourceDir, err)
	}
	defer root.Close()

	// Root.Lstat resolves every INTERMEDIATE path segment (following an
	// internal symlink only if its target stays within sourceDir) but —
	// matching plain os.Lstat semantics — does NOT follow the FINAL
	// component if it is itself a symlink; it reports that symlink's own
	// FileInfo instead, preserving the existing top-level-symlink check
	// below. Any segment (intermediate or final resolution) that would
	// escape sourceDir makes Lstat return an error instead of silently
	// following it.
	info, err := root.Lstat(outputSubpath)
	if err != nil {
		return fmt.Errorf(
			"crossbuild: refusing to copy artifact %q: OutputSubpath %q could not be resolved "+
				"safely within SourceDir %q: %w (anti-bluff/security XB3-1: a path segment — "+
				"intermediate or final — that is a symlink escaping SourceDir must not be "+
				"followed; the artifact-fetch step MUST NOT read files outside the project root)",
			src, outputSubpath, sourceDir, err)
	}
	// Security (artifact-fetch boundary): refuse to follow a TOP-LEVEL
	// artifact that is itself a symlink. The file the build drops AT
	// OutputSubpath can be a freshly-created symlink pointing OUTSIDE
	// SourceDir — following it would exfiltrate an arbitrary host file
	// (e.g. an SSH key) into HostOutputDir. copyDir already refuses to
	// follow symlinks INSIDE a directory artifact; this gives the
	// top-level produced path the same no-follow treatment. Fail
	// closed — a build whose declared artifact is a symlink is refused,
	// not silently dereferenced.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"crossbuild: refusing to copy artifact %q: it is a symlink "+
				"(anti-bluff/security: the artifact-fetch step MUST NOT follow a link "+
				"that may resolve outside the project root)", src)
	}
	var copyErr error
	if info.IsDir() {
		// The top-level directory itself was just proven reachable
		// without crossing an escaping symlink (Root.Lstat succeeded
		// above); the recursive walk below independently re-validates
		// every NESTED symlink target against the same root (see
		// copyDir's own isWithinDir check), so using the plain absolute
		// path from here on is safe.
		copyErr = copyDir(src, src, dst)
	} else {
		copyErr = copyRegularFileFromRoot(root, outputSubpath, dst, info.Mode())
	}
	if copyErr != nil {
		// XB2-3: a failure partway through the copy (a nested entry
		// that cannot be written, a rejected escaping symlink, a
		// permission error, …) MUST NOT leave partial/truncated debris
		// under HostOutputDir — a downstream glob of HostOutputDir/*
		// could otherwise pick up a corrupt leftover from a FAILED
		// build and mistake it for a real artifact. Best-effort: the
		// removal's own error is deliberately swallowed — the
		// ORIGINAL copy error is what the caller needs to see, and a
		// failure to clean up debris does not change the fact that
		// the copy itself failed.
		_ = os.RemoveAll(dst)
		return copyErr
	}
	return nil
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

// copyRegularFileFromRoot is copyRegularFile's XB3-1 counterpart for the
// TOP-LEVEL produced artifact: it opens the source file THROUGH the
// os.Root (root.Open) that already proved outputSubpath resolves inside
// sourceDir without crossing an escaping symlink, rather than re-deriving
// the path and calling plain os.Open — this keeps the whole
// resolve-then-read step mediated by the same root handle end to end,
// minimising the TOCTOU window between the Root.Lstat check in copyFile
// and the actual read.
func copyRegularFileFromRoot(root *os.Root, outputSubpath, dst string, mode os.FileMode) error {
	srcFile, err := root.Open(outputSubpath)
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
//
// root is the TOP-LEVEL artifact directory (the same value across the
// whole recursion — only src/dst change on nested calls). XB2-2: a
// nested symlink's TARGET is validated to stay within root before it is
// recreated. Without this, a build that drops a symlink deep inside a
// directory artifact pointing at an absolute host path (or a
// "../../.."-escaping relative path) would have that escaping/dangling
// link recreated VERBATIM in HostOutputDir — shipping a link that
// resolves outside the project root to whatever consumes the artifact
// next. Escaping targets are refused outright (fail closed), matching
// the existing top-level-symlink-artifact policy in copyFile.
func copyDir(root, src, dst string) error {
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
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(srcPath), resolved)
			}
			if !isWithinDir(root, resolved) {
				return fmt.Errorf(
					"crossbuild: refusing to copy directory-artifact symlink %q: its target %q "+
						"escapes the artifact root %q "+
						"(anti-bluff/security: a nested symlink inside a directory artifact "+
						"MUST NOT resolve outside the artifact tree)",
					srcPath, target, root)
			}
			if err := os.Symlink(target, dstPath); err != nil {
				return err
			}
		case info.IsDir():
			if err := copyDir(root, srcPath, dstPath); err != nil {
				return err
			}
		default:
			// XBUILD2-3 (security/DoS): a directory-artifact entry that is
			// neither a symlink (handled above), a directory (handled
			// above), nor a REGULAR file — i.e. a FIFO/named-pipe, unix
			// socket, or char/block device special file — must be REFUSED,
			// never opened. copyRegularFile's os.Open on a named pipe with
			// no writer BLOCKS FOREVER (the artifact-copy step is not
			// ctx-bounded, so req.Timeout does not save it), wedging the
			// whole build past its declared deadline; a device/socket would
			// copy garbage or error. Only real regular files are valid
			// artifact content here. Fail closed, matching copyFile's
			// top-level symlink-refusal and copyDir's escaping-symlink
			// refusal policy.
			if !info.Mode().IsRegular() {
				return fmt.Errorf(
					"crossbuild: refusing to copy directory-artifact entry %q: it is a %v special file "+
						"(FIFO/socket/device), not a regular file (anti-bluff/security: the artifact-copy "+
						"step MUST NOT open a special file — a named pipe with no writer blocks the copy "+
						"indefinitely)",
					srcPath, info.Mode().Type())
			}
			if err := copyRegularFile(srcPath, dstPath, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}
