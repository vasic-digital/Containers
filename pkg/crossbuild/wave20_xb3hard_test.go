package crossbuild

// Wave-20 XB3-HARD — §11.4.118 fresh discovery-pressure audit of
// pkg/crossbuild, five findings fixed + guarded here:
//
//   - XB3-1 (HIGH SECURITY, flagship): isWithinDir (validateRequest) was
//     pure string math and copyFile's symlink guard (XBUILD-F1) only
//     rejected a TOP-LEVEL artifact that is ITSELF a symlink. An
//     INTERMEDIATE segment of OutputSubpath that is a symlink to an
//     outside directory was followed transparently by a plain
//     os.Lstat/os.Open on the joined path, exfiltrating an arbitrary
//     host file into HostOutputDir even though the final path
//     component was an ordinary file. Fixed in host_direct.go's
//     copyFile: OutputSubpath is now resolved against SourceDir via
//     os.OpenRoot + Root.Lstat/Root.Open (go1.25+), which refuses to
//     follow ANY path segment — intermediate or final — whose symlink
//     target would land outside SourceDir. Affects all 4 backends
//     (they share copyFile).
//   - XB3-2 (HIGH): realAppleContainerRunner.ImageExists set no
//     cmd.WaitDelay + no configureProcessGroup despite capturing
//     Stdout/Stderr into a bytes.Buffer, so it could hang past ctx on a
//     wedged `container image list` whose descendant inherited the
//     pipes — called from EVERY Build(). Fixed with the same
//     WaitDelay + configureProcessGroup guard already present on
//     Run().
//   - XB3-3 (MED, §11.4.14): realContainerRunner.ImageExists lacked
//     configureProcessGroup, orphaning any descendant on ctx
//     cancellation. Fixed with configureProcessGroup + WaitDelay.
//   - XB3-4 (MED, §11.4.108): mountFlagRejected scanned the ENTIRE
//     combined stderr for co-occurrence of "mount" + a rejection word
//     ANYWHERE in the buffer, false-positiving on unrelated log lines,
//     and Run() unconditionally Reset() the shared Stdout/Stderr
//     buffers before a fallback retry, destroying the first attempt's
//     real diagnostics if the fallback also failed. Fixed:
//     mountFlagRejected now requires same-LINE co-occurrence; Run()
//     preserves + restores the first attempt's captured output ahead
//     of the fallback's own output on a final failure.
//   - XB3-5 (MED, §11.4.85): all three real runners attached an
//     uncapped bytes.Buffer as Stdout/Stderr for a 30-45 min build, so
//     a flooding build tool could exhaust host memory long before the
//     one-time end-of-build tailString truncation ever ran. Fixed via
//     boundedBufferWriter, which caps retained bytes to
//     maxCapturedOutputBytes while always keeping the tail.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// XB3-1 — intermediate-segment symlink escape via the REAL
// HostDirectBackend (real sh -c runner, no mocks) — the flagship
// finding.
// ---------------------------------------------------------------------

// TestXB3_1_HostDirect_IntermediateSymlinkArtifactExfilBlocked is the
// §11.4.115 GREEN-polarity guard for XB3-1. Unlike XBUILD-F1
// (hardening_symlink_exfil_test.go), the FINAL path component
// ("id_ed25519") is an ORDINARY file name — never itself a symlink. The
// escape is entirely via the INTERMEDIATE segment "linked", which the
// build creates as a symlink pointing OUTSIDE SourceDir at a directory
// containing a real secret. OutputSubpath itself never leaves SourceDir
// syntactically (isWithinDir's string math passes); only a filesystem-
// aware resolution catches this.
func TestXB3_1_HostDirect_IntermediateSymlinkArtifactExfilBlocked(t *testing.T) {
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "id_ed25519")
	const secretContent = "SUPER-SECRET-PRIVATE-KEY-MATERIAL-XB3-1"
	require.NoError(t, os.WriteFile(secret, []byte(secretContent), 0o600))

	srcDir := t.TempDir()
	outDir := t.TempDir()

	sub := "desktopApp/build/linked/id_ed25519"
	buildCmd := "mkdir -p desktopApp/build && ln -s " + outsideDir + " desktopApp/build/linked"

	be := NewHostDirectBackend()
	res := be.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  buildCmd,
		OutputSubpath: sub,
		HostOutputDir: outDir,
		Timeout:       30 * time.Second,
	})

	require.Error(t, res.Error,
		"an OutputSubpath reachable only via an INTERMEDIATE symlink escaping SourceDir must be refused, "+
			"even though its final path component is an ordinary (non-symlink) file name")
	assert.Contains(t, res.Error.Error(), "could not be resolved safely")
	assert.Empty(t, res.ArtifactPath, "no artifact path on rejection")

	copied := filepath.Join(outDir, filepath.Base(sub))
	leaked := false
	if b, err := os.ReadFile(copied); err == nil && strings.Contains(string(b), secretContent) {
		leaked = true
	}
	assert.False(t, leaked, "secret must NOT be exfiltrated into HostOutputDir via an intermediate symlink (XB3-1)")

	// Belt-and-braces: the secret content must not appear ANYWHERE under
	// HostOutputDir.
	_ = filepath.WalkDir(outDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil {
			assert.NotContains(t, string(b), secretContent,
				"secret must not leak into any file under HostOutputDir")
		}
		return nil
	})
}

