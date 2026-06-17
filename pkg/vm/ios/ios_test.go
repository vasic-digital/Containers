// SPDX-License-Identifier: Apache-2.0
package ios

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeXcodeRunner is the test seam. Records calls and returns
// pre-configured responses.
type fakeXcodeRunner struct {
	xcodeVersionResult string
	xcodeVersionErr    error
	xcrunVersionResult string
	xcrunVersionErr    error
	buildStdout        string
	buildStderr        string
	buildExitCode      int
	buildErr           error
	exportStdout       string
	exportStderr       string
	exportExitCode     int
	exportErr          error
	simctlResult       string
	simctlErr          error
	simctlRunStdout    string
	simctlRunStderr    string
	simctlRunExitCode  int
	simctlRunErr       error
	buildCalled        bool
	exportCalled       bool
	simctlCalled       bool
	simctlRunCalled    bool
	lastSimctlSubcmd   string
	lastBuildArgs      []string
}

func (f *fakeXcodeRunner) XcodeVersion(_ context.Context) (string, error) {
	return f.xcodeVersionResult, f.xcodeVersionErr
}

func (f *fakeXcodeRunner) XcrunVersion(_ context.Context) (string, error) {
	return f.xcrunVersionResult, f.xcrunVersionErr
}

func (f *fakeXcodeRunner) Build(_ context.Context, args []string, _ time.Duration) (string, string, int, error) {
	f.buildCalled = true
	f.lastBuildArgs = append(f.lastBuildArgs, args...)
	return f.buildStdout, f.buildStderr, f.buildExitCode, f.buildErr
}

func (f *fakeXcodeRunner) ExportArchive(_ context.Context, _, _, _ string, _ time.Duration) (string, string, int, error) {
	f.exportCalled = true
	return f.exportStdout, f.exportStderr, f.exportExitCode, f.exportErr
}

func (f *fakeXcodeRunner) SimctlList(_ context.Context) (string, error) {
	f.simctlCalled = true
	return f.simctlResult, f.simctlErr
}

func (f *fakeXcodeRunner) SimctlRun(_ context.Context, subcommand string, _ []string, _ time.Duration) (string, string, int, error) {
	if !f.simctlRunCalled {
		// Record only the FIRST subcommand so tests can assert the primary
		// operation that was requested (BootSimulator calls boot then bootstatus).
		f.lastSimctlSubcmd = subcommand
	}
	f.simctlRunCalled = true
	return f.simctlRunStdout, f.simctlRunStderr, f.simctlRunExitCode, f.simctlRunErr
}

// TestIOSBuilder_XcodeVersion_NonMacOS: on non-macOS host, XcodeVersion
// must return ErrXcodeNotAvailable.
func TestIOSBuilder_XcodeVersion_NonMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// On macOS simulate absence via the runner.
		fake := &fakeXcodeRunner{xcodeVersionErr: errors.New("xcodebuild not found")}
		b := newIOSBuilderWithRunner(fake)
		_, err := b.XcodeVersion(context.Background())
		require.Error(t, err)
		assert.Equal(t, ErrXcodeNotAvailable, err)
		return
	}
	b := NewIOSBuilder()
	_, err := b.XcodeVersion(context.Background())
	require.Error(t, err)
	assert.Equal(t, ErrXcodeNotAvailable, err,
		"non-macOS host must return ErrXcodeNotAvailable from XcodeVersion")
}

// TestIOSBuilder_XcodeVersion_Installed: when xcodebuild responds, the
// version string is returned.
func TestIOSBuilder_XcodeVersion_Installed(t *testing.T) {
	// SKIP-OK: #ios-build-requires-xcode-macos
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — XcodeVersion darwin branch requires macOS host")
	}
	fake := &fakeXcodeRunner{xcodeVersionResult: "Xcode 15.4", xcodeVersionErr: nil}
	b := newIOSBuilderWithRunner(fake)
	v, err := b.XcodeVersion(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Xcode 15.4", v)
}

// TestIOSBuilder_BuildIPA_NonMacOS: on non-macOS host, BuildIPA must
// return ErrXcodeNotAvailable immediately without touching xcodeRunner.
func TestIOSBuilder_BuildIPA_NonMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test verifies the non-macOS fast-path; skip on macOS SKIP-OK: #darwin-skip-non-macos-fastpath")
	}
	fake := &fakeXcodeRunner{}
	b := newIOSBuilderWithRunner(fake)

	result := b.BuildIPA(context.Background(), BuildIPARequest{
		ProjectDir: "/some/project.xcodeproj",
		Scheme:     "MyApp",
		ExportPath: "/tmp/out",
	})

	require.Error(t, result.Error)
	assert.Equal(t, ErrXcodeNotAvailable, result.Error,
		"non-macOS host must return ErrXcodeNotAvailable without invoking xcodeRunner")
	assert.False(t, fake.buildCalled, "xcodeRunner.Build must not be called on non-macOS host")
}

