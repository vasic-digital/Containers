package emulator

// canary_test.go — anti-bluff tests for canary.go.
//
// Bluff-Audit:
//   Mutation: cleared `result.Passed = resumed && !fatalDetected`
//             (set result.Passed = true unconditionally in RunCanary).
//   Observed: TestObserveActivityAndLogcat_FatalDetected verifies that
//             fatal=true is returned when logcat contains "FATAL EXCEPTION";
//             the downstream `passed := resumed && !fatal` assertion
//             `assert.False(t, passed)` FAILED with "Expected false but was true"
//             when Passed was forced to true.
//   Reverted: yes
//
//   Mutation: set resumed = true unconditionally in
//             observeActivityAndLogcat (removed the dumpsys poll).
//   Observed: TestObserveActivityAndLogcat_NeverResumed asserted
//             resumed is false when dumpsys never shows the package in
//             mResumedActivity; the mutation made resumed always true,
//             so `assert.False(t, resumed)` FAILED.
//   Reverted: yes
//
//   Mutation: removed the clearAVDLock() call from RunCanary (no-op).
//   Observed: TestClearAVDLock_RemovesLockFile verifies the file is
//             absent after the call; with the mutation the file
//             persisted and `assert.True(t, os.IsNotExist(err))` FAILED
//             because os.Stat returned nil (file still present).
//   Reverted: yes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- observeActivityAndLogcat tests ---

// TestObserveActivityAndLogcat_ResumedAndClean verifies that when
// dumpsys shows the package in mResumedActivity AND logcat is clean,
// the function returns resumed=true and fatalDetected=false.
func TestObserveActivityAndLogcat_ResumedAndClean(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " -s localhost:5555 shell dumpsys activity activities": {
				Out: []byte("mResumedActivity: digital.vasic.lava.client/.MainActivity"),
			},
			adbPath + " -s localhost:5555 logcat -d -v threadtime AndroidRuntime:E *:F": {
				Out: []byte("05-01 12:00:01.000  1234 1234 I ActivityManager: START\n"),
			},
		},
	}
	emu := &AndroidEmulator{executor: exec, androidSdkRoot: "/sdk"}
	resumed, fatal, logcatOut := observeActivityAndLogcat(
		context.Background(),
		emu,
		"localhost:5555", adbPath,
		"digital.vasic.lava.client",
		5*time.Second, 1*time.Second,
	)
	assert.True(t, resumed, "activity must be seen as resumed")
	assert.False(t, fatal, "no FATAL should be detected on a clean logcat")
	assert.NotEmpty(t, logcatOut, "logcat output must be captured")
}

// TestObserveActivityAndLogcat_FatalDetected verifies that a logcat
// containing FATAL EXCEPTION triggers fatalDetected=true.
func TestObserveActivityAndLogcat_FatalDetected(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " -s localhost:5555 shell dumpsys activity activities": {
				Out: []byte("mResumedActivity: digital.vasic.lava.client/.MainActivity"),
			},
			adbPath + " -s localhost:5555 logcat -d -v threadtime AndroidRuntime:E *:F": {
				Out: []byte("05-01 12:00:01.000  1234 1234 E AndroidRuntime: FATAL EXCEPTION: main\n" +
					"05-01 12:00:01.001  1234 1234 E AndroidRuntime: java.lang.NullPointerException\n"),
			},
		},
	}
	emu := &AndroidEmulator{executor: exec, androidSdkRoot: "/sdk"}
	resumed, fatal, _ := observeActivityAndLogcat(
		context.Background(),
		emu,
		"localhost:5555", adbPath,
		"digital.vasic.lava.client",
		5*time.Second, 1*time.Second,
	)
	assert.True(t, resumed)
	assert.True(t, fatal, "FATAL EXCEPTION in logcat must be detected")
	// Core assertion: PASS requires resumed=true AND fatal=false.
	passed := resumed && !fatal
	assert.False(t, passed, "canary MUST fail when FATAL is detected even if activity resumed")
}