// TestXB3_1_HostDirect_IntermediateSymlinkWithinSourceDirStillWorks is
// the anti-regression companion: a build that uses an intermediate
// symlink whose target stays WITHIN SourceDir (a common, legitimate
// pattern — e.g. "build/current -> ../releases/v1.2.3") must still
// produce and copy the artifact exactly as before this fix.
func TestXB3_1_HostDirect_IntermediateSymlinkWithinSourceDirStillWorks(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	sub := "desktopApp/build/linked/artifact.bin"
	buildCmd := "mkdir -p desktopApp/real_build desktopApp/build && " +
		"ln -s ../real_build desktopApp/build/linked && " +
		"printf 'REALARTIFACT' > desktopApp/real_build/artifact.bin"

	be := NewHostDirectBackend()
	res := be.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  buildCmd,
		OutputSubpath: sub,
		HostOutputDir: outDir,
		Timeout:       30 * time.Second,
	})

	require.NoError(t, res.Error,
		"a legitimate intermediate symlink whose target stays WITHIN SourceDir must still work")
	require.NotEmpty(t, res.ArtifactPath)
	content, err := os.ReadFile(res.ArtifactPath)
	require.NoError(t, err)
	assert.Equal(t, "REALARTIFACT", string(content))
}

// ---------------------------------------------------------------------
// XB3-2 — apple-container ImageExists process-group kill on ctx
// timeout, driven through the REAL realAppleContainerRunner via a fake
// `container` binary shimmed onto PATH.
// ---------------------------------------------------------------------

func TestXB3_2_AppleContainerImageExists_KillsWholeProcessGroupOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XB3-2: process-group kill (SysProcAttr.Setpgid + negative-pid SIGKILL) is Unix-only; " +
			"configureProcessGroup is a documented no-op on this GOOS (§11.4.3 topology SKIP)")
	}

	tmpBin := t.TempDir()
	marker := filepath.Join(tmpBin, "xb3-2-grandchild-marker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"image\" ] && [ \"$2\" = \"list\" ]; then\n" +
		"  (sleep 5; touch " + marker + ") & disown\n" +
		"  sleep 30\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpBin, "container"), []byte(script), 0o755))
	t.Setenv("PATH", tmpBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	got := realAppleContainerRunner{}.ImageExists(ctx, "docker.io/library/alpine:latest")
	elapsed := time.Since(start)

	assert.False(t, got, "a timed-out `container image list` must not report the image as present")
	if elapsed > 4*time.Second {
		t.Fatalf("ImageExists took %s to return after a 1s ctx timeout (WaitDelay=%s) — expected it back "+
			"within a few seconds; a hang this long means Wait() is stuck draining a pipe a surviving "+
			"descendant still holds open — exactly the XB3-2 regression", elapsed, subprocessWaitDelay)
	}

	time.Sleep(6 * time.Second)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("grandchild marker %q EXISTS — the backgrounded/disowned descendant of `container image "+
			"list` survived past ImageExists' ctx timeout; the WHOLE process group must be killed, not just "+
			"the direct child (XB3-2)", marker)
	}
}

// ---------------------------------------------------------------------
// XB3-3 — podman/docker ImageExists process-group configuration,
// driven through the REAL realContainerRunner via a fake `podman`
// binary shimmed onto PATH.
// ---------------------------------------------------------------------

