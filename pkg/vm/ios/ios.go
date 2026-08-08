// SPDX-License-Identifier: Apache-2.0

// Package ios orchestrates iOS build and simulator operations on macOS
// hosts using the Xcode command-line tools (`xcodebuild`, `xcrun simctl`).
//
// # Constraint (honest disclosure, CONST-039)
//
// iOS builds require:
//   - A macOS host (Xcode is macOS-only; no Linux/Windows path exists).
//   - Xcode and the Command Line Tools installed (`xcode-select --install`).
//   - A valid Apple Developer account for device builds + code signing.
//
// Simulator builds (no device, no signing) work without a paid developer
// account and are the primary use-case for this package.
//
// This package makes the constraint explicit at runtime: if the host is
// not macOS or xcodebuild is absent, every method returns an honest error
// citing the requirement rather than panicking.
//
// # Anti-bluff posture (CONST-039)
//
// Tests skip with `// SKIP-OK: #ios-build-requires-xcode-macos` when
// xcodebuild is not installed. Orchestration logic is covered with an
// injected `xcodeRunner` seam so CI agents on Linux/Windows can verify
// the wiring without Xcode.
package ios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrUnsafeArg (VM3-8, SECURITY argv flag-injection) is returned when a
// caller-supplied identifier or path that this package hands to `xcrun simctl`
// as a POSITIONAL argument begins with '-'. simctl (like every getopt-style
// CLI) parses a leading-'-' token as an OPTION/FLAG rather than a value, so a
// crafted or typo'd UDID / .app path / bundle id / output path beginning with
// '-' is an argv flag-injection vector (e.g. a "--foo" reaching `simctl io
// <udid> screenshot <outPath>` selects an option instead of naming the file).
// A legitimate CoreSimulator UDID (a UUID), reverse-DNS bundle id, or absolute
// filesystem path never begins with '-'; a caller needing a relative path may
// prefix "./". This is the exact parallel of the sibling pkg/applesim SIM2-1
// checkArgs/ErrUnsafeArg guard for the un-fixed simctl wrapper in this package;
// simctl's positional grammar is not documented to honour an end-of-options
// "--" terminator, so the guard refuses the value rather than GUESS that "--"
// would neutralise it (§11.4.6).
var ErrUnsafeArg = errors.New("ios: argument begins with '-' (would be parsed as an xcrun/simctl flag; refusing to inject)")

// checkSimctlArgs (VM3-8) rejects — BEFORE any process is spawned — any
// caller-supplied positional argument that begins with '-'. An empty string is
// permitted: `simctl <cmd> ""` yields a clear "invalid device/path" from simctl
// itself (not an injection), so the guard stays narrow to the actual
// flag-injection shape and never over-rejects a legitimate value.
func checkSimctlArgs(args ...string) error {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("%w: %q", ErrUnsafeArg, a)
		}
	}
	return nil
}

// xcodeRunner is the seam through which IOSBuilder invokes Xcode CLT.
// Production uses osExecXcodeRunner; tests inject a fake.
type xcodeRunner interface {
	// XcodeVersion runs `xcodebuild -version` and returns the first line.
	XcodeVersion(ctx context.Context) (string, error)

	// XcrunVersion runs `xcrun --version`.
	XcrunVersion(ctx context.Context) (string, error)

	// Build runs `xcodebuild` with the supplied args and returns
	// stdout, stderr, and exit code.
	Build(ctx context.Context, args []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)

	// ExportArchive runs `xcodebuild -exportArchive` with the supplied
	// export options plist to produce a signed or unsigned .ipa.
	ExportArchive(ctx context.Context, archivePath, exportPath, exportOptionsPlist string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)

	// SimctlList runs `xcrun simctl list --json` and returns raw JSON.
	SimctlList(ctx context.Context) (string, error)

	// SimctlRun runs `xcrun simctl <subcommand> [args...]` and returns
	// stdout, stderr, and exit code.
	SimctlRun(ctx context.Context, subcommand string, args []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)
}