// TestIOSBuilder_BuildIPA_HappyPath: on macOS (or injected darwin path)
// verifies archive → exportArchive sequencing with CONST-039 runtime
// evidence: stdout captured, IPAPath set, exit code 0.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_BuildIPA_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — BuildIPA happy path requires macOS host")
	}

	fake := &fakeXcodeRunner{
		xcodeVersionResult: "Xcode 15.4",
		buildStdout:        "** ARCHIVE SUCCEEDED **",
		buildExitCode:      0,
		exportStdout:       "** EXPORT SUCCEEDED **",
		exportExitCode:     0,
	}
	b := newIOSBuilderWithRunner(fake)

	result := b.BuildIPA(context.Background(), BuildIPARequest{
		ProjectDir: "/fake/project.xcodeproj",
		Scheme:     "MyScheme",
		ExportPath: "/tmp/ios-out",
		Timeout:    2 * time.Minute,
	})

	// CONST-039 runtime evidence assertions.
	require.NoError(t, result.Error)
	assert.True(t, fake.buildCalled, "xcodebuild archive must have been called")
	assert.True(t, fake.exportCalled, "xcodebuild -exportArchive must have been called")
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "ARCHIVE SUCCEEDED")
	assert.Equal(t, "/tmp/ios-out/MyScheme.ipa", result.IPAPath)
}

// TestIOSBuilder_BuildIPA_MissingProjectDir: empty ProjectDir returns
// an error without calling xcodeRunner.
func TestIOSBuilder_BuildIPA_MissingProjectDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{xcodeVersionResult: "Xcode 15.4"}
	b := newIOSBuilderWithRunner(fake)

	result := b.BuildIPA(context.Background(), BuildIPARequest{
		ProjectDir: "",
		Scheme:     "MyApp",
		ExportPath: "/tmp/out",
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "ProjectDir is required")
	assert.False(t, fake.buildCalled)
}

// TestIOSBuilder_BuildIPA_MissingScheme: empty Scheme returns error.
func TestIOSBuilder_BuildIPA_MissingScheme(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{xcodeVersionResult: "Xcode 15.4"}
	b := newIOSBuilderWithRunner(fake)

	result := b.BuildIPA(context.Background(), BuildIPARequest{
		ProjectDir: "/fake/project.xcodeproj",
		Scheme:     "",
		ExportPath: "/tmp/out",
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "Scheme is required")
}

// TestIOSBuilder_BuildIPA_ArchiveFails: xcodebuild archive failure
// propagates before exportArchive is attempted.
func TestIOSBuilder_BuildIPA_ArchiveFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{
		xcodeVersionResult: "Xcode 15.4",
		buildExitCode:      65,
		buildStderr:        "ARCHIVE FAILED: error: build input file 'X.swift' cannot be found",
	}
	b := newIOSBuilderWithRunner(fake)

	result := b.BuildIPA(context.Background(), BuildIPARequest{
		ProjectDir: "/fake/project.xcodeproj",
		Scheme:     "MyApp",
		ExportPath: "/tmp/out",
		Timeout:    2 * time.Minute,
	})

	require.Error(t, result.Error)
	assert.Equal(t, 65, result.ExitCode)
	assert.False(t, fake.exportCalled,
		"exportArchive must not be called after archive failure")
}

// TestIOSBuilder_ListSimulators_NonMacOS: on non-macOS returns error.
func TestIOSBuilder_ListSimulators_NonMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test verifies the non-macOS fast-path; skip on macOS SKIP-OK: #darwin-skip-non-macos-fastpath")
	}
	b := NewIOSBuilder()
	_, err := b.ListSimulators(context.Background())
	require.Error(t, err)
	assert.Equal(t, ErrXcodeNotAvailable, err)
}

// TestIOSBuilder_ListSimulators_HappyPath: returns simctl JSON on macOS.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_ListSimulators_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{
		simctlResult: `{"devices": {}, "runtimes": [], "devicetypes": []}`,
	}
	b := newIOSBuilderWithRunner(fake)
	out, err := b.ListSimulators(context.Background())
	require.NoError(t, err)
	assert.Contains(t, out, "devices")
	assert.True(t, fake.simctlCalled)
}