func TestXB3_3_ContainerRunnerImageExists_KillsWholeProcessGroupOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XB3-3: process-group kill (SysProcAttr.Setpgid + negative-pid SIGKILL) is Unix-only; " +
			"configureProcessGroup is a documented no-op on this GOOS (§11.4.3 topology SKIP)")
	}

	tmpBin := t.TempDir()
	marker := filepath.Join(tmpBin, "xb3-3-grandchild-marker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"image\" ] && [ \"$2\" = \"exists\" ]; then\n" +
		"  (sleep 5; touch " + marker + ") & disown\n" +
		"  sleep 30\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpBin, "podman"), []byte(script), 0o755))
	t.Setenv("PATH", tmpBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	got := realContainerRunner{}.ImageExists(ctx, "some/image:latest")
	elapsed := time.Since(start)

	assert.False(t, got, "a timed-out `podman image exists` must not report the image as present")
	if elapsed > 4*time.Second {
		t.Fatalf("ImageExists took %s to return after a 1s ctx timeout (WaitDelay=%s) — expected it back "+
			"within a few seconds; a hang this long means Wait() is stuck draining a pipe a surviving "+
			"descendant still holds open, or ctx cancellation only killed the direct child, leaving the "+
			"descendant orphaned (XB3-3)", elapsed, subprocessWaitDelay)
	}

	time.Sleep(6 * time.Second)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("grandchild marker %q EXISTS — the backgrounded/disowned descendant of `podman image "+
			"exists` survived past ImageExists' ctx timeout; the WHOLE process group must be killed, not "+
			"just the direct child (XB3-3)", marker)
	}
}

// ---------------------------------------------------------------------
// XB3-4 — mountFlagRejected line-scoping + Run() diagnostics
// preservation on a fallback failure.
// ---------------------------------------------------------------------

// TestXB3_4_MountFlagRejected_LineScoped_NoCrossLineFalsePositive proves
// the tightened predicate still catches genuine single-line rejection
// messages (matching the pre-existing fixtures in apple_container_test.go)
// while no longer false-positiving on "mount" and a rejection word that
// merely co-occur SOMEWHERE in the buffer on unrelated lines.
func TestXB3_4_MountFlagRejected_LineScoped_NoCrossLineFalsePositive(t *testing.T) {
	// Pre-existing genuine-rejection fixtures must still match.
	assert.True(t, mountFlagRejected(bytes.NewBufferString("Error: unknown flag --mount")))
	assert.True(t, mountFlagRejected(bytes.NewBufferString("unsupported mount type")))
	assert.False(t, mountFlagRejected(bytes.NewBufferString("make: *** No rule to make target")))
	assert.False(t, mountFlagRejected(nil))

	// XB3-4 false positive: "mount" and "invalid" both appear SOMEWHERE
	// in the buffer but on SEPARATE, unrelated lines — a benign "mount
	// namespace setup complete" info line followed, much later, by an
	// unrelated in-container "invalid configuration" error that has
	// nothing to do with the --mount CLI flag. The old whole-buffer
	// scan wrongly matched this; the line-scoped predicate must not.
	falsePositive := bytes.NewBufferString(
		"mount namespace setup complete\n" +
			"starting build...\n" +
			"invalid configuration: missing GRADLE_HOME\n")
	assert.False(t, mountFlagRejected(falsePositive),
		"co-occurrence of 'mount' and 'invalid' on SEPARATE, unrelated lines must NOT trigger a "+
			"mount-flag-rejection retry (XB3-4)")

	// Anti-regression: the SAME words on the SAME line must still match.
	sameLine := bytes.NewBufferString("Error: mount flag invalid for this container version\n")
	assert.True(t, mountFlagRejected(sameLine),
		"'mount' and a rejection word co-occurring on the SAME line must still be treated as a genuine rejection")
}

