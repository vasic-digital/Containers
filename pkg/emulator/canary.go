package emulator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CanaryConfig parameterises a cold-start canary run. A canary installs a
// release-signed APK, launches its main activity, and asserts that:
//
//   - The activity is resumed within ActivityTimeout.
//   - No FATAL / AndroidRuntime exception appears in logcat within the
//     observation window.
//
// This fills the §6.Z tooling gap: debug-signed androidTest APKs cannot
// test a release-signed APK (signature mismatch), so instrumentation
// tests are not an option for release canaries. The canary uses the
// host's `adb shell am start` + logcat to observe the running release
// build without needing a matching test APK.
//
// Anti-bluff posture (clauses 6.J/6.L): the primary assertion is on
// user-visible state — the activity reached RESUMED state OR the logcat
// recorded a FATAL crash. "App installed without error" is NOT a canary
// PASS. Only "activity is running and logcat is clean" is PASS.
type CanaryConfig struct {
	// AndroidSdkRoot is the host path to the Android SDK. The canary
	// uses ${AndroidSdkRoot}/platform-tools/adb.
	AndroidSdkRoot string

	// APKPath is the host path to the release APK to install.
	APKPath string

	// PackageName is the Android package name of the APK
	// (e.g. "digital.vasic.lava.client").
	PackageName string

	// LaunchActivity is the fully-qualified activity name to launch via
	// `adb shell am start -n <PackageName>/<LaunchActivity>`. Must be
	// non-empty — the canary always targets a specific entry point
	// rather than inferring one from the manifest (which would require
	// aapt and add a dependency).
	LaunchActivity string

	// AVD is the target AVD to cold-boot and install the APK on. The
	// canary reuses the existing Boot/WaitForBoot/Install/Teardown
	// lifecycle from AndroidEmulator.
	AVD AVD

	// BootTimeout is the per-AVD cold-boot timeout. Default 5 minutes.
	BootTimeout time.Duration

	// ActivityTimeout is how long the canary waits for the activity to
	// reach RESUMED state. Default 30 seconds.
	ActivityTimeout time.Duration

	// LogcatWindow is the duration of logcat to observe after activity
	// launch. Default 10 seconds.
	LogcatWindow time.Duration

	// EvidenceDir is where the canary writes its attestation JSON and
	// the captured logcat output.
	EvidenceDir string

	// ColdBoot enforces no-snapshot-reload (clause 6.I clause 6) when
	// true. Gate-mode canary runs MUST use true.
	ColdBoot bool
}

// CanaryResult captures the outcome of a single RunCanary invocation.
type CanaryResult struct {
	// APKPath is the release APK that was tested.
	APKPath string `json:"apk_path"`
	// PackageName is the Android package that was launched.
	PackageName string `json:"package_name"`
	// LaunchActivity is the activity entry point that was started.
	LaunchActivity string `json:"launch_activity"`
	// AVD is the target AVD.
	AVDName string `json:"avd_name"`
	// APILevel is the AVD's Android API level.
	APILevel int `json:"api_level,omitempty"`

	// BootDuration is how long cold-booting the emulator took.
	BootDuration time.Duration `json:"boot_duration_seconds"`
	// ActivityResumed is true when the canary observed the activity in
	// RESUMED state within ActivityTimeout.
	ActivityResumed bool `json:"activity_resumed"`
	// FatalDetected is true when the canary observed a FATAL /
	// AndroidRuntime crash in logcat during the observation window.
	FatalDetected bool `json:"fatal_detected"`
	// Passed is true iff ActivityResumed=true AND FatalDetected=false.
	// This is the primary user-visible assertion: the app launched
	// successfully on a cold-booted clean emulator.
	Passed bool `json:"passed"`

	// LogcatOutput is the captured logcat content from the observation
	// window. Truncated to 64KB in this field; full output written to
	// EvidenceDir/logcat.txt.
	LogcatOutput string `json:"logcat_output,omitempty"`
	// Error is the first non-nil error encountered during the run.
	// When non-empty, Passed is always false.
	Error string `json:"error,omitempty"`

	// StartedAt / FinishedAt record wall-clock timing for the attestation.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// AttestationFile is the on-disk path of the written canary
	// attestation JSON.
	AttestationFile string `json:"attestation_file,omitempty"`
}

