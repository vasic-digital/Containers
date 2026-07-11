// Package genymotion provides detection and lifecycle control of Genymotion
// Desktop Android virtual devices for the vasic-digital container ecosystem.
//
// Genymotion runs Android in a VM (VirtualBox/QEMU/Hypervisor.framework), so it
// satisfies the consuming project's §6.AH mandate ("virtual devices / emulators MUST run in
// Containers or VMs — never host-direct"): a Genymotion VM is NOT a host-direct
// `emulator -avd` AVD, it is a managed virtual machine. This package is the
// Containers-submodule landing that makes bringing such a device up/down
// programmatic and OS-portable (macOS + Linux), per the operator directive of
// 2026-06-06.
//
// Design for testability (§6.J anti-bluff, inherited from the Containers parent):
// every public method routes its external `gmtool` invocation through the
// injectable [Tool.exec] field, and all output parsing lives in PURE functions
// ([parseList], [parseVersion]) — so the package is fully unit-testable against
// captured real gmtool output WITHOUT a live Genymotion install. The exec field
// defaults to a real os/exec runner.
package genymotion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNotInstalled is returned by [Detect] when no gmtool binary is found on
// any OS-appropriate candidate path or on PATH.
var ErrNotInstalled = errors.New("genymotion: gmtool not found (Genymotion Desktop not installed or not on PATH)")

// Device is one row of `gmtool admin list` — a Genymotion virtual device.
type Device struct {
	// State is "On" (running) or "Off" (stopped) as reported by gmtool.
	State string
	// ADBSerial is the adb serial of a running device (e.g. "127.0.0.1:6555"),
	// empty when the device is Off.
	ADBSerial string
	// UUID is the device's stable Genymotion identifier.
	UUID string
	// Name is the human-readable device name (e.g. "Google Pixel 9").
	Name string
}

// IsOn reports whether the device is currently running.
func (d Device) IsOn() bool { return strings.EqualFold(strings.TrimSpace(d.State), "On") }

// ADBProbe reports whether the Android system inside a running Genymotion VM
// has actually finished booting — adb-reachable AND `sys.boot_completed=1` —
// for the given adb serial. It is the genymotion analogue of
// [emulator.AndroidEmulator.WaitForBoot]. Attach one via [Tool.WithADBProbe]
// so [Tool.StartAndWait] waits for real Android boot rather than returning as
// soon as the VM is merely powered (see the StartAndWait readiness boundary).
type ADBProbe func(ctx context.Context, serial string) (booted bool, err error)

// Tool is a handle to a located gmtool binary.
type Tool struct {
	// Path is the absolute path to the gmtool executable.
	Path string
	// exec runs gmtool with the given args and returns combined stdout+stderr.
	// Injectable for tests; defaults to a real os/exec runner.
	exec func(ctx context.Context, path string, args ...string) (string, error)
	// adbProbe, when non-nil, is polled by StartAndWait after the VM reports
	// On+serial to confirm Android has booted before returning ready (§11.4.108
	// readiness = booted, not merely powered). Attach via WithADBProbe.
	adbProbe ADBProbe
	// pollInterval is the StartAndWait re-list cadence; 0 means the 2s default.
	// Injectable so timeout/retry logic is unit-testable without multi-second
	// waits.
	pollInterval time.Duration
}

// NewTool returns a Tool bound to an explicit gmtool path with the real exec
// runner. Used by callers that already know the path (and by Detect).
func NewTool(path string) *Tool {
	return &Tool{Path: path, exec: realExec}
}

// WithADBProbe attaches an Android boot-readiness probe so [Tool.StartAndWait]
// waits for `sys.boot_completed=1` (not merely a powered VM with an allocated
// adb serial) before returning ready. Returns t for chaining. Additive:
// callers that do not attach a probe keep the powered-but-not-booted readiness
// boundary documented on StartAndWait.
func (t *Tool) WithADBProbe(p ADBProbe) *Tool { t.adbProbe = p; return t }

