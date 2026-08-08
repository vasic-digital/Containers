package crossbuild

// Wave-20 XBUILD2 — §11.4.118 DEEPER (2nd-pass) discovery-pressure audit
// of pkg/crossbuild under a SECURITY lens. Three NEW findings the first
// pass (XBUILD-F1/F3, XB2-1..4, XB3-1..5) did not cover, each fixed +
// guarded here:
//
//   - XBUILD2-1 (MED, §11.4.108/§11.4.1 anti-bluff): verifyFreshArtifact's
//     zero-byte check is meaningful only for a single-FILE artifact. For a
//     DIRECTORY artifact os.Stat reports the directory INODE's own size
//     (non-zero — 40-80 bytes here even for an empty dir), so a build that
//     reports SUCCESS but drops an EMPTY directory (or one whose only
//     entries are zero-byte files / empty subdirs) was reported as a
//     genuine build — a "BUILD SUCCESSFUL but nothing really built" bluff.
//     Fixed in host_direct.go: verifyFreshArtifact now walks a directory
//     artifact and requires ≥1 byte of real regular-file content.
//
//   - XBUILD2-2 (MED, SECURITY argv-injection): both argv builders
//     (container_runner.go realContainerRunner.Run for podman/docker and
//     apple_container.go buildRunArgs for Apple `container`) placed
//     spec.Image as a BARE POSITIONAL with no "--" end-of-options marker.
//     spec.Image flows from a backend imageRef OR, via RunInLinuxContainer,
//     straight from a caller-supplied req.Image; a crafted reference
//     beginning with '-' ("--privileged", "--network=host", "-v/:/host")
//     was parseable by the CLI as a RUNTIME FLAG rather than the image
//     name — a privilege/mount/network-escalation vector. Fixed: an
//     explicit "--" now precedes the image positional in both builders
//     (mirror of the module's OR-1/RM2/VOL2/EG2 arg-injection hardenings).
//
//   - XBUILD2-3 (MED, SECURITY DoS): copyDir's `default` branch treated
//     every non-symlink, non-directory entry of a directory artifact as a
//     regular file and copyRegularFile'd it. A nested FIFO/named-pipe with
//     no writer makes os.Open BLOCK FOREVER (the artifact-copy step is not
//     ctx-bounded, so req.Timeout does not save it), wedging the whole
//     build past its declared deadline; a device/socket copies garbage.
//     Fixed in host_direct.go's copyDir: non-regular special-file entries
//     are refused (fail closed), matching copyFile's symlink policy.

import (
	"bytes"
	"context"
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
// XBUILD2-1 — empty-content directory artifact must be rejected as a
// bluff, driven end-to-end through LinuxContainerBackend's real
// verifyFreshArtifact/copyFile choke-point via the injected runner seam.
// ---------------------------------------------------------------------

// xb2EmptyContentDirRunner simulates a build (via the containerRunner
// seam LinuxContainerBackend uses) that reports SUCCESS but drops a
// DIRECTORY artifact WITH entries (so os.Stat(dir).Size() is non-zero,
// exactly like a real jpackage app-image directory) yet ZERO bytes of
// real file content: one empty subdirectory + one zero-byte file. That
// is the bluff XBUILD2-1 catches.
type xb2EmptyContentDirRunner struct {
	imageExists bool
	dirPath     string
}

func (r *xb2EmptyContentDirRunner) ImageExists(ctx context.Context, imageRef string) bool {
	return r.imageExists
}

func (r *xb2EmptyContentDirRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	if err := os.MkdirAll(filepath.Join(r.dirPath, "lib"), 0o755); err != nil {
		return -1, err
	}
	// A ZERO-byte file: the directory now HAS entries (non-zero inode
	// size) but zero bytes of real content.
	if err := os.WriteFile(filepath.Join(r.dirPath, "bin_app"), []byte{}, 0o755); err != nil {
		return -1, err
	}
	return 0, nil
}

func TestWave20_XBUILD2_EmptyDirectoryArtifactRejectedAsBluff(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp" // a directory app-image
	dirPath := filepath.Join(srcDir, subpath)

	runner := &xb2EmptyContentDirRunner{imageExists: true, dirPath: dirPath}
	l := newLinuxContainerBackendWithRunner("test/linux-image", "amd64", runner)

	result := l.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "amd64"},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error,
		"a directory artifact with entries but ZERO bytes of real file content is a "+
			"'BUILD SUCCESSFUL but nothing built' bluff and MUST be rejected — os.Stat(dir).Size() "+
			"being non-zero (the inode's own size) must NOT be accepted as proof of a real artifact")
	assert.Contains(t, result.Error.Error(), "zero bytes of real file content")
	assert.Empty(t, result.ArtifactPath, "no artifact path on rejection")
	assert.Zero(t, result.ArtifactSize, "no artifact size on rejection")

	// Belt-and-braces: nothing was copied into HostOutputDir.
	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "an empty-content directory artifact must not be copied to HostOutputDir")
}