// RunCanary performs a cold-start canary run on the configured AVD.
// It cold-boots the emulator, installs the release APK, launches the
// activity, observes logcat, and writes a CanaryResult attestation JSON
// to EvidenceDir.
//
// The emulator is always torn down when RunCanary returns (whether the
// canary passed or failed) so the host does not accumulate zombie
// emulators.
//
// Anti-bluff posture (clauses 6.J/6.L): the function's primary
// assertion is on user-visible state — the activity is RESUMED AND
// logcat is FATAL-free. "App installed without error" is NOT a PASS.
func RunCanary(ctx context.Context, cfg CanaryConfig) (CanaryResult, error) {
	startedAt := time.Now()
	result := CanaryResult{
		APKPath:        cfg.APKPath,
		PackageName:    cfg.PackageName,
		LaunchActivity: cfg.LaunchActivity,
		AVDName:        cfg.AVD.Name,
		APILevel:       cfg.AVD.APILevel,
		StartedAt:      startedAt,
	}

	bootTimeout := cfg.BootTimeout
	if bootTimeout == 0 {
		bootTimeout = 5 * time.Minute
	}
	activityTimeout := cfg.ActivityTimeout
	if activityTimeout == 0 {
		activityTimeout = 30 * time.Second
	}
	logcatWindow := cfg.LogcatWindow
	if logcatWindow == 0 {
		logcatWindow = 10 * time.Second
	}

	if err := os.MkdirAll(cfg.EvidenceDir, 0o755); err != nil {
		result.Error = fmt.Sprintf("create evidence dir: %v", err)
		result.FinishedAt = time.Now()
		return result, fmt.Errorf("create evidence dir: %w", err)
	}

	// Pre-boot cleanup: clear any zombie qemu-system-* processes so
	// they don't hold ADB ports our new emulator wants.
	_, _ = Cleanup(ctx)

	// Clear any stale AVD lock file before booting. The lock lives at
	// $HOME/.android/avd/<name>.avd/<name>.lock on most hosts. If an
	// interrupted previous run left it, the emulator refuses to boot.
	clearAVDLock(cfg.AVD.Name)

	emu := NewAndroidEmulator(cfg.AndroidSdkRoot)

	bootResult, err := emu.Boot(ctx, cfg.AVD, cfg.ColdBoot)
	result.BootDuration = bootResult.BootDuration
	if err != nil {
		result.Error = fmt.Sprintf("boot: %v", err)
		result.FinishedAt = time.Now()
		return result, fmt.Errorf("canary boot: %w", err)
	}

	defer func() { _ = emu.Teardown(ctx, bootResult.ADBPort) }()

	if _, err := emu.WaitForBoot(ctx, bootResult.ADBPort, bootTimeout); err != nil {
		result.Error = fmt.Sprintf("wait-for-boot: %v", err)
		result.FinishedAt = time.Now()
		return result, fmt.Errorf("canary wait-for-boot: %w", err)
	}

	// Install the release APK.
	if err := emu.Install(ctx, bootResult.ADBPort, cfg.APKPath); err != nil {
		result.Error = fmt.Sprintf("install: %v", err)
		result.FinishedAt = time.Now()
		return result, fmt.Errorf("canary install: %w", err)
	}

	// Launch the activity.
	adb := emu.adbBinary()
	target := fmt.Sprintf("localhost:%d", bootResult.ADBPort)
	componentSpec := cfg.PackageName + "/" + cfg.LaunchActivity
	launchOut, launchErr := emu.executor.Execute(
		ctx, adb, "-s", target,
		"shell", "am", "start", "-n", componentSpec,
	)
	if launchErr != nil {
		result.Error = fmt.Sprintf("am start: %v (output: %s)", launchErr, launchOut)
		result.FinishedAt = time.Now()
		return result, fmt.Errorf("canary am start: %w", launchErr)
	}

	// Poll for the activity to reach RESUMED state. `adb shell dumpsys
	// activity | grep mResumedActivity` is the canonical check:
	// when the package appears in mResumedActivity the activity is
	// on-screen and interactive.
	resumed, fatalDetected, logcatOutput := observeActivityAndLogcat(
		ctx, emu, target, adb, cfg.PackageName, activityTimeout, logcatWindow,
	)
	result.ActivityResumed = resumed
	result.FatalDetected = fatalDetected
	result.Passed = resumed && !fatalDetected

	// Persist logcat output.
	const maxLogcatInResult = 64 * 1024
	if len(logcatOutput) <= maxLogcatInResult {
		result.LogcatOutput = logcatOutput
	} else {
		result.LogcatOutput = logcatOutput[:maxLogcatInResult] + "\n...(truncated)"
	}
	logcatPath := filepath.Join(cfg.EvidenceDir, "logcat.txt")
	_ = os.WriteFile(logcatPath, []byte(logcatOutput), 0o644)

	result.FinishedAt = time.Now()

	// Write attestation JSON.
	attestationPath := filepath.Join(cfg.EvidenceDir, "canary-attestation.json")
	if err := writeCanaryAttestation(attestationPath, result); err == nil {
		result.AttestationFile = attestationPath
	}

	return result, nil
}

