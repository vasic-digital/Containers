package crossbuild

// Wave-20 XBUILD2-HARD — §11.4.118 fresh discovery-pressure audit of
// pkg/crossbuild, four findings fixed + guarded here:
//
//   - XB2-1 (HIGH):   realRunner / realContainerRunner / realAppleContainerRunner
//     set no cmd.WaitDelay + kill only the direct child on ctx
//     cancellation, so a wedged build whose descendants inherited the
//     stdout/stderr pipes can hang PAST the declared Timeout and leave
//     orphaned processes running. Fixed in host_direct.go,
//     container_runner.go, apple_container.go via cmd.WaitDelay +
//     configureProcessGroup (subprocess_group_{unix,other}.go).
//   - XB2-2 (MED, security): copyDir recreated a nested symlink
//     verbatim without checking whether its target escapes the
//     artifact tree. Fixed in host_direct.go's copyDir (now threads
//     the artifact root through the recursion and refuses an
//     escaping/absolute nested-symlink target, mirroring the existing
//     top-level-symlink-artifact policy in copyFile).
//   - XB2-3 (MED, §11.4.14): copyRegularFile/copyDir left partial
//     debris in HostOutputDir on a mid-copy failure. Fixed in
//     host_direct.go's copyFile: any copy error now best-effort
//     removes the destination before returning.
//   - XB2-4 (MED, -race): Selector.backends was an unsynchronized
//     slice — concurrent Register vs Choose/SupportedTargets is a
//     genuine data race. Fixed in selector.go via sync.RWMutex.
//
// Follow-ups flagged but NOT implemented in this wave (reported to the
// conductor): XB2-5 (selinuxEnabled bare-var seam fragility, LOW),
// XB2-6 (misleading exit=0 on launch-failure, shared with pkg/runtime,
// LOW), XB2-7 (hardcoded default image refs, already mitigated by
// constructor override, LOW/advisory).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// XB2-1 — process-group kill on ctx timeout, driven through the REAL
// production runner (NewHostDirectBackend, not a fake).
// ---------------------------------------------------------------------

// TestXB2_1_HostDirect_KillsWholeProcessGroupOnTimeout is the §11.4.115
// committed guard for XB2-1. It drives the REAL realRunner (via
// NewHostDirectBackend — the existing orchestration tests all inject a
// fakeRunner that never touches os/exec, so none of them would have
// caught this) with a BuildCommand that backgrounds-and-disowns a
// grandchild sleeping 5s before touching a marker file, then sleeps 30s
// itself. Timeout is 1s.
//
// Asserts BOTH observable symptoms of the finding:
//  1. Build() returns promptly (within a few seconds of the 1s
//     Timeout+WaitDelay), never anywhere near the BuildCommand's own
//     `sleep 30` — proving Wait() is not wedged on a pipe that a
//     surviving descendant still holds open.
//  2. The backgrounded grandchild's marker file does NOT appear even
//     after waiting comfortably past its own 5s sleep — proving the
//     WHOLE process group was killed, not just the direct "sh" child.
//
// Process-group kill semantics (SysProcAttr.Setpgid + a negative-pid
// SIGKILL) are Unix-only; on a host where they do not apply the test
// is SKIPPED with a clear reason rather than fake-passing (§11.4.3).
func TestXB2_1_HostDirect_KillsWholeProcessGroupOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XB2-1: process-group kill (SysProcAttr.Setpgid + negative-pid SIGKILL) is Unix-only; " +
			"configureProcessGroup is a documented no-op on this GOOS (§11.4.3 topology SKIP)")
	}

	srcDir := t.TempDir()
	outDir := t.TempDir()
	marker := filepath.Join(srcDir, "xb2-1-grandchild-marker")

	be := NewHostDirectBackend()

	start := time.Now()
	result := be.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "(sleep 5; touch " + marker + ") & disown; sleep 30",
		OutputSubpath: "unused-artifact.bin", // never produced; ctx times out first
		HostOutputDir: outDir,
		Timeout:       1 * time.Second,
	})
	elapsed := time.Since(start)

	require.Error(t, result.Error, "a Timeout-exceeding build must surface as a Build error")

	if elapsed > 4*time.Second {
		t.Fatalf("Build() took %s to return after a 1s Timeout (WaitDelay=%s) — expected it back within a few "+
			"seconds, nowhere near the BuildCommand's own `sleep 30`; a hang this long means Wait() is stuck "+
			"draining a pipe a surviving descendant still holds open — exactly the XB2-1 regression", elapsed, subprocessWaitDelay)
	}

	// Give the grandchild's own 5s sleep comfortable room to have
	// completed (and touched the marker) IF it had survived the kill.
	time.Sleep(6 * time.Second)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("grandchild marker %q EXISTS — the backgrounded/disowned descendant survived past the killed "+
			"build command; the WHOLE process group must be killed on ctx timeout, not just the direct child", marker)
	}
}

