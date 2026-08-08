// SPDX-License-Identifier: Apache-2.0
package macos

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

// fakeTartRunner is the test seam. It records calls and returns
// pre-configured responses so tests exercise the orchestration logic
// without a real Tart installation.
type fakeTartRunner struct {
	versionResult string
	versionErr    error
	cloneErr      error
	runErr        error
	sshStdout     string
	sshStderr     string
	sshExitCode   int
	sshErr        error
	stopErr       error
	deleteErr     error
	cloneCalled   bool
	runCalled     bool
	sshCalled     bool
	stopCalled    bool
	deleteCalled  bool
	lastVMName    string
	lastImage     string
	lastCommand   string
}

func (f *fakeTartRunner) Version(_ context.Context) (string, error) {
	return f.versionResult, f.versionErr
}

func (f *fakeTartRunner) Clone(_ context.Context, remoteImage, vmName string) error {
	f.cloneCalled = true
	f.lastVMName = vmName
	f.lastImage = remoteImage
	return f.cloneErr
}

func (f *fakeTartRunner) Run(_ context.Context, vmName string, _ RunOptions) error {
	f.runCalled = true
	f.lastVMName = vmName
	// Block until versionErr signals (simulates VM running in background).
	// For tests: return runErr after a short delay so SSHExec has time to run.
	time.Sleep(10 * time.Millisecond)
	return f.runErr
}

func (f *fakeTartRunner) SSHExec(_ context.Context, vmName, _, _, cmd string, _ time.Duration) (string, string, int, error) {
	f.sshCalled = true
	f.lastCommand = cmd
	f.lastVMName = vmName
	return f.sshStdout, f.sshStderr, f.sshExitCode, f.sshErr
}

func (f *fakeTartRunner) Stop(_ context.Context, vmName string) error {
	f.stopCalled = true
	f.lastVMName = vmName
	return f.stopErr
}

func (f *fakeTartRunner) Delete(_ context.Context, vmName string) error {
	f.deleteCalled = true
	return f.deleteErr
}

// TestMacOSBuilder_TartVersion_NotInstalled: on any non-macOS host
// (or when Tart returns an error), TartVersion must return
// ErrTartNotAvailable — not panic, not nil.
func TestMacOSBuilder_TartVersion_NotInstalled(t *testing.T) {
	// SKIP-OK: #tart-requires-macos-apple-silicon
	// This sub-test exercises the non-macOS path on all hosts.
	if runtime.GOOS == "darwin" {
		// On macOS the GOOS check passes; simulate Tart absent via the runner.
		fake := &fakeTartRunner{versionErr: errors.New("exec: tart not found")}
		b := newMacOSBuilderWithRunner(fake)
		_, err := b.TartVersion(context.Background())
		require.Error(t, err)
		assert.Equal(t, ErrTartNotAvailable, err)
		return
	}
	b := NewMacOSBuilder()
	_, err := b.TartVersion(context.Background())
	require.Error(t, err)
	assert.Equal(t, ErrTartNotAvailable, err,
		"non-macOS host must return ErrTartNotAvailable from TartVersion")
}

// TestMacOSBuilder_TartVersion_Installed: when Tart responds, the
// version string is returned verbatim.
func TestMacOSBuilder_TartVersion_Installed(t *testing.T) {
	fake := &fakeTartRunner{versionResult: "0.42.0", versionErr: nil}
	b := newMacOSBuilderWithRunner(fake)

	// Force darwin path via the runner (runtime.GOOS check skips non-darwin).
	// SKIP-OK: #tart-requires-macos-apple-silicon — real path requires macOS.
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon — TartVersion darwin branch requires macOS host")
	}
	v, err := b.TartVersion(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "0.42.0", v)
}

// TestMacOSBuilder_RunInVM_NonMacOSReturnsErrTartNotAvailable:
// on non-macOS host, RunInVM must return ErrTartNotAvailable
// immediately without touching the tartRunner.
func TestMacOSBuilder_RunInVM_NonMacOSReturnsErrTartNotAvailable(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test verifies the non-macOS fast-path; skip on macOS SKIP-OK: #darwin-skip-non-macos-fastpath")
	}
	fake := &fakeTartRunner{}
	b := newMacOSBuilderWithRunner(fake)

	result := b.RunInVM(context.Background(), VMRunRequest{
		Image:   "ghcr.io/example/macos-vm:latest",
		Command: "echo hello",
	})

	require.Error(t, result.Error)
	assert.Equal(t, ErrTartNotAvailable, result.Error,
		"non-macOS host must return ErrTartNotAvailable without invoking tartRunner")
	assert.False(t, fake.cloneCalled, "tartRunner.Clone must not be called on non-macOS host")
}

