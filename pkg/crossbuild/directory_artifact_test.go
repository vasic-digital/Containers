package crossbuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirArtifactRunner simulates a build (e.g. jpackage runtime image, or
// `gradle ... packageReleaseDeb` producing an app-image directory) that
// drops its artifact as a DIRECTORY containing real files.
type dirArtifactRunner struct {
	imageExists bool
	dirPath     string
	runCalled   bool
}

func (d *dirArtifactRunner) ImageExists(ctx context.Context, imageRef string) bool {
	return d.imageExists
}
func (d *dirArtifactRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	d.runCalled = true
	// Real jpackage app-image: a directory tree, not a single file.
	if err := os.MkdirAll(filepath.Join(d.dirPath, "lib"), 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(d.dirPath, "bin_app"), []byte("ELF..."), 0o755); err != nil {
		return -1, err
	}
	return 0, nil
}

// RED: a jpackage runtime image / app-image DIRECTORY is a real, valid,
// non-empty artifact. The backend advertises jpackage support in
// ArtifactNotes. A successful build that produces it MUST NOT be reported
// as a failure. Today it is (copyFile cannot read a directory) — false-FAIL
// (§11.4.1 FAIL-bluff).
func TestLinuxContainer_DirectoryArtifact_jpackage(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	subpath := "desktopApp/build/jpackage/MyApp" // a directory app-image

	runner := &dirArtifactRunner{imageExists: true, dirPath: filepath.Join(srcDir, subpath)}
	l := newLinuxContainerBackendWithRunner("test/linux-image", "amd64", runner)

	result := l.Build(context.Background(), BuildRequest{
		Target:        Target{OS: "linux", Arch: "amd64"},
		SourceDir:     srcDir,
		BuildCommand:  "./gradlew :desktopApp:packageRelease",
		OutputSubpath: subpath,
		HostOutputDir: outDir,
	})

	require.NoError(t, result.Error,
		"a real non-empty directory app-image is a valid artifact, not a build failure")
	assert.Greater(t, result.ArtifactSize, int64(0))
	// The artifact must actually land in HostOutputDir as a usable tree.
	require.NotEmpty(t, result.ArtifactPath)
	st, err := os.Stat(result.ArtifactPath)
	require.NoError(t, err)
	assert.True(t, st.IsDir(), "directory artifact must be copied as a directory")
	_, err = os.Stat(filepath.Join(result.ArtifactPath, "bin_app"))
	require.NoError(t, err, "contents of the directory artifact must be copied")
}