// Anti-regression companion: a directory artifact holding at least one
// byte of real regular-file content must still succeed exactly as before.
func TestWave20_XBUILD2_DirectoryArtifactWithRealContentStillSucceeds(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp"
	dirPath := filepath.Join(srcDir, subpath)

	runner := &xb2WithContentDirRunner{imageExists: true, dirPath: dirPath}
	l := newLinuxContainerBackendWithRunner("test/linux-image", "amd64", runner)

	result := l.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "amd64"},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error,
		"a directory artifact with real, non-zero regular-file content must still be a valid build")
	require.NotEmpty(t, result.ArtifactPath)
	st, err := os.Stat(result.ArtifactPath)
	require.NoError(t, err)
	assert.True(t, st.IsDir(), "directory artifact must be copied as a directory")
	body, err := os.ReadFile(filepath.Join(result.ArtifactPath, "bin_app"))
	require.NoError(t, err)
	assert.Equal(t, "ELF-REAL", string(body))
}

type xb2WithContentDirRunner struct {
	imageExists bool
	dirPath     string
}

func (r *xb2WithContentDirRunner) ImageExists(ctx context.Context, imageRef string) bool {
	return r.imageExists
}

func (r *xb2WithContentDirRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	if err := os.MkdirAll(filepath.Join(r.dirPath, "lib"), 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(r.dirPath, "bin_app"), []byte("ELF-REAL"), 0o755); err != nil {
		return -1, err
	}
	return 0, nil
}

// ---------------------------------------------------------------------
// XBUILD2-2 — image-ref argv flag-injection: the image positional MUST be
// preceded by an end-of-options "--" in BOTH argv builders.
// ---------------------------------------------------------------------

// argIndex returns the index of the first occurrence of want in args, or
// -1 if absent.
func argIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestWave20_XBUILD2_AppleRunArgs_ImageBehindEndOfOptions proves the Apple
// `container run` argv builder places an explicit "--" immediately before
// the image positional so a crafted reference beginning with '-' can never
// be parsed as a runtime flag. Pure function — no live engine.
func TestWave20_XBUILD2_AppleRunArgs_ImageBehindEndOfOptions(t *testing.T) {
	crafted := []string{
		"docker.io/library/alpine:latest", // ordinary ref
		"-crafted-injected-image-xb2",     // hostile ref beginning with '-'
		"--privileged",                    // would be a runtime flag if not guarded
	}
	for _, img := range crafted {
		for _, mode := range []string{"virtiofs", "volume"} {
			spec := appleContainerRunSpec{
				Image:       img,
				MountSource: "/host/src",
				MountTarget: "/work/src",
				WorkDir:     "/work/src",
				Command:     "true",
			}
			args := buildRunArgs(spec, mode)

			idx := argIndex(args, img)
			require.GreaterOrEqual(t, idx, 1, "image %q must appear as a positional in argv (mode=%s)", img, mode)
			assert.Equal(t, "--", args[idx-1],
				"the image positional %q MUST be immediately preceded by an end-of-options '--' "+
					"so a crafted image ref beginning with '-' can never be parsed as a runtime flag "+
					"(mode=%s)", img, mode)
			assert.Equal(t, 1, countArg(args, "--"),
				"exactly one end-of-options '--' must appear (mode=%s img=%q)", mode, img)
		}
	}
}

func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