// TestXB3_4_RealAppleContainerRun_PreservesFirstAttemptDiagnosticsOnFallbackFailure
// drives the REAL realAppleContainerRunner.Run (no mocks) against a fake
// `container` binary that rejects the virtiofs (--mount) attempt with a
// genuine mount-flag-rejection message, then genuinely fails the
// fallback volume (-v) attempt with a DIFFERENT, unrelated error. Both
// attempts' diagnostics must survive into the final spec.Stderr — proof
// that Run() no longer destroys the first attempt's real diagnostics via
// a blind Reset (XB3-4).
func TestXB3_4_RealAppleContainerRun_PreservesFirstAttemptDiagnosticsOnFallbackFailure(t *testing.T) {
	tmpBin := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"run\" ]; then\n" +
		"  shift\n" +
		"  has_mount=0\n" +
		"  has_dashv=0\n" +
		"  for arg in \"$@\"; do\n" +
		"    case \"$arg\" in\n" +
		"      --mount) has_mount=1 ;;\n" +
		"      -v) has_dashv=1 ;;\n" +
		"    esac\n" +
		"  done\n" +
		"  if [ \"$has_mount\" = \"1\" ]; then\n" +
		"    echo \"Error: unknown flag --mount\" >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"  if [ \"$has_dashv\" = \"1\" ]; then\n" +
		"    echo \"boom: real second failure - build script genuinely broken\" >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpBin, "container"), []byte(script), 0o755))
	t.Setenv("PATH", tmpBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	spec := appleContainerRunSpec{
		Image:       "alpine",
		MountSource: "/host/src",
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     "true",
		Stdout:      &stdout,
		Stderr:      &stderr,
	}

	exitCode, err := realAppleContainerRunner{}.Run(context.Background(), spec)

	require.Error(t, err, "both the rejected --mount attempt and the genuinely-failing -v fallback must "+
		"surface as an overall Run() failure")
	assert.NotEqual(t, 0, exitCode)
	assert.Contains(t, stderr.String(), "unknown flag --mount",
		"the FIRST (rejected --mount) attempt's real diagnostics must survive into the final buffer (XB3-4)")
	assert.Contains(t, stderr.String(), "boom: real second failure",
		"the SECOND (fallback -v) attempt's genuine diagnostics must also be present")
}

// ---------------------------------------------------------------------
// XB3-5 — bounded output capture: memory stays bounded + tail is
// correct, both for the wrapper in isolation and end-to-end through the
// REAL HostDirectBackend.
// ---------------------------------------------------------------------

// TestXB3_5_BoundedBufferWriter_CapsMemoryRetainsCorrectTail unit-tests
// boundedBufferWriter directly: writing far more than its budget must
// leave the backing bytes.Buffer's capacity BOUNDED (not growing
// linearly with total bytes ever written) while retaining EXACTLY the
// tail of what was written.
func TestXB3_5_BoundedBufferWriter_CapsMemoryRetainsCorrectTail(t *testing.T) {
	var buf bytes.Buffer
	w := &boundedBufferWriter{buf: &buf, budget: 1000}

	chunk := bytes.Repeat([]byte("A"), 4096)
	var totalWritten int
	const iterations = 500 // ~2 MiB total, far beyond the 1000-byte budget
	var lastMarker string
	for i := 0; i < iterations; i++ {
		// Write the filler chunk FIRST, then a small per-iteration
		// marker LAST — since the chunk (4096 bytes) alone already
		// exceeds the whole budget, only bytes written AFTER it within
		// the same iteration can possibly still be present once the
		// loop ends; this lets the final iteration's marker serve as
		// an unambiguous "most recently written bytes" oracle.
		n1, err1 := w.Write(chunk)
		require.NoError(t, err1)
		assert.Equal(t, len(chunk), n1)
		lastMarker = fmt.Sprintf("[iter%05d]", i)
		n2, err2 := w.Write([]byte(lastMarker))
		require.NoError(t, err2)
		assert.Equal(t, len(lastMarker), n2)
		totalWritten += len(chunk) + len(lastMarker)
	}

	assert.Equal(t, 1000, buf.Len(),
		"retained content must be capped to the budget regardless of the %d total bytes written", totalWritten)
	assert.Less(t, cap(buf.Bytes()), 20000,
		"backing array capacity must stay BOUNDED (not grow linearly with total bytes written) — a "+
			"flooding build tool must not be able to exhaust host memory over a 30-45 min build (XB3-5)")

	assert.True(t, bytes.HasSuffix(buf.Bytes(), []byte(lastMarker)),
		"the retained tail must end with the LAST write's marker, not stale/earlier content")
}

// TestXB3_5_HostDirect_BoundedOutputCaptureStillProducesCorrectTail
// drives the REAL production path (NewHostDirectBackend -> realRunner.Run
// -> boundedBufferWriter) with a build command that floods stdout with
// MORE than maxCapturedOutputBytes before emitting a final marker line
// and producing its artifact. Proves the wiring doesn't corrupt or lose
// the meaningful tail even though the underlying buffer is now capped.
func TestXB3_5_HostDirect_BoundedOutputCaptureStillProducesCorrectTail(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	const subpath = "artifact.bin"

	require.Greater(t, 1500000, maxCapturedOutputBytes,
		"test precondition: the flood must exceed maxCapturedOutputBytes to actually exercise the cap")

	buildCmd := "head -c 1500000 /dev/zero | tr '\\0' 'X'; echo; echo FINAL-MARKER-XB3-5; " +
		"printf ARTIFACT > " + subpath

	be := NewHostDirectBackend()
	res := be.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  buildCmd,
		OutputSubpath: subpath,
		HostOutputDir: outDir,
		Timeout:       30 * time.Second,
	})

	require.NoError(t, res.Error)
	assert.Contains(t, res.StdoutTail, "FINAL-MARKER-XB3-5",
		"the tail of a flooding build's stdout must still surface the LAST meaningful output through the "+
			"bounded writer (XB3-5)")
	assert.LessOrEqual(t, len(res.StdoutTail), 4096+len("…"),
		"BuildResult.StdoutTail must still respect its existing 4 KiB display truncation")
	assert.Equal(t, int64(len("ARTIFACT")), res.ArtifactSize)
}
