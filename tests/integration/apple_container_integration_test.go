//go:build integration

// Package integration — REAL-stack Apple `container` integration test.
//
// Per the no-fakes-beyond-unit-tests mandate, this test exercises the
// REAL Apple `container` engine (no mocks). It is fully self-driving:
// it detects engine + kernel readiness at runtime and SKIPs-with-reason
// (honest kernel-gap) when the engine is absent, the host is not macOS,
// or a probe `container run` cannot boot a Linux micro-VM — it NEVER
// fakes a PASS.
//
// The proof is UNFORGEABLE: the host `uname -s -m` reports
// "Darwin arm64", but a Linux container's `uname -s -m` reports
// "Linux aarch64". Asserting the latter proves a real Linux kernel ran
// inside an Apple-`container` micro-VM, not the macOS host.
//
// Run with:  go test -tags=integration ./tests/integration/ -run AppleContainer -v
// Re-runnable: self-cleaning (`--rm` containers + t.TempDir).
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/crossbuild"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const appleProbeImage = "docker.io/library/alpine:latest"

// appleContainerReady reports whether the host can run a real Linux
// container via Apple `container`, plus a human-readable reason when it
// cannot (used as the SKIP message). It performs an actual probe boot
// so a half-installed engine (binary present, kernel still downloading)
// is correctly classified as NOT ready — honest kernel-gap per the
// cross-platform-parity mandate.
func appleContainerReady(t *testing.T) (bool, string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return false, "host is not macOS (Apple `container` is macOS-only); on Linux the podman/docker LinuxContainerBackend serves the same target"
	}
	if _, err := exec.LookPath("container"); err != nil {
		return false, "apple `container` CLI not installed (one-time: brew install --cask container && container system start)"
	}
	// Engine system service running?
	statusCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	statusOut, _ := exec.CommandContext(statusCtx, "container", "system", "status").CombinedOutput()
	if !strings.Contains(strings.ToLower(string(statusOut)), "running") {
		return false, "apple `container` system service not running (one-time: container system start)"
	}
	// Probe boot: actually run a Linux micro-VM. This catches the
	// "default kernel still downloading" state honestly.
	probe := crossbuild.RunInLinuxContainer(context.Background(), crossbuild.LinuxRunRequest{
		Image:   appleProbeImage,
		Command: "true",
		Timeout: 120 * time.Second,
	})
	if probe.Error != nil {
		return false, "apple `container` probe boot failed (engine/kernel not ready): " + probe.Error.Error()
	}
	if probe.ExitCode != 0 {
		return false, "apple `container` probe boot exited non-zero (engine/kernel not ready)"
	}
	return true, ""
}

// TestAppleContainer_RealLinuxKernel is the unforgeable proof: a real
// Linux container reports "Linux aarch64" while the macOS host reports
// "Darwin arm64".
func TestAppleContainer_RealLinuxKernel(t *testing.T) {
	ready, reason := appleContainerReady(t)
	if !ready {
		t.Skipf("SKIP (honest kernel-gap per cross-platform-parity mandate): %s", reason)
	}

	res := crossbuild.RunInLinuxContainer(context.Background(), crossbuild.LinuxRunRequest{
		Image:   appleProbeImage,
		Command: "uname -s -m",
		Timeout: 120 * time.Second,
	})
	require.NoError(t, res.Error, "real container run must launch")
	require.Equal(t, 0, res.ExitCode, "uname must exit 0; stderr=%s", res.Stderr)

	out := strings.TrimSpace(res.Stdout)
	t.Logf("container uname -s -m => %q (host is %s/%s)", out, runtime.GOOS, runtime.GOARCH)

	// UNFORGEABLE: the macOS host's kernel is Darwin. A Linux kernel
	// here proves a real Linux micro-VM booted. Assert Linux, NOT
	// Darwin — pointing the assertion at the host would wrongly pass.
	assert.Contains(t, out, "Linux", "must be a Linux kernel, not the Darwin host")
	assert.Contains(t, out, "aarch64", "must be native arm64 Linux (aarch64)")
	assert.NotContains(t, out, "Darwin", "must NOT be the macOS host kernel")
}

// TestAppleContainer_HostDirMountRoundTrip writes a sentinel file on
// the HOST and asserts the container reads it back THROUGH the mount —
// proving the bind mount actually wires the host dir into the guest.
func TestAppleContainer_HostDirMountRoundTrip(t *testing.T) {
	ready, reason := appleContainerReady(t)
	if !ready {
		t.Skipf("SKIP (honest kernel-gap per cross-platform-parity mandate): %s", reason)
	}

	srcDir := t.TempDir()
	sentinel := "apple-container-mount-proof-" + time.Now().Format("150405.000")
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "sentinel.txt"), []byte(sentinel), 0o644))

	res := crossbuild.RunInLinuxContainer(context.Background(), crossbuild.LinuxRunRequest{
		Image:       appleProbeImage,
		Command:     "cat /work/src/sentinel.txt",
		MountSource: srcDir,
		MountTarget: "/work/src",
		Timeout:     120 * time.Second,
	})
	require.NoError(t, res.Error)
	require.Equal(t, 0, res.ExitCode, "cat must exit 0; stderr=%s", res.Stderr)
	assert.Equal(t, sentinel, strings.TrimSpace(res.Stdout),
		"container MUST read the host-written sentinel through the mount")

	// Reverse direction: container writes into the mount, host reads it
	// back — proves the mount is read-write and round-trips both ways.
	reverse := "guest-wrote-" + sentinel
	resW := crossbuild.RunInLinuxContainer(context.Background(), crossbuild.LinuxRunRequest{
		Image:       appleProbeImage,
		Command:     "printf '%s' '" + reverse + "' > /work/src/from_guest.txt",
		MountSource: srcDir,
		MountTarget: "/work/src",
		Timeout:     120 * time.Second,
	})
	require.NoError(t, resW.Error)
	require.Equal(t, 0, resW.ExitCode, "guest write must exit 0; stderr=%s", resW.Stderr)

	got, err := os.ReadFile(filepath.Join(srcDir, "from_guest.txt"))
	require.NoError(t, err, "host must see the file the guest wrote through the mount")
	assert.Equal(t, reverse, string(got))
}
