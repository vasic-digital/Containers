// Package applesim provides detection and lifecycle control of Apple iOS / iPadOS
// / tvOS / watchOS Simulator devices for the vasic-digital container ecosystem,
// by wrapping the host-native `xcrun simctl` toolchain.
//
// Why a host-native wrapper (and NOT a container): Apple Simulator runtimes are
// macOS-host-only — they depend on the CoreSimulator framework, the Simulator.app
// host process, and Apple's closed `simctl` toolchain. They CANNOT run inside a
// Linux container (Docker/Podman) the way an Android emulator or a Genymotion VM
// can, and Apple's licence forbids virtualising macOS off Apple hardware. So,
// unlike pkg/emulator (Android-in-container) and pkg/genymotion (Android-in-VM),
// this package orchestrates the host's own `xcrun simctl` lifecycle: list, boot,
// install, launch, record, shutdown. It is the Containers-submodule landing that
// makes driving an iOS Simulator programmatic and uniform with the rest of the
// device fleet, even though the only viable backend is the host toolchain.
//
// Design for testability (§6.J anti-bluff, inherited from the Containers parent):
// every public method routes its external `xcrun simctl` invocation through the
// injectable [Tool.exec] field, and all output parsing lives in PURE functions
// ([parseListJSON]) — so the package is fully unit-testable against captured real
// simctl output WITHOUT a live Xcode install. The exec field defaults to a real
// os/exec runner.
//
// Project-decoupling (CONST-051): this package hardcodes no consuming-project
// names, bundle ids, app paths, device names, or recording paths. Every such
// value is supplied by the caller at call time.
package applesim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// interruptSignal is the signal simctl recordVideo expects to flush + finalise
// the output file. os.Interrupt (SIGINT) is the cross-platform spelling.
var interruptSignal os.Signal = os.Interrupt

// ErrNotInstalled is returned by [Detect] when no `xcrun` binary is found on
// PATH (Xcode command-line tools not installed) or when `xcrun simctl help`
// fails (Simulator subsystem unavailable — e.g. non-macOS host).
var ErrNotInstalled = errors.New("applesim: xcrun simctl not available (Xcode command-line tools not installed, or not a macOS host)")

// Device is one simulator device as reported by `xcrun simctl list devices -j`.
type Device struct {
	// UDID is the simulator's stable unique identifier (the canonical handle
	// for every lifecycle operation — never an enumeration index, per §11.4.111).
	UDID string
	// Name is the human-readable device name (e.g. "iPhone 14").
	Name string
	// State is "Booted", "Shutdown", "Booting", or "Shutting Down".
	State string
	// Runtime is the SimRuntime identifier the device belongs to
	// (e.g. "com.apple.CoreSimulator.SimRuntime.iOS-17-2").
	Runtime string
	// DeviceTypeIdentifier is the device-type id
	// (e.g. "com.apple.CoreSimulator.SimDeviceType.iPhone-14").
	DeviceTypeIdentifier string
	// IsAvailable reports whether the runtime backing the device is installed.
	IsAvailable bool
}

// IsBooted reports whether the device is currently running.
func (d Device) IsBooted() bool { return strings.EqualFold(strings.TrimSpace(d.State), "Booted") }

// Tool is a handle to a located `xcrun` binary used to drive `simctl`.
type Tool struct {
	// Path is the absolute path to the xcrun executable.
	Path string
	// exec runs `xcrun simctl <args...>` and returns combined stdout+stderr.
	// Injectable for tests; defaults to a real os/exec runner.
	exec func(ctx context.Context, path string, args ...string) (string, error)
}

// NewTool returns a Tool bound to an explicit xcrun path with the real exec
// runner. Used by callers that already know the path (and by Detect).
func NewTool(path string) *Tool {
	return &Tool{Path: path, exec: realExec}
}

