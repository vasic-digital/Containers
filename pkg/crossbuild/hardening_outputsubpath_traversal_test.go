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

func TestCTHardenXbuild2_OutputSubpath_RejectsPathTraversal(t *testing.T) {
	parent := t.TempDir()
	srcDir := filepath.Join(parent, "project")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	outDir := t.TempDir()

	secretPath := filepath.Join(parent, "secret.txt")
	const secretContent = "TOP-SECRET-HOST-FILE-OUTSIDE-SOURCEDIR"
	require.NoError(t, os.WriteFile(secretPath, []byte(secretContent), 0o600))

	runner := &fakeRunner{exitCode: 0, stdout: "BUILD SUCCESSFUL\n"}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "true",
		OutputSubpath: "../secret.txt", // escapes SourceDir
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error,
		"OutputSubpath that resolves outside SourceDir MUST be rejected before any artifact "+
			"fetch happens — silently succeeding here means arbitrary host files reachable via "+
			"'..' segments get exfiltrated into HostOutputDir")
	assert.Contains(t, result.Error.Error(), "escapes SourceDir")
	assert.Empty(t, result.ArtifactPath, "no artifact path on rejection")

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	for _, e := range entries {
		content, _ := os.ReadFile(filepath.Join(outDir, e.Name()))
		assert.NotContains(t, string(content), secretContent,
			"HostOutputDir must never contain the exfiltrated secret file's bytes")
	}
}

func TestCTHardenXbuild2_OutputSubpath_RejectsAbsolutePath(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	etcPasswd := filepath.Join(t.TempDir(), "passwd-like-file")
	require.NoError(t, os.WriteFile(etcPasswd, []byte("root:x:0:0"), 0o644))

	runner := &fakeRunner{exitCode: 0}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "true",
		OutputSubpath: etcPasswd, // absolute path
		HostOutputDir: outDir,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "must be relative to SourceDir")
}

// Anti-regression companion: a normal nested-but-within-SourceDir
// OutputSubpath must still work exactly as before the fix.
func TestCTHardenXbuild2_OutputSubpath_LegitimateNestedPathStillWorks(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/libs/app.jar"

	runner := &fakeRunner{
		exitCode:        0,
		producesPath:    filepath.Join(srcDir, subpath),
		producesContent: []byte("JARCONTENT"),
	}
	hd := newHostDirectBackendWithRunner(runner)

	result := hd.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew jar",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error, "a legitimate nested-within-SourceDir OutputSubpath must still work")
	assert.Equal(t, int64(len("JARCONTENT")), result.ArtifactSize)
}