// observeActivityAndLogcat polls dumpsys activity for the package's
// RESUMED state and captures logcat during the observation window.
// Returns (resumed, fatalDetected, logcatOutput).
//
// Anti-bluff posture: "no crash" is observed positively via dumpsys
// + logcat; the function does NOT short-circuit on install success.
func observeActivityAndLogcat(
	ctx context.Context,
	emu *AndroidEmulator,
	target, adb, packageName string,
	activityTimeout, logcatWindow time.Duration,
) (resumed, fatalDetected bool, logcatOutput string) {
	deadline := time.Now().Add(activityTimeout)

	// First: poll dumpsys activity until the package appears as resumed
	// or the timeout fires.
	for time.Now().Before(deadline) {
		dumpsysOut, err := emu.executor.Execute(
			ctx, adb, "-s", target,
			"shell", "dumpsys", "activity", "activities",
		)
		if err == nil {
			for _, line := range strings.Split(string(dumpsysOut), "\n") {
				if strings.Contains(line, "mResumedActivity") && strings.Contains(line, packageName) {
					resumed = true
					break
				}
			}
		}
		if resumed {
			break
		}
		select {
		case <-ctx.Done():
			return false, false, ""
		case <-time.After(1 * time.Second):
		}
	}

	// Second: capture logcat for the observation window, looking for
	// FATAL or AndroidRuntime entries.
	logcatCtx, cancel := context.WithTimeout(ctx, logcatWindow)
	defer cancel()
	logOut, _ := emu.executor.Execute(
		logcatCtx, adb, "-s", target,
		"logcat", "-d", "-v", "threadtime",
		"AndroidRuntime:E", "*:F",
	)
	logcatOutput = string(logOut)

	// Detect crash markers in the captured logcat.
	for _, line := range strings.Split(logcatOutput, "\n") {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "FATAL EXCEPTION") ||
			strings.Contains(upper, "ANDROIDRUNTIME") && strings.Contains(upper, " E ") {
			fatalDetected = true
			break
		}
	}

	return resumed, fatalDetected, logcatOutput
}

// writeCanaryAttestation serialises a CanaryResult to a JSON file.
func writeCanaryAttestation(path string, r CanaryResult) error {
	type doc struct {
		StartedAt       string `json:"started_at"`
		FinishedAt      string `json:"finished_at"`
		APKPath         string `json:"apk_path"`
		PackageName     string `json:"package_name"`
		LaunchActivity  string `json:"launch_activity"`
		AVD             string `json:"avd"`
		APILevel        int    `json:"api_level,omitempty"`
		BootSeconds     float64 `json:"boot_seconds"`
		ActivityResumed bool   `json:"activity_resumed"`
		FatalDetected   bool   `json:"fatal_detected"`
		Passed          bool   `json:"passed"`
		Error           string `json:"error,omitempty"`
	}
	d := doc{
		StartedAt:       r.StartedAt.Format(time.RFC3339),
		FinishedAt:      r.FinishedAt.Format(time.RFC3339),
		APKPath:         r.APKPath,
		PackageName:     r.PackageName,
		LaunchActivity:  r.LaunchActivity,
		AVD:             r.AVDName,
		APILevel:        r.APILevel,
		BootSeconds:     r.BootDuration.Seconds(),
		ActivityResumed: r.ActivityResumed,
		FatalDetected:   r.FatalDetected,
		Passed:          r.Passed,
		Error:           r.Error,
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// clearAVDLock removes the AVD lock file that the Android emulator
// writes when it starts and removes when it shuts down cleanly. A
// lock left by an interrupted run prevents the next boot.
//
// The lock file is at $HOME/.android/avd/<name>.avd/<name>.lock.
// Best-effort — failure to remove is NOT an error (the lock may
// not exist, or the emulator may manage its own lock format on newer
// SDK versions).
func clearAVDLock(avdName string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	lockPath := filepath.Join(
		home, ".android", "avd",
		avdName+".avd",
		avdName+".lock",
	)
	_ = os.Remove(lockPath)
}
