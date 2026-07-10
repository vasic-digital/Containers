package crossbuild

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCTHardenXbuild1_StaleArtifact_RejectedAsBluff(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "build/out/artifact.bin"
	full := filepath.Join(srcDir, subpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	const staleContent = "STALE-FROM-A-PRIOR-BUILD-INVOCATION"
	require.NoError(t, os.WriteFile(full, []byte(staleContent), 0o644))

	// Runner simulates a build that reports SUCCESS but never
	// regenerates the artifact — producesPath intentionally unset.
	runner := &fakeRunner{exitCode: 0, stdout: "BUILD SUCCESSFUL\n"}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "true",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error,
		"a pre-existing artifact whose mtime+size are UNCHANGED after a 'successful' build "+
			"MUST be rejected as stale — reporting it as this build's fresh output is a bluff")
	assert.Contains(t, result.Error.Error(), "was NOT modified by this build")
	assert.Empty(t, result.ArtifactPath, "no artifact path on rejection")
}

// Anti-regression: a genuine overwrite (new content/size) must still succeed.
func TestCTHardenXbuild1_FreshOverwrite_StillSucceeds(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "build/out/artifact.bin"
	full := filepath.Join(srcDir, subpath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte("OLD"), 0o644))

	runner := &fakeRunner{
		exitCode:        0,
		producesPath:    full,
		producesContent: []byte("FRESH-REBUILT-CONTENT"),
	}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew rebuild",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error, "a build that genuinely rewrites its artifact must succeed")
	assert.Equal(t, int64(len("FRESH-REBUILT-CONTENT")), result.ArtifactSize)
}

// Anti-regression: the common "first build, nothing pre-existing" case
// (pre == nil) must never trigger the staleness rejection.
func TestCTHardenXbuild1_FirstBuild_NoPriorArtifact_StillSucceeds(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "build/out/artifact.bin"

	runner := &fakeRunner{
		exitCode:        0,
		producesPath:    filepath.Join(srcDir, subpath),
		producesContent: []byte("FIRST-BUILD"),
	}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew assemble",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error, "a first build with no pre-existing artifact must succeed")
	assert.Equal(t, int64(len("FIRST-BUILD")), result.ArtifactSize)
}