func realExec(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CandidatePathsForOS returns the ordered list of well-known gmtool install
// locations for a given GOOS + home directory. Pure (no filesystem access) so
// it is unit-testable across OSes from any host.
func CandidatePathsForOS(goos, home string) []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Genymotion.app/Contents/MacOS/gmtool",
			filepath.Join(home, "Applications/Genymotion.app/Contents/MacOS/gmtool"),
		}
	case "linux":
		return []string{
			"/opt/genymobile/genymotion/gmtool",
			"/opt/genymotion/gmtool",
			filepath.Join(home, "genymotion/gmtool"),
			filepath.Join(home, ".local/share/genymotion/gmtool"),
			"/usr/local/bin/gmtool",
		}
	default:
		return nil
	}
}

// Detect locates a gmtool binary: it checks the OS-appropriate candidate paths
// first, then falls back to PATH. Returns [ErrNotInstalled] when none is found.
func Detect() (*Tool, error) {
	home, _ := os.UserHomeDir()
	for _, p := range CandidatePathsForOS(runtime.GOOS, home) {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return NewTool(p), nil
		}
	}
	if p, err := exec.LookPath("gmtool"); err == nil {
		return NewTool(p), nil
	}
	return nil, ErrNotInstalled
}

// Version returns the Genymotion version string (e.g. "3.10.0").
func (t *Tool) Version(ctx context.Context) (string, error) {
	out, err := t.exec(ctx, t.Path, "version")
	if err != nil {
		return "", fmt.Errorf("genymotion: gmtool version failed: %w (%s)", err, strings.TrimSpace(out))
	}
	v := parseVersion(out)
	if v == "" {
		return "", fmt.Errorf("genymotion: could not parse version from gmtool output: %q", out)
	}
	return v, nil
}

// List returns every Genymotion virtual device known to gmtool (`gmtool admin
// list`), running or not.
func (t *Tool) List(ctx context.Context) ([]Device, error) {
	out, err := t.exec(ctx, t.Path, "admin", "list")
	if err != nil {
		return nil, fmt.Errorf("genymotion: gmtool admin list failed: %w (%s)", err, strings.TrimSpace(out))
	}
	return parseList(out), nil
}

// Running returns only the devices currently in the On state.
func (t *Tool) Running(ctx context.Context) ([]Device, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	var on []Device
	for _, d := range all {
		if d.IsOn() {
			on = append(on, d)
		}
	}
	return on, nil
}