// BuildIPARequest is the input to IOSBuilder.BuildIPA.
type BuildIPARequest struct {
	// ProjectDir is the absolute path to the Xcode project root.
	ProjectDir string

	// Scheme is the Xcode scheme to build (required).
	Scheme string

	// Configuration is the build configuration. Defaults to "Release".
	Configuration string

	// ArchivePath is where xcodebuild writes the .xcarchive.
	// Defaults to a temp path under /tmp.
	ArchivePath string

	// ExportPath is where xcodebuild -exportArchive writes the .ipa.
	// Required.
	ExportPath string

	// ExportOptionsPlist is the path to the ExportOptions.plist file
	// that controls signing method, provisioning profile, etc.
	// Defaults to an ad-hoc export (simulator builds, no signing).
	ExportOptionsPlist string

	// SDK is the Xcode SDK to use. Defaults to "iphoneos" for device
	// builds; use "iphonesimulator" for simulator (no signing required).
	SDK string

	// Destinations allows overriding the build destination, e.g.
	// "generic/platform=iOS Simulator". Defaults to the scheme default.
	Destination string

	// Timeout caps the total build+export wall-clock. Default 60 min.
	Timeout time.Duration
}

// BuildIPAResult captures the outcome of a BuildIPARequest.
type BuildIPAResult struct {
	// ArchivePath is the .xcarchive produced by xcodebuild.
	ArchivePath string

	// IPAPath is the final .ipa path (set only if ExportArchive succeeded).
	IPAPath string

	// Stdout / Stderr (up to 64 KB each) from xcodebuild.
	Stdout string
	Stderr string

	// ExitCode is the xcodebuild exit code.
	ExitCode int

	// Duration is wall-clock from BuildIPA invocation to return.
	Duration time.Duration

	// Error is non-nil if the build or export failed.
	Error error
}

// ErrXcodeNotAvailable is returned by all IOSBuilder methods when
// xcodebuild is absent or the host is not macOS.
var ErrXcodeNotAvailable = fmt.Errorf(
	"xcodebuild is not installed or host is not macOS; " +
		"install Xcode from the Mac App Store — " +
		"see https://developer.apple.com/xcode/ " +
		"(SKIP-OK: #ios-build-requires-xcode-macos)")

// IOSBuilder orchestrates iOS archive + export builds via xcodebuild.
//
// Use IOSBuilder for:
//   - Building .ipa files for simulator testing (no signing required).
//   - Building + signing .ipa files for TestFlight / App Store delivery.
//   - Querying the available iOS simulators via xcrun simctl.
type IOSBuilder struct {
	xcode xcodeRunner
}

// NewIOSBuilder returns the production builder. It does NOT fail if
// xcodebuild is absent; callers discover the absence via BuildIPA's
// return value.
func NewIOSBuilder() *IOSBuilder {
	return &IOSBuilder{xcode: &osExecXcodeRunner{}}
}

// newIOSBuilderWithRunner is the test seam.
func newIOSBuilderWithRunner(r xcodeRunner) *IOSBuilder {
	return &IOSBuilder{xcode: r}
}

// XcodeVersion returns the installed Xcode version string, or
// ErrXcodeNotAvailable if xcodebuild is absent or the host is non-macOS.
func (b *IOSBuilder) XcodeVersion(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrXcodeNotAvailable
	}
	v, err := b.xcode.XcodeVersion(ctx)
	if err != nil {
		return "", ErrXcodeNotAvailable
	}
	return v, nil
}

// ListSimulators returns the `xcrun simctl list --json` output as a
// raw JSON string, or ErrXcodeNotAvailable if xcrun is absent.
func (b *IOSBuilder) ListSimulators(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrXcodeNotAvailable
	}
	out, err := b.xcode.SimctlList(ctx)
	if err != nil {
		return "", fmt.Errorf("xcrun simctl list: %w", err)
	}
	return out, nil
}