// ---------------------------------------------------------------------
// XB2-2 — nested directory-artifact symlink escaping the artifact root
// must be refused, not recreated verbatim.
// ---------------------------------------------------------------------

// escapingSymlinkDirRunner simulates a build (via the containerRunner
// seam used by LinuxContainerBackend) whose directory artifact contains
// a NESTED symlink pointing OUTSIDE the artifact root.
type escapingSymlinkDirRunner struct {
	imageExists bool
	dirPath     string
	evilTarget  string
}

func (r *escapingSymlinkDirRunner) ImageExists(ctx context.Context, imageRef string) bool {
	return r.imageExists
}

func (r *escapingSymlinkDirRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	if err := os.MkdirAll(r.dirPath, 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(r.dirPath, "bin_app"), []byte("ELF..."), 0o755); err != nil {
		return -1, err
	}
	if err := os.Symlink(r.evilTarget, filepath.Join(r.dirPath, "evil_link")); err != nil {
		return -1, err
	}
	return 0, nil
}

// TestXB2_2_CopyDir_NestedSymlinkEscapeRefused is the §11.4.115
// committed guard for XB2-2. A directory app-image containing a nested
// symlink whose target resolves OUTSIDE the artifact root must be
// refused wholesale (fail closed, matching the existing top-level
// symlink-artifact policy in copyFile) rather than recreated verbatim
// in HostOutputDir — recreating it would ship a link that resolves to
// an arbitrary host path to whatever consumes the artifact next.
func TestXB2_2_CopyDir_NestedSymlinkEscapeRefused(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "outside-secret.txt")
	const secretContent = "OUTSIDE-THE-ARTIFACT-ROOT-MUST-NEVER-BE-LINKED-TO"
	require.NoError(t, os.WriteFile(secret, []byte(secretContent), 0o600))

	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp"
	dirPath := filepath.Join(srcDir, subpath)

	runner := &escapingSymlinkDirRunner{imageExists: true, dirPath: dirPath, evilTarget: secret}
	l := newLinuxContainerBackendWithRunner("test/linux-image", "amd64", runner)

	result := l.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "amd64"},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error,
		"a directory artifact whose nested symlink escapes the artifact root must be refused, not recreated")
	assert.Contains(t, result.Error.Error(), "escapes the artifact root")
	assert.Empty(t, result.ArtifactPath, "no artifact path on rejection")

	// Belt-and-braces: no symlink anywhere under HostOutputDir may
	// point at the outside secret — whether via the evil_link entry
	// itself (must not exist at all, since the whole copy is refused)
	// or leaked some other way.
	_ = filepath.WalkDir(outDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if target, rerr := os.Readlink(path); rerr == nil {
				assert.NotEqual(t, secret, target,
					"an escaping symlink must never be recreated anywhere in HostOutputDir")
			}
		}
		return nil
	})
}

// Anti-regression companion: a nested symlink whose target stays
// WITHIN the artifact root must still be copied exactly as before.
func TestXB2_2_CopyDir_NestedSymlinkWithinRootStillWorks(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp"
	dirPath := filepath.Join(srcDir, subpath)

	runner := &internalSymlinkDirRunner{imageExists: true, dirPath: dirPath}
	l := newLinuxContainerBackendWithRunner("test/linux-image", "amd64", runner)

	result := l.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "amd64"},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error, "a nested symlink whose target stays within the artifact root must still work")
	require.NotEmpty(t, result.ArtifactPath)
	linkPath := filepath.Join(result.ArtifactPath, "lib", "app.so")
	target, err := os.Readlink(linkPath)
	require.NoError(t, err, "the legitimate internal symlink must be recreated")
	assert.Equal(t, "../real_app.so", target)
}

// internalSymlinkDirRunner produces a directory artifact whose nested
// symlink target stays WITHIN the artifact root (a common real-world
// pattern: lib/app.so -> ../real_app.so).
type internalSymlinkDirRunner struct {
	imageExists bool
	dirPath     string
}