func realExec(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Detect locates the `xcrun` binary on PATH and verifies the simctl subsystem
// is reachable. Returns [ErrNotInstalled] when xcrun is absent or simctl fails.
func Detect(ctx context.Context) (*Tool, error) {
	p, err := exec.LookPath("xcrun")
	if err != nil {
		return nil, ErrNotInstalled
	}
	t := NewTool(p)
	// Confirm the simctl subsystem actually answers (rejects non-macOS hosts
	// where an `xcrun` shim might exist but CoreSimulator does not).
	if err := verifySimctl(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// verifySimctl runs `simctl help` to confirm the CoreSimulator subsystem is
// reachable. SIM-4: on failure it wraps [ErrNotInstalled] with the underlying
// cause (%w keeps errors.Is(err, ErrNotInstalled) true) instead of discarding
// the real error, so a help-failed host is DISTINGUISHABLE from a LookPath-
// absent host (which returns the bare sentinel) and the operator sees why.
func verifySimctl(ctx context.Context, t *Tool) error {
	if out, err := t.exec(ctx, t.Path, "simctl", "help"); err != nil {
		return fmt.Errorf("%w: simctl help failed: %v (%s)", ErrNotInstalled, err, strings.TrimSpace(out))
	}
	return nil
}

// Version returns the active Xcode/CoreSimulator version string from
// `xcrun simctl help` is not version-bearing, so we query `xcodebuild -version`
// via xcrun. Returns the first non-empty line (e.g. "Xcode 16.4").
func (t *Tool) Version(ctx context.Context) (string, error) {
	out, err := t.exec(ctx, t.Path, "xcodebuild", "-version")
	if err != nil {
		return "", fmt.Errorf("applesim: xcrun xcodebuild -version failed: %w (%s)", err, strings.TrimSpace(out))
	}
	v := parseVersion(out)
	if v == "" {
		return "", fmt.Errorf("applesim: could not parse version from output: %q", out)
	}
	return v, nil
}

// List returns every simulator device known to simctl (`xcrun simctl list
// devices -j`), booted or not, across every runtime.
func (t *Tool) List(ctx context.Context) ([]Device, error) {
	out, err := t.exec(ctx, t.Path, "simctl", "list", "devices", "-j")
	if err != nil {
		return nil, fmt.Errorf("applesim: xcrun simctl list devices -j failed: %w (%s)", err, strings.TrimSpace(out))
	}
	devices, perr := parseListJSON(out)
	if perr != nil {
		return nil, fmt.Errorf("applesim: parsing simctl device list: %w", perr)
	}
	return devices, nil
}

// Running returns only the devices currently in the Booted state.
func (t *Tool) Running(ctx context.Context) ([]Device, error) {
	all, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	var booted []Device
	for _, d := range all {
		if d.IsBooted() {
			booted = append(booted, d)
		}
	}
	return booted, nil
}

// Resolve returns the device whose UDID or Name matches the given identifier.
// UDID match takes precedence over Name match. Returns an error if no device
// matches. Resolution is by stable identifier per §11.4.111 — never by index.
func (t *Tool) Resolve(ctx context.Context, idOrName string) (Device, error) {
	all, err := t.List(ctx)
	if err != nil {
		return Device{}, err
	}
	// UDID-exact match takes precedence and is inherently unique.
	for _, d := range all {
		if d.UDID == idOrName {
			return d, nil
		}
	}
	// SIM-3 (§11.4.50 determinism + §11.4.111 stable-identifier): simctl
	// routinely lists the SAME device Name under multiple runtimes, and
	// parseListJSON iterates a Go map (randomised order), so a naive first-Name
	// match returned a DIFFERENT UDID across runs. Collect every Name match and
	// select DETERMINISTICALLY — the lexicographically-smallest UDID — so
	// Resolve(name) is stable across runs for the same device set.
	var best Device
	found := false
	for _, d := range all {
		if !strings.EqualFold(d.Name, idOrName) {
			continue
		}
		if !found || d.UDID < best.UDID {
			best = d
			found = true
		}
	}
	if found {
		return best, nil
	}
	return Device{}, fmt.Errorf("applesim: no device matching UDID-or-name %q", idOrName)
}

// Boot boots the device identified by UDID (`xcrun simctl boot <udid>`). It is
// no-op-safe: simctl returns a "current state: Booted" error if already booted,
// which Boot treats as success.
func (t *Tool) Boot(ctx context.Context, udid string) error {
	out, err := t.exec(ctx, t.Path, "simctl", "boot", udid)
	if err != nil {
		// "Unable to boot device in current state: Booted" is benign. SIM-5: the
		// former "current state: Booted" clause is subsumed by "state: Booted"
		// (any string containing the former contains the latter), so one precise
		// phrase match suffices — kept precise, NOT broadened to bare "Booted".
		if strings.Contains(out, "state: Booted") {
			return nil
		}
		return fmt.Errorf("applesim: xcrun simctl boot %q failed: %w (%s)", udid, err, strings.TrimSpace(out))
	}
	return nil
}

// BootAndWait boots the device, then blocks until it is fully booted using
// `xcrun simctl bootstatus <udid> -b` (or the timeout elapses). Returns the
// resolved Device so the caller has its confirmed state.
func (t *Tool) BootAndWait(ctx context.Context, udid string, timeout time.Duration) (Device, error) {
	// SIM-1: derive the bounded context ONCE at the top and thread it through
	// Boot, bootstatus, AND Resolve so `timeout` genuinely bounds the WHOLE
	// operation. Previously only the bootstatus exec was bounded, so a wedged
	// `simctl boot` (Boot) or `simctl list` (Resolve) hung forever on the
	// unbounded caller ctx despite the `timeout` argument.
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := t.Boot(bctx, udid); err != nil {
		return Device{}, err
	}
	// bootstatus blocks until the device finishes booting.
	if out, err := t.exec(bctx, t.Path, "simctl", "bootstatus", udid, "-b"); err != nil {
		return Device{}, fmt.Errorf("applesim: bootstatus %q did not complete within %s: %w (%s)", udid, timeout, err, strings.TrimSpace(out))
	}
	return t.Resolve(bctx, udid)
}

// Install installs an .app bundle onto the device
// (`xcrun simctl install <udid> <appPath>`).
func (t *Tool) Install(ctx context.Context, udid, appPath string) error {
	out, err := t.exec(ctx, t.Path, "simctl", "install", udid, appPath)
	if err != nil {
		return fmt.Errorf("applesim: xcrun simctl install %q %q failed: %w (%s)", udid, appPath, err, strings.TrimSpace(out))
	}
	return nil
}

// Launch launches an installed app by bundle id
// (`xcrun simctl launch <udid> <bundleID>`) and returns the launched PID line
// simctl prints (e.g. "com.example.app: 38941").
func (t *Tool) Launch(ctx context.Context, udid, bundleID string) (string, error) {
	out, err := t.exec(ctx, t.Path, "simctl", "launch", udid, bundleID)
	if err != nil {
		return "", fmt.Errorf("applesim: xcrun simctl launch %q %q failed: %w (%s)", udid, bundleID, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// Terminate stops a running app by bundle id
// (`xcrun simctl terminate <udid> <bundleID>`).
func (t *Tool) Terminate(ctx context.Context, udid, bundleID string) error {
	out, err := t.exec(ctx, t.Path, "simctl", "terminate", udid, bundleID)
	if err != nil {
		return fmt.Errorf("applesim: xcrun simctl terminate %q %q failed: %w (%s)", udid, bundleID, err, strings.TrimSpace(out))
	}
	return nil
}

// Screenshot captures a still image of the device screen to outPath
// (`xcrun simctl io <udid> screenshot <outPath>`).
func (t *Tool) Screenshot(ctx context.Context, udid, outPath string) error {
	out, err := t.exec(ctx, t.Path, "simctl", "io", udid, "screenshot", outPath)
	if err != nil {
		return fmt.Errorf("applesim: xcrun simctl io %q screenshot %q failed: %w (%s)", udid, outPath, err, strings.TrimSpace(out))
	}
	return nil
}

// Recording is a handle to an in-progress `xcrun simctl io recordVideo` process.
// simctl writes the video only when the process receives SIGINT, so Stop()
// sends an interrupt and waits for the file to be flushed to disk.
type Recording struct {
	cmd     *exec.Cmd
	outPath string
	// cancel releases the recorder's own independent context (SIM-2). It is
	// invoked ONLY after the process has already exited via Stop()'s SIGINT +
	// Wait, so its default SIGKILL never reaches a live process (which would
	// truncate the mp4). May be nil for a zero-value/never-started handle.
	cancel context.CancelFunc
}

// OutPath returns the path the recording is being written to.
func (r *Recording) OutPath() string { return r.outPath }

// Stop sends SIGINT to the recordVideo process (the only way simctl flushes the
// file) and waits for it to exit. simctl exits non-zero on SIGINT by design, so
// a signal-terminated exit is treated as success.
func (r *Recording) Stop() error {
	if r.cmd == nil || r.cmd.Process == nil {
		return errors.New("applesim: recording not started")
	}
	// Release the recorder's independent context AFTER the process is finalised
	// below — the process is terminated via SIGINT (which flushes the file),
	// never the context's default SIGKILL (which would truncate it).
	if r.cancel != nil {
		defer r.cancel()
	}
	if err := r.cmd.Process.Signal(interruptSignal); err != nil {
		return fmt.Errorf("applesim: failed to interrupt recordVideo: %w", err)
	}
	// Wait returns an *exec.ExitError on SIGINT termination — expected, not a
	// failure: the video is flushed on interrupt.
	_ = r.cmd.Wait()
	return nil
}

// RecordVideo starts an `xcrun simctl io <udid> recordVideo <outPath>` process
// in the background and returns a [Recording] handle. The caller drives the
// device (launch app, interact) then calls Recording.Stop() to flush the file.
// Unlike the other methods this does NOT route through Tool.exec because it must
// own the live process for signalling; it uses the same Tool.Path binary.
//
// SIM-2 — lifetime boundary (§11.4.107 recording-usability + §11.4.6 honesty):
// the recorder process is bound to a FRESH, INDEPENDENT context owned by the
// returned Recording — NOT the caller's per-call `ctx`. simctl recordVideo only
// flushes/finalises the .mp4 when it receives SIGINT, but a process bound via
// exec.CommandContext is terminated with SIGKILL when its context cancels/
// expires — which would truncate the video the moment the per-call ctx ended,
// even though the returned handle is long-lived and outlives this call. The
// process is therefore terminated ONLY via Recording.Stop() (SIGINT → finalise
// → Wait). The caller MUST call Stop() to end + flush the recording; if it does
// not, the process outlives the call by design (that is the contract of a live
// handle, not a leak the per-call ctx should paper over by killing — and
// truncating — the file). `ctx` is intentionally NOT used to bind the recorder;
// it is retained in the signature for API stability (§11.4.122) and callers
// that need a hard cap can wrap Stop() in their own deadline.
func (t *Tool) RecordVideo(ctx context.Context, udid, outPath string, codec string) (*Recording, error) {
	_ = ctx // see doc comment: the recorder is deliberately detached from the per-call ctx.
	args := []string{"simctl", "io", udid, "recordVideo"}
	if codec != "" {
		args = append(args, "--codec="+codec)
	}
	args = append(args, outPath)
	recCtx, recCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(recCtx, t.Path, args...)
	if err := cmd.Start(); err != nil {
		recCancel()
		return nil, fmt.Errorf("applesim: failed to start recordVideo for %q: %w", udid, err)
	}
	return &Recording{cmd: cmd, outPath: outPath, cancel: recCancel}, nil
}

// Shutdown shuts the device down (`xcrun simctl shutdown <udid>`). It is
// no-op-safe: a "current state: Shutdown" error is treated as success.
func (t *Tool) Shutdown(ctx context.Context, udid string) error {
	out, err := t.exec(ctx, t.Path, "simctl", "shutdown", udid)
	if err != nil {
		// SIM-5: "current state: Shutdown" is subsumed by "state: Shutdown" — one
		// precise phrase match suffices; kept precise, NOT broadened to "Shutdown".
		if strings.Contains(out, "state: Shutdown") {
			return nil
		}
		return fmt.Errorf("applesim: xcrun simctl shutdown %q failed: %w (%s)", udid, err, strings.TrimSpace(out))
	}
	return nil
}

// parseVersion extracts the first non-empty line from `xcodebuild -version`
// output (e.g. "Xcode 16.4\nBuild version 16F6" -> "Xcode 16.4"). Pure.
func parseVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// simctlDeviceList mirrors the JSON shape of `xcrun simctl list devices -j`:
//
//	{ "devices": { "<runtime-id>": [ {udid,name,state,deviceTypeIdentifier,isAvailable}, ... ], ... } }
type simctlDeviceList struct {
	Devices map[string][]simctlDevice `json:"devices"`
}

type simctlDevice struct {
	UDID                 string `json:"udid"`
	Name                 string `json:"name"`
	State                string `json:"state"`
	DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
	IsAvailable          bool   `json:"isAvailable"`
}

// parseListJSON parses `xcrun simctl list devices -j` output into a flat
// []Device, annotating each with its runtime key. Pure function (no exec).
func parseListJSON(out string) ([]Device, error) {
	var raw simctlDeviceList
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	var devices []Device
	for runtime, list := range raw.Devices {
		for _, d := range list {
			devices = append(devices, Device{
				UDID:                 d.UDID,
				Name:                 d.Name,
				State:                d.State,
				Runtime:              runtime,
				DeviceTypeIdentifier: d.DeviceTypeIdentifier,
				IsAvailable:          d.IsAvailable,
			})
		}
	}
	return devices, nil
}