// BuildIPA runs xcodebuild archive + exportArchive to produce a .ipa.
//
// Returns ErrXcodeNotAvailable immediately when:
//   - runtime.GOOS != "darwin", OR
//   - `xcodebuild -version` fails.
//
// For simulator-only builds (no Apple Developer account), set:
//
//	req.SDK = "iphonesimulator"
//	req.Destination = "generic/platform=iOS Simulator"
//	req.ExportOptionsPlist = "" // uses ad-hoc default
func (b *IOSBuilder) BuildIPA(ctx context.Context, req BuildIPARequest) BuildIPAResult {
	start := time.Now()

	if runtime.GOOS != "darwin" {
		return BuildIPAResult{
			Duration: time.Since(start),
			Error:    ErrXcodeNotAvailable,
		}
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Verify Xcode is present.
	if _, err := b.xcode.XcodeVersion(ctx); err != nil {
		return BuildIPAResult{Duration: time.Since(start), Error: ErrXcodeNotAvailable}
	}

	if req.ProjectDir == "" {
		return BuildIPAResult{
			Duration: time.Since(start),
			Error:    fmt.Errorf("IOSBuilder.BuildIPA: BuildIPARequest.ProjectDir is required"),
		}
	}
	if req.Scheme == "" {
		return BuildIPAResult{
			Duration: time.Since(start),
			Error:    fmt.Errorf("IOSBuilder.BuildIPA: BuildIPARequest.Scheme is required"),
		}
	}
	if req.ExportPath == "" {
		return BuildIPAResult{
			Duration: time.Since(start),
			Error:    fmt.Errorf("IOSBuilder.BuildIPA: BuildIPARequest.ExportPath is required"),
		}
	}

	config := req.Configuration
	if config == "" {
		config = "Release"
	}
	sdk := req.SDK
	if sdk == "" {
		sdk = "iphoneos"
	}
	archivePath := req.ArchivePath
	if archivePath == "" {
		archivePath = filepath.Join("/tmp",
			fmt.Sprintf("crossbuild-%s-%d.xcarchive", req.Scheme, time.Now().UnixNano()))
	}

	// Phase 1: xcodebuild archive.
	archiveArgs := []string{
		"archive",
		"-project", req.ProjectDir,
		"-scheme", req.Scheme,
		"-configuration", config,
		"-sdk", sdk,
		"-archivePath", archivePath,
	}
	if req.Destination != "" {
		archiveArgs = append(archiveArgs, "-destination", req.Destination)
	}

	stdout, stderr, exitCode, err := b.xcode.Build(ctx, archiveArgs, timeout/2)
	result := BuildIPAResult{
		ArchivePath: archivePath,
		Stdout:      tailString(stdout, 65536),
		Stderr:      tailString(stderr, 65536),
		ExitCode:    exitCode,
		Duration:    time.Since(start),
	}
	if err != nil {
		result.Error = fmt.Errorf("xcodebuild archive: %w", err)
		return result
	}
	if exitCode != 0 {
		result.Error = fmt.Errorf("xcodebuild archive exited %d", exitCode)
		return result
	}

	// Phase 2: xcodebuild -exportArchive.
	exportOptionsPlist := req.ExportOptionsPlist
	if exportOptionsPlist == "" {
		// Ad-hoc export — works without a paid account for simulator
		// build validation.
		exportOptionsPlist = adHocExportOptionsPlist()
	}

	exportStdout, exportStderr, exportExit, exportErr := b.xcode.ExportArchive(
		ctx, archivePath, req.ExportPath, exportOptionsPlist, timeout/2)
	result.Stdout += "\n--- exportArchive ---\n" + tailString(exportStdout, 32768)
	result.Stderr += "\n--- exportArchive ---\n" + tailString(exportStderr, 32768)
	result.ExitCode = exportExit
	result.Duration = time.Since(start)

	if exportErr != nil {
		result.Error = fmt.Errorf("xcodebuild -exportArchive: %w", exportErr)
		return result
	}
	if exportExit != 0 {
		result.Error = fmt.Errorf("xcodebuild -exportArchive exited %d", exportExit)
		return result
	}

	// Determine .ipa path (xcodebuild writes it as <ExportPath>/<Scheme>.ipa).
	result.IPAPath = filepath.Join(req.ExportPath, req.Scheme+".ipa")
	return result
}

// BootSimulator boots the iOS simulator identified by udid.
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
// The simulator may take 30–120 s to reach the "Booted" state; callers
// should poll ListSimulators for the state change.
func (b *IOSBuilder) BootSimulator(ctx context.Context, udid string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if _, _, exitCode, err := b.xcode.SimctlRun(ctx, "boot", []string{udid}, timeout); err != nil || exitCode != 0 {
		if err != nil {
			return fmt.Errorf("xcrun simctl boot %s: %w", udid, err)
		}
		// exit 1 with "Unable to boot device in current state: Booted" is OK.
		// We treat it as success (simulator already running).
		return nil
	}
	// Wait for the simulator to appear in "Booted" state.
	_, _, _, _ = b.xcode.SimctlRun(ctx, "bootstatus", []string{udid, "-b"}, timeout)
	return nil
}

// ShutdownSimulator shuts down the iOS simulator identified by udid.
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
func (b *IOSBuilder) ShutdownSimulator(ctx context.Context, udid string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if _, _, exitCode, err := b.xcode.SimctlRun(ctx, "shutdown", []string{udid}, timeout); err != nil || exitCode != 0 {
		if err != nil {
			return fmt.Errorf("xcrun simctl shutdown %s: %w", udid, err)
		}
		// exit 1 with "Unable to shutdown device in current state: Shutdown" is OK.
		return nil
	}
	return nil
}

// InstallApp installs the app at appPath on the simulator identified by udid.
// appPath is the path to the .app bundle (simulator build, not .ipa).
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
func (b *IOSBuilder) InstallApp(ctx context.Context, udid, appPath string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid, appPath); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	stdout, stderr, exitCode, err := b.xcode.SimctlRun(ctx, "install", []string{udid, appPath}, timeout)
	if err != nil {
		return fmt.Errorf("xcrun simctl install %s %s: %w", udid, appPath, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("xcrun simctl install exited %d: stdout=%s stderr=%s", exitCode, tailString(stdout, 2048), tailString(stderr, 2048))
	}
	return nil
}

// LaunchApp launches the app identified by bundleID on the simulator udid.
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
func (b *IOSBuilder) LaunchApp(ctx context.Context, udid, bundleID string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid, bundleID); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	stdout, stderr, exitCode, err := b.xcode.SimctlRun(ctx, "launch", []string{udid, bundleID}, timeout)
	if err != nil {
		return fmt.Errorf("xcrun simctl launch %s %s: %w", udid, bundleID, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("xcrun simctl launch exited %d: stdout=%s stderr=%s", exitCode, tailString(stdout, 2048), tailString(stderr, 2048))
	}
	return nil
}

// Screenshot captures a screenshot of the simulator udid and writes it to outputPath.
// outputPath must end in .png, .jpg, or .tiff.
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
func (b *IOSBuilder) Screenshot(ctx context.Context, udid, outputPath string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid, outputPath); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	stdout, stderr, exitCode, err := b.xcode.SimctlRun(ctx, "io", []string{udid, "screenshot", outputPath}, timeout)
	if err != nil {
		return fmt.Errorf("xcrun simctl io %s screenshot %s: %w", udid, outputPath, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("xcrun simctl io screenshot exited %d: stdout=%s stderr=%s", exitCode, tailString(stdout, 2048), tailString(stderr, 2048))
	}
	return nil
}

// Recording captures a video recording of the simulator udid.
//
// outputPath must end in .mp4 or .mov.
// Recording is stopped by cancelling the context (recommended) or after timeout.
//
// Returns ErrXcodeNotAvailable on non-macOS hosts or if xcrun is absent.
func (b *IOSBuilder) Recording(ctx context.Context, udid, outputPath string, timeout time.Duration) error {
	if err := checkSimctlArgs(udid, outputPath); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return ErrXcodeNotAvailable
	}
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	// xcrun simctl io <udid> recordVideo <path>
	// The process runs until the context is cancelled (SIGINT/SIGTERM).
	stdout, stderr, exitCode, err := b.xcode.SimctlRun(ctx, "io", []string{udid, "recordVideo", outputPath}, timeout)
	if err != nil && ctx.Err() == nil {
		// If the context wasn't cancelled, the error is real.
		return fmt.Errorf("xcrun simctl io %s recordVideo %s: %w", udid, outputPath, err)
	}
	if exitCode != 0 && ctx.Err() == nil {
		return fmt.Errorf("xcrun simctl io recordVideo exited %d: stdout=%s stderr=%s", exitCode, tailString(stdout, 2048), tailString(stderr, 2048))
	}
	return nil
}