// TestWave20_XBUILD2_ContainerRunArgs_ImageBehindEndOfOptions drives the
// REAL realContainerRunner.Run (no arg-builder mock) against a fake
// `podman` binary shimmed onto PATH that records the exact argv it was
// handed. The recorded argv MUST place "--" immediately before the image
// positional. Unix-only (shell shim).
func TestWave20_XBUILD2_ContainerRunArgs_ImageBehindEndOfOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XBUILD2-2: the argv-recording shim is a POSIX shell script; skipping on Windows (§11.4.3 topology SKIP)")
	}

	tmpBin := t.TempDir()
	marker := filepath.Join(tmpBin, "recorded-argv")
	// Record every arg (after the `run` subcommand) one-per-line, then
	// exit 0 so realContainerRunner.Run reports success.
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" = \"run\" ]; then\n" +
		"  shift\n" +
		"  printf '%s\\n' \"$@\" > " + marker + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpBin, "podman"), []byte(shim), 0o755))
	t.Setenv("PATH", tmpBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const craftedImage = "-crafted-injected-image-xb2"
	var stdout, stderr bytes.Buffer
	exit, err := realContainerRunner{}.Run(context.Background(), containerRunSpec{
		Image:       craftedImage,
		MountSource: "/host/src",
		MountTarget: "/work/src",
		WorkDir:     "/work/src",
		Command:     "true",
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, exit)

	raw, err := os.ReadFile(marker)
	require.NoError(t, err, "the shim must have recorded the argv it was handed")
	var recorded []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			recorded = append(recorded, line)
		}
	}

	idx := argIndex(recorded, craftedImage)
	require.GreaterOrEqual(t, idx, 1, "the crafted image ref must appear in the recorded podman argv: %v", recorded)
	assert.Equal(t, "--", recorded[idx-1],
		"realContainerRunner.Run MUST place an end-of-options '--' immediately before the image "+
			"positional so a crafted image ref beginning with '-' is never parsed by podman/docker as a "+
			"runtime flag (recorded argv: %v)", recorded)
}

// ---------------------------------------------------------------------
// XBUILD2-3 — a nested FIFO/special-file inside a directory artifact must
// be REFUSED, not opened (opening a named pipe with no writer blocks the
// artifact copy forever). Driven end-to-end through the REAL
// HostDirectBackend (real `sh -c` runner + `mkfifo`) — no mocks.
// ---------------------------------------------------------------------

func TestWave20_XBUILD2_NestedFifoArtifactRefusedNotHung(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XBUILD2-3: FIFO/named-pipe special files + `mkfifo` are a POSIX concept; " +
			"skipping on Windows (§11.4.3 topology SKIP)")
	}

	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "out/app" // a DIRECTORY artifact
	fifoPath := filepath.Join(srcDir, subpath, "pipe")

	// The build drops a directory artifact containing (a) a REAL non-empty
	// regular file (so the XBUILD2-1 empty-directory check passes and we
	// actually reach the copy walk) and (b) a nested FIFO. `real.bin`
	// sorts AFTER `pipe`, so copyDir reaches the FIFO first.
	buildCmd := "mkdir -p " + subpath + " && printf REAL > " + subpath + "/real.bin && mkfifo " + subpath + "/pipe"

	// If the fix regresses (or is reverted for the anti-tautology proof),
	// copyDir's os.Open on the writer-less FIFO blocks forever; unblock any
	// stuck reader at teardown so no test goroutine is wedged.
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifoPath, os.O_RDWR, 0); err == nil {
			_ = f.Close()
		}
	})

	be := NewHostDirectBackend()
	resCh := make(chan BuildResult, 1) // buffered: the goroutine can send even after we time out
	go func() {
		resCh <- be.Build(context.Background(), BuildRequest{
			Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
			SourceDir:     srcDir,
			BuildCommand:  buildCmd,
			OutputSubpath: subpath,
			HostOutputDir: outDir,
			Timeout:       30 * time.Second, // generous: the COPY step is NOT ctx-bounded, so this is not what saves us
		})
	}()

	select {
	case res := <-resCh:
		require.Error(t, res.Error,
			"a directory artifact containing a nested FIFO/special file must be REFUSED — copying it as a "+
				"regular file would open a named pipe with no writer and block the artifact-copy forever")
		assert.Contains(t, res.Error.Error(), "special file")
		assert.Empty(t, res.ArtifactPath, "no artifact path when the copy is refused")
		// Debris cleanup (XB2-3 companion): nothing partial under HostOutputDir.
		if st, statErr := os.Stat(filepath.Join(outDir, "app")); statErr == nil {
			t.Fatalf("refused-copy debris left under HostOutputDir: %s exists (%v)",
				filepath.Join(outDir, "app"), st.Mode())
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Build HUNG on a nested FIFO artifact entry — copyDir opened a named pipe as if it were a " +
			"regular file (os.Open on a writer-less FIFO blocks forever) instead of refusing it; the " +
			"artifact-copy step is not ctx-bounded, so req.Timeout does not save it (XBUILD2-3)")
	}
}