// TestMacOSBuilder_RunInVM_HappyPath: on macOS (or injected darwin path)
// verifies Clone → Run → SSHExec → Stop → Delete sequencing with
// CONST-039 runtime evidence: stdout captured, exit code propagated.
//
// SKIP-OK: #tart-requires-macos-apple-silicon — requires macOS host for
// the GOOS check to pass. On non-macOS CI this test is skipped.
func TestMacOSBuilder_RunInVM_HappyPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon — RunInVM happy path requires macOS host")
	}

	fake := &fakeTartRunner{
		versionResult: "0.42.0",
		sshStdout:     "build completed successfully",
		sshExitCode:   0,
	}
	b := newMacOSBuilderWithRunner(fake)

	result := b.RunInVM(context.Background(), VMRunRequest{
		Image:   "ghcr.io/example/macos-vm:latest",
		Command: "./gradlew :desktopApp:packageDmg",
		Timeout: 2 * time.Minute,
	})

	// CONST-039 runtime evidence assertions.
	require.NoError(t, result.Error)
	assert.True(t, fake.cloneCalled, "Clone must have been called")
	assert.True(t, fake.sshCalled, "SSHExec must have been called")
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "build completed")
	assert.Equal(t, "./gradlew :desktopApp:packageDmg", fake.lastCommand)
	assert.True(t, fake.deleteCalled, "VM must be deleted after run (KeepVM=false)")
}

// TestMacOSBuilder_RunInVM_MissingImage: empty Image returns error
// without cloning.
func TestMacOSBuilder_RunInVM_MissingImage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon")
	}
	fake := &fakeTartRunner{versionResult: "0.42.0"}
	b := newMacOSBuilderWithRunner(fake)

	result := b.RunInVM(context.Background(), VMRunRequest{
		Image:   "",
		Command: "echo hi",
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "Image is required")
	assert.False(t, fake.cloneCalled)
}

// TestMacOSBuilder_RunInVM_CloneFailure: Clone error propagates cleanly.
func TestMacOSBuilder_RunInVM_CloneFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon")
	}
	fake := &fakeTartRunner{
		versionResult: "0.42.0",
		cloneErr:      errors.New("network timeout cloning image"),
	}
	b := newMacOSBuilderWithRunner(fake)

	result := b.RunInVM(context.Background(), VMRunRequest{
		Image:   "ghcr.io/example/macos-vm:latest",
		Command: "echo hi",
		Timeout: 1 * time.Minute,
	})

	require.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "network timeout cloning image")
	assert.False(t, fake.sshCalled, "SSH must not be attempted after Clone failure")
}

// TestMacOSBuilder_RunInVM_SSHNonZeroExitCode: non-zero SSH exit
// code is an error, not a silent success.
func TestMacOSBuilder_RunInVM_SSHNonZeroExitCode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon")
	}
	fake := &fakeTartRunner{
		versionResult: "0.42.0",
		sshStdout:     "",
		sshStderr:     "build failed: compilation error",
		sshExitCode:   1,
	}
	b := newMacOSBuilderWithRunner(fake)

	result := b.RunInVM(context.Background(), VMRunRequest{
		Image:   "ghcr.io/example/macos-vm:latest",
		Command: "./gradlew :desktopApp:packageDmg",
		Timeout: 2 * time.Minute,
	})

	require.Error(t, result.Error)
	assert.Equal(t, 1, result.ExitCode)
	assert.Contains(t, result.Error.Error(), "exited 1")
}

// TestTartInstalled is the real-stack smoke test for the CI environment.
// It runs `tart --version` via os/exec — not the fake — and expects
// either a version string or the absence of Tart to be detected honestly.
// This is the CONST-039 evidence that the production binary path exists.
func TestTartInstalled(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// SKIP-OK: #tart-requires-macos-apple-silicon
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon — tart requires macOS Apple Silicon host")
	}
	cmd := exec.Command("tart", "--version")
	out, err := cmd.Output()
	if err != nil {
		// Tart not installed — honest skip, not a failure.
		t.Logf("Tart not installed on this macOS host: %v", err)
		t.Skip("// SKIP-OK: #tart-requires-macos-apple-silicon — tart binary not found on PATH")
	}
	// CONST-039 runtime evidence: version string is non-empty.
	version := string(out)
	assert.NotEmpty(t, version, "tart --version must produce a non-empty version string")
	t.Logf("CONST-039 evidence: tart --version = %s", version)
}