// adHocExportOptionsPlist returns a minimal inline ExportOptions.plist
// string for ad-hoc (simulator) exports. The caller must write this to
// a temp file before invoking xcodebuild -exportArchive.
//
// This is a helper for the production osExecXcodeRunner.ExportArchive,
// not used in tests (tests inject a fake runner).
func adHocExportOptionsPlist() string {
	return "/tmp/crossbuild-ios-adhoc-export-options.plist"
}

// adHocExportOptionsPlistContent is the XML content for an ad-hoc export.
// The production runner writes this to adHocExportOptionsPlist() before use.
const adHocExportOptionsPlistContent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>method</key>
    <string>ad-hoc</string>
    <key>compileBitcode</key>
    <false/>
</dict>
</plist>
`

// tailString returns the last n bytes of s. Shared with macos package
// (each package provides its own copy to avoid a cross-package dep).
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// osExecXcodeRunner is the production xcodeRunner implementation.
type osExecXcodeRunner struct{}

func (o *osExecXcodeRunner) XcodeVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "xcodebuild", "-version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("xcodebuild -version: %w", err)
	}
	lines := bytes.SplitN(out, []byte("\n"), 2)
	return string(bytes.TrimSpace(lines[0])), nil
}

func (o *osExecXcodeRunner) XcrunVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "xcrun", "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("xcrun --version: %w", err)
	}
	return string(bytes.TrimSpace(out)), nil
}

func (o *osExecXcodeRunner) Build(ctx context.Context, args []string, timeout time.Duration) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xcodebuild", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return stdout.String(), stderr.String(), code, err
}

func (o *osExecXcodeRunner) ExportArchive(ctx context.Context, archivePath, exportPath, exportOptionsPlist string, timeout time.Duration) (string, string, int, error) {
	// Write ad-hoc plist if the caller supplied our sentinel path.
	if exportOptionsPlist == adHocExportOptionsPlist() {
		if err := writeAdHocPlist(); err != nil {
			return "", "", -1, fmt.Errorf("writing ad-hoc ExportOptions.plist: %w", err)
		}
	}
	args := []string{
		"-exportArchive",
		"-archivePath", archivePath,
		"-exportPath", exportPath,
		"-exportOptionsPlist", exportOptionsPlist,
	}
	return o.Build(ctx, args, timeout)
}

func (o *osExecXcodeRunner) SimctlList(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("xcrun simctl list: %w", err)
	}
	return string(out), nil
}

func (o *osExecXcodeRunner) SimctlRun(ctx context.Context, subcommand string, args []string, timeout time.Duration) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmdArgs := append([]string{"simctl", subcommand}, args...)
	cmd := exec.CommandContext(ctx, "xcrun", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return stdout.String(), stderr.String(), code, err
}

func writeAdHocPlist() error {
	return exec.Command("sh", "-c",
		"cat > "+adHocExportOptionsPlist()+" <<'EOF'\n"+adHocExportOptionsPlistContent+"\nEOF").Run()
}