// Start boots the named virtual device (`gmtool admin start <name>`). It is a
// no-op-safe call: gmtool returns success if the device is already running.
func (t *Tool) Start(ctx context.Context, name string) error {
	out, err := t.exec(ctx, t.Path, "admin", "start", name)
	if err != nil {
		return fmt.Errorf("genymotion: gmtool admin start %q failed: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Stop shuts the named virtual device down (`gmtool admin stop <name>`).
func (t *Tool) Stop(ctx context.Context, name string) error {
	out, err := t.exec(ctx, t.Path, "admin", "stop", name)
	if err != nil {
		return fmt.Errorf("genymotion: gmtool admin stop %q failed: %w (%s)", name, err, strings.TrimSpace(out))
	}
	return nil
}

// StartAndWait boots the named device, then polls `admin list` until that device
// reports On with a non-empty ADB serial (or the timeout elapses), returning the
// resolved Device.
//
// Readiness boundary (§11.4.108, honest per §11.4.6): WITHOUT an [ADBProbe]
// attached (via [Tool.WithADBProbe]), StartAndWait returns as soon as the VM is
// powered (State On) with a gmtool-allocated adb serial. That confirms the VM
// booted and its adb endpoint was ALLOCATED — it does NOT confirm Android
// reached `sys.boot_completed=1`; gmtool reports On+serial before the guest
// finishes booting, so an immediate `adb -s <serial> shell` may fail with
// "device offline". Callers needing a drive-ready device MUST either attach a
// probe (StartAndWait then waits for `sys.boot_completed=1`, mirroring
// [emulator.AndroidEmulator.WaitForBoot]) or run their own
// `adb -s <serial> wait-for-device` + boot-completed poll after this returns.
//
// Addressing (§11.4.174-adjacent): the device is matched by Name OR UUID.
// Genymotion permits multiple clones with an identical Name; matching by Name
// returns the FIRST On+serial match, which may not be the clone Start booted.
// To address a specific clone unambiguously, pass its UUID as name.
//
// Timeout enforcement (GENY-1): the timeout deadline bounds Start AND every
// `admin list` exec — a wedged gmtool (VM/hypervisor stall) is cancelled at the
// deadline instead of blocking forever on the caller's (possibly non-cancelling)
// context.
func (t *Tool) StartAndWait(ctx context.Context, name string, timeout time.Duration) (Device, error) {
	deadline := time.Now().Add(timeout)
	// GENY-1: bind the whole wait (Start + every List + every probe) to the
	// deadline, so each underlying exec.CommandContext is cancelled when the
	// timeout elapses even if the caller passed context.Background(). The
	// existing select{<-ctx.Done()} then fires at the deadline.
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if err := t.Start(cctx, name); err != nil {
		return Device{}, err
	}
	interval := t.pollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	var last Device
	var lastErr error
	for time.Now().Before(deadline) {
		devices, err := t.List(cctx)
		if err != nil {
			// GENY-4: a single transient `admin list` failure (e.g. device
			// busy mid-boot) must NOT abort the whole wait — retain the error
			// for the timeout diagnostic and keep polling to the deadline.
			lastErr = err
		} else {
			for _, d := range devices {
				if !strings.EqualFold(d.Name, name) && d.UUID != name {
					continue
				}
				last = d
				if !d.IsOn() || d.ADBSerial == "" {
					continue
				}
				// VM is powered with an allocated adb serial.
				if t.adbProbe == nil {
					return d, nil
				}
				// GENY-2: confirm Android actually booted before ready.
				booted, perr := t.adbProbe(cctx, d.ADBSerial)
				if perr != nil {
					lastErr = perr
				} else if booted {
					return d, nil
				}
			}
		}
		select {
		case <-cctx.Done():
			return Device{}, cctx.Err()
		case <-time.After(interval):
		}
	}
	if lastErr != nil {
		return Device{}, fmt.Errorf("genymotion: device %q did not reach On+serial+booted within %s (last state %q, last error: %v)", name, timeout, last.State, lastErr)
	}
	return Device{}, fmt.Errorf("genymotion: device %q did not reach On+serial+booted within %s (last state %q)", name, timeout, last.State)
}

// parseVersion extracts the version from `gmtool version` output. The line of
// interest is `Version  : 3.10.0`.
func parseVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "version") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// parseList parses the pipe-delimited table emitted by `gmtool admin list`:
//
//	 State    |   ADB Serial    |                UUID                |      Name
//	----------+-----------------+------------------------------------+---------------
//	       On |  127.0.0.1:6555 |ed811e5e-...-4250058e57e3| Google Pixel 9
//
// The header row and the dashed separator row are skipped; every remaining row
// with at least 4 pipe-separated columns AND a non-empty State becomes a Device.
//
// GENY-3: rows whose State is neither "On" nor "Off" (transitional states such
// as "Starting"/"Booting", or a version-drift label) are RETAINED verbatim, not
// dropped — so a mid-transition device is observable in [Tool.StartAndWait]'s
// `last` diagnostic instead of vanishing and being reported as `last state ""`
// (§11.4.6 masked-diagnostic avoidance), and a running device labelled anything
// but the literal "On" stays visible. Pure function.
func parseList(out string) []Device {
	var devices []Device
	for _, line := range strings.Split(out, "\n") {
		raw := strings.TrimSpace(line)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "|") {
			continue
		}
		// Skip the dashed separator row (only dashes, pluses, spaces).
		if strings.Trim(raw, "-+ ") == "" {
			continue
		}
		cols := strings.Split(raw, "|")
		if len(cols) < 4 {
			continue
		}
		state := strings.TrimSpace(cols[0])
		serial := strings.TrimSpace(cols[1])
		uuid := strings.TrimSpace(cols[2])
		name := strings.TrimSpace(strings.Join(cols[3:], "|"))
		// Skip the header row.
		if strings.EqualFold(state, "State") && strings.EqualFold(name, "Name") {
			continue
		}
		// A data row must carry a non-empty state; unknown non-empty states
		// (transitional/version-drift) are retained verbatim (GENY-3).
		if state == "" {
			continue
		}
		devices = append(devices, Device{State: state, ADBSerial: serial, UUID: uuid, Name: name})
	}
	return devices
}