func (r *internalSymlinkDirRunner) ImageExists(ctx context.Context, imageRef string) bool {
	return r.imageExists
}

func (r *internalSymlinkDirRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	if err := os.MkdirAll(filepath.Join(r.dirPath, "lib"), 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(r.dirPath, "real_app.so"), []byte("ELF..."), 0o755); err != nil {
		return -1, err
	}
	if err := os.Symlink("../real_app.so", filepath.Join(r.dirPath, "lib", "app.so")); err != nil {
		return -1, err
	}
	return 0, nil
}

// ---------------------------------------------------------------------
// XB2-3 — a mid-copy failure must leave no debris under HostOutputDir.
// ---------------------------------------------------------------------

// xb23FailingDirRunner produces a directory artifact with two entries:
// "aaa_first_copied" sorts first (os.ReadDir returns entries sorted by
// name) so copyDir copies it successfully BEFORE reaching
// "zzz_second_will_fail", whose copy is made to fail by the test
// pre-creating a DIRECTORY at that exact destination path (a
// directory-vs-regular-file collision — os.OpenFile(O_WRONLY|...) on
// an existing directory fails with EISDIR regardless of whether the
// test process runs as root, unlike a permission-bit-based collision
// which root would simply bypass).
type xb23FailingDirRunner struct {
	dirPath string
}

func (r *xb23FailingDirRunner) Run(ctx context.Context, dir, command string, env map[string]string,
	stdout, stderr *bytes.Buffer) (int, error) {
	if err := os.MkdirAll(r.dirPath, 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(r.dirPath, "aaa_first_copied"), []byte("FIRST"), 0o644); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(r.dirPath, "zzz_second_will_fail"), []byte("SECOND"), 0o644); err != nil {
		return -1, err
	}
	return 0, nil
}

// TestXB2_3_CopyDir_MidCopyFailureLeavesNoDebris is the §11.4.115
// committed guard for XB2-3.
func TestXB2_3_CopyDir_MidCopyFailureLeavesNoDebris(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp"
	dirPath := filepath.Join(srcDir, subpath)

	runner := &xb23FailingDirRunner{dirPath: dirPath}
	hd := newHostDirectBackendWithRunner(runner)

	// Pre-create a DIRECTORY at the destination the second artifact
	// entry (a regular file) would otherwise be copied to, forcing
	// copyRegularFile to fail partway through copyDir's walk — AFTER
	// "aaa_first_copied" has already been copied successfully.
	artifactDstRoot := filepath.Join(outDir, filepath.Base(dirPath))
	require.NoError(t, os.MkdirAll(filepath.Join(artifactDstRoot, "zzz_second_will_fail"), 0o755))

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error, "a mid-copy failure on the second entry must surface as a Build error")
	assert.Empty(t, result.ArtifactPath, "no artifact path on a failed copy")

	_, statErr := os.Stat(filepath.Join(artifactDstRoot, "aaa_first_copied"))
	assert.True(t, os.IsNotExist(statErr),
		"the earlier-copied entry must be cleaned up after a mid-copy failure, got stat err=%v", statErr)
	_, rootStatErr := os.Stat(artifactDstRoot)
	assert.True(t, os.IsNotExist(rootStatErr),
		"the whole partially-built artifact tree must be removed after a mid-copy failure, got stat err=%v", rootStatErr)
}

// ---------------------------------------------------------------------
// XB2-4 — Selector.backends must be race-free under concurrent
// Register vs Choose/SupportedTargets.
// ---------------------------------------------------------------------

// TestXB2_4_Selector_ConcurrentRegisterAndChooseNoRace is the §11.4.115
// committed guard for XB2-4. Run under `go test -race`, this exercises
// concurrent Register (append) against concurrent Choose +
// SupportedTargets (range) for ~200ms; the race detector — not a
// testify assertion — is the oracle here: an unguarded backends slice
// makes `go test -race` report a DATA RACE and fail the binary.
func TestXB2_4_Selector_ConcurrentRegisterAndChooseNoRace(t *testing.T) {
	s := NewSelectorForHost("linux", "amd64")
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.Register(&fakeBackend{
					name: "concurrent",
					caps: Capabilities{SupportsTargets: []Target{{OS: "linux", Arch: "amd64"}}},
				})
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.Choose(BuildRequest{Target: Target{OS: "linux", Arch: "amd64"}})
				_ = s.SupportedTargets()
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