// TestIOSBuilder_BootSimulator_NonMacOS verifies the non-macOS guard.
func TestIOSBuilder_BootSimulator_NonMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("SKIP-OK: this test verifies the non-macOS fast-path; skip on macOS")
	}
	b := NewIOSBuilder()
	err := b.BootSimulator(context.Background(), "fake-udid", 0)
	require.Error(t, err)
	assert.Equal(t, ErrXcodeNotAvailable, err)
}

// TestIOSBuilder_BootSimulator_HappyPath verifies BootSimulator calls simctl boot.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_BootSimulator_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{simctlRunExitCode: 0}
	b := newIOSBuilderWithRunner(fake)
	err := b.BootSimulator(context.Background(), "test-udid", 5*time.Second)
	require.NoError(t, err)
	assert.True(t, fake.simctlRunCalled)
	assert.Equal(t, "boot", fake.lastSimctlSubcmd)
}

// TestIOSBuilder_ShutdownSimulator_HappyPath verifies ShutdownSimulator calls simctl shutdown.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_ShutdownSimulator_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{simctlRunExitCode: 0}
	b := newIOSBuilderWithRunner(fake)
	err := b.ShutdownSimulator(context.Background(), "test-udid", 5*time.Second)
	require.NoError(t, err)
	assert.True(t, fake.simctlRunCalled)
	assert.Equal(t, "shutdown", fake.lastSimctlSubcmd)
}

// TestIOSBuilder_InstallApp_HappyPath verifies InstallApp calls simctl install.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_InstallApp_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{simctlRunExitCode: 0}
	b := newIOSBuilderWithRunner(fake)
	err := b.InstallApp(context.Background(), "test-udid", "/path/to/Yole.app", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, fake.simctlRunCalled)
	assert.Equal(t, "install", fake.lastSimctlSubcmd)
}

// TestIOSBuilder_LaunchApp_HappyPath verifies LaunchApp calls simctl launch.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_LaunchApp_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{simctlRunExitCode: 0}
	b := newIOSBuilderWithRunner(fake)
	err := b.LaunchApp(context.Background(), "test-udid", "digital.vasic.yole.ios", 10*time.Second)
	require.NoError(t, err)
	assert.True(t, fake.simctlRunCalled)
	assert.Equal(t, "launch", fake.lastSimctlSubcmd)
}

// TestIOSBuilder_Screenshot_HappyPath verifies Screenshot calls simctl io screenshot.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestIOSBuilder_Screenshot_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos")
	}
	fake := &fakeXcodeRunner{simctlRunExitCode: 0}
	b := newIOSBuilderWithRunner(fake)
	err := b.Screenshot(context.Background(), "test-udid", "/tmp/screen.png", 5*time.Second)
	require.NoError(t, err)
	assert.True(t, fake.simctlRunCalled)
	assert.Equal(t, "io", fake.lastSimctlSubcmd)
}

// TestXcodeInstalled is the real-stack smoke test for the CI environment.
// Runs `xcodebuild -version` via os/exec and expects either a version
// string or an honest skip if Xcode is absent.
func TestXcodeInstalled(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// SKIP-OK: #ios-build-requires-xcode-macos
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — xcodebuild requires macOS host")
	}
	cmd := exec.Command("xcodebuild", "-version")
	out, err := cmd.Output()
	if err != nil {
		t.Logf("xcodebuild not installed on this macOS host: %v", err)
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — xcodebuild not found on PATH")
	}
	// CONST-039 runtime evidence: version string is non-empty.
	version := string(out)
	assert.NotEmpty(t, version, "xcodebuild -version must produce a non-empty version string")
	t.Logf("CONST-039 evidence: xcodebuild -version = %s", version)
}

// TestXcrunSimctlLists is the real-stack smoke test that xcrun simctl
// can enumerate simulators on the current macOS host.
//
// SKIP-OK: #ios-build-requires-xcode-macos
func TestXcrunSimctlLists(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — xcrun requires macOS host")
	}
	cmd := exec.Command("xcrun", "simctl", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		t.Skip("// SKIP-OK: #ios-build-requires-xcode-macos — xcrun not installed")
	}
	// CONST-039 runtime evidence: JSON output contains "devices" key.
	assert.Contains(t, string(out), `"devices"`,
		"xcrun simctl list --json must contain a 'devices' key")
	t.Logf("CONST-039 evidence: xcrun simctl list returned %d bytes of JSON", len(out))
}