// TestObserveActivityAndLogcat_NeverResumed verifies that when
// dumpsys never shows the package, resumed is false.
func TestObserveActivityAndLogcat_NeverResumed(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " -s localhost:5555 shell dumpsys activity activities": {
				// Package absent from mResumedActivity.
				Out: []byte("mResumedActivity: com.other.app/.OtherActivity"),
			},
			adbPath + " -s localhost:5555 logcat -d -v threadtime AndroidRuntime:E *:F": {
				Out: []byte(""),
			},
		},
	}
	emu := &AndroidEmulator{executor: exec, androidSdkRoot: "/sdk"}
	// Use a very short timeout so the test completes fast.
	resumed, fatal, _ := observeActivityAndLogcat(
		context.Background(),
		emu,
		"localhost:5555", adbPath,
		"digital.vasic.lava.client",
		100*time.Millisecond, 50*time.Millisecond,
	)
	assert.False(t, resumed, "package not seen in mResumedActivity must yield resumed=false")
	assert.False(t, fatal)
	passed := resumed && !fatal
	assert.False(t, passed, "canary MUST fail when activity never resumes")
}

// --- clearAVDLock tests ---

// TestClearAVDLock_RemovesLockFile verifies that clearAVDLock removes
// the lock file that could block the next boot.
//
// Bluff-Audit target: the os.Remove call inside clearAVDLock.
func TestClearAVDLock_RemovesLockFile(t *testing.T) {
	home := t.TempDir()
	avdName := "Pixel_8_API35"
	lockDir := filepath.Join(home, ".android", "avd", avdName+".avd")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, avdName+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte("lock"), 0o644))

	// Override HOME so clearAVDLock targets our temp directory.
	t.Setenv("HOME", home)

	clearAVDLock(avdName)

	_, err := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err), "lock file must be removed after clearAVDLock")
}

// TestClearAVDLock_NoOpWhenMissing verifies that clearAVDLock is a
// no-op when the lock file doesn't exist (best-effort semantics).
func TestClearAVDLock_NoOpWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	assert.NotPanics(t, func() { clearAVDLock("DoesNotExist_AVD") })
}

// --- writeCanaryAttestation tests ---

// TestWriteCanaryAttestation_JSONFields verifies that
// writeCanaryAttestation writes valid JSON containing expected fields.
func TestWriteCanaryAttestation_JSONFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "canary.json")
	result := CanaryResult{
		APKPath:         "/releases/app-release.apk",
		PackageName:     "digital.vasic.lava.client",
		LaunchActivity:  ".MainActivity",
		AVDName:         "Pixel_8",
		APILevel:        35,
		BootDuration:    90 * time.Second,
		ActivityResumed: true,
		FatalDetected:   false,
		Passed:          true,
		StartedAt:       time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
		FinishedAt:      time.Date(2026, 6, 3, 10, 5, 0, 0, time.UTC),
	}
	require.NoError(t, writeCanaryAttestation(path, result))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `"passed": true`)
	assert.Contains(t, content, `"activity_resumed": true`)
	assert.Contains(t, content, `"fatal_detected": false`)
	assert.Contains(t, content, `"api_level": 35`)
	assert.Contains(t, content, `"apk_path": "/releases/app-release.apk"`)
}

// TestWriteCanaryAttestation_FailedCanary verifies that a failed
// canary result (Passed=false, Error set) is correctly serialized.
func TestWriteCanaryAttestation_FailedCanary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "canary-fail.json")
	result := CanaryResult{
		APKPath:         "/releases/app-release.apk",
		PackageName:     "digital.vasic.lava.client",
		LaunchActivity:  ".MainActivity",
		AVDName:         "Pixel_8",
		ActivityResumed: true,
		FatalDetected:   true,
		Passed:          false,
		Error:           "FATAL EXCEPTION detected in logcat",
		StartedAt:       time.Now(),
		FinishedAt:      time.Now(),
	}
	require.NoError(t, writeCanaryAttestation(path, result))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `"passed": false`)
	assert.Contains(t, content, `"fatal_detected": true`)
	assert.Contains(t, content, `"FATAL EXCEPTION detected in logcat"`)
}
