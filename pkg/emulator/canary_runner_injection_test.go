package emulator

// canary_runner_injection_test.go — anti-bluff tests for the §6.X
// runner-agnostic canary extension (RunADBCommand interface method +
// CanaryConfig.Emu injection point).
//
// Forensic anchor: cmd/emulator-canary previously ALWAYS constructed a
// host-direct NewAndroidEmulator internally, with no way to run the
// canary through the Containerized (podman/docker) runner. On a Linux
// gate host that violates this project's own §6.AH ("virtual devices
// MUST run in containers or VMs — host-direct is forbidden, no
// exception") — and canary.go is the ONLY §6.Z verification path for
// release-signed APKs (instrumented tests cannot run against them,
// signature mismatch), so the gap meant release builds could never be
// gate-verified at all on Linux. These tests prove the fix is real:
// RunCanary genuinely uses an injected Emulator when CanaryConfig.Emu
// is set, rather than silently falling back to host-direct regardless.
//
// Bluff-Audit:
//   Mutation: in RunCanary, changed
//       `if cfg.Emu != nil { emu = cfg.Emu } else { emu = NewAndroidEmulator(...) }`
//     to unconditionally `emu = NewAndroidEmulator(cfg.AndroidSdkRoot)`,
//     discarding cfg.Emu entirely (simulating the pre-fix behavior).
//   Observed: TestRunCanary_UsesInjectedEmulator FAILED — NOT with the
//     originally-predicted "Boot call count: expected 1, got 0" (the
//     test never reaches that assertion), but earlier, at
//     `require.NoError(t, err)`:
//       "Received unexpected error: canary boot: fork/exec
//        /sdk/emulator/emulator: no such file or directory"
//     because the mutated code now genuinely tries to launch a
//     REAL host-direct emulator process (using the test's placeholder
//     "/sdk" AndroidSdkRoot) instead of driving the injected spy. This
//     is still a fully genuine, real falsifiability signal — the test
//     correctly fails when the injection point is broken — the actual
//     failure surface just differs from the first prediction, and is
//     recorded here as actually observed rather than left as a guess,
//     per this project's own anti-bluff documentation standard.
//   Reverted: yes — restored the cfg.Emu != nil branch; re-ran; PASS.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canarySpyEmulator is a minimal Emulator spy that records every call
// so tests can assert RunCanary drove THIS instance (proving
// injection works) rather than a freshly-constructed AndroidEmulator.
type canarySpyEmulator struct {
	bootCalls       int
	waitCalls       int
	installCalls    int
	teardownCalls   int
	adbCommandCalls [][]string // each entry is the args slice of one RunADBCommand call

	// canned responses
	dumpsysOut []byte
	logcatOut  []byte
}

func (s *canarySpyEmulator) Boot(_ context.Context, avd AVD, _ bool) (BootResult, error) {
	s.bootCalls++
	return BootResult{AVD: avd, Started: true, ConsolePort: 5554, ADBPort: 5555}, nil
}

func (s *canarySpyEmulator) WaitForBoot(_ context.Context, _ int, _ time.Duration) (time.Duration, error) {
	s.waitCalls++
	return 0, nil
}

func (s *canarySpyEmulator) Install(_ context.Context, _ int, _ string) error {
	s.installCalls++
	return nil
}

func (s *canarySpyEmulator) RunInstrumentation(_ context.Context, _ int, _ string, _ time.Duration) (string, bool, error) {
	return "", false, nil
}

func (s *canarySpyEmulator) RunADBCommand(_ context.Context, _ int, args ...string) ([]byte, error) {
	s.adbCommandCalls = append(s.adbCommandCalls, append([]string{}, args...))
	// Dispatch canned output by command shape so observeActivityAndLogcat's
	// two distinct calls (dumpsys, logcat) each get a sensible response.
	if len(args) > 0 && args[0] == "shell" && len(args) > 1 && args[1] == "dumpsys" {
		return s.dumpsysOut, nil
	}
	if len(args) > 0 && args[0] == "logcat" {
		return s.logcatOut, nil
	}
	return nil, nil // e.g. the "shell am start" launch call
}

func (s *canarySpyEmulator) Teardown(_ context.Context, _ int) error {
	s.teardownCalls++
	return nil
}

var _ Emulator = (*canarySpyEmulator)(nil)

// TestRunCanary_UsesInjectedEmulator proves RunCanary drives the
// CanaryConfig.Emu instance end-to-end (Boot -> WaitForBoot -> Install
// -> RunADBCommand x2+ -> Teardown) instead of silently constructing
// its own host-direct AndroidEmulator. This is the §6.X gap fix: a
// caller now passes a Containerized instance here for gate runs on
// Linux; this test proves the plumbing with a deterministic spy
// (a real Containerized.Boot needs an actual podman/docker host,
// which is not available in a hermetic unit test — the spy proves
// the SAME interface-driven code path a real Containerized would
// exercise, since RunCanary only ever calls Emulator interface
// methods after the cfg.Emu != nil branch is taken).
func TestRunCanary_UsesInjectedEmulator(t *testing.T) {
	dir := t.TempDir()
	spy := &canarySpyEmulator{
		dumpsysOut: []byte("mResumedActivity: digital.vasic.lava.client/.MainActivity"),
		logcatOut:  []byte("05-01 12:00:01.000  1234 1234 I ActivityManager: START\n"),
	}
	cfg := CanaryConfig{
		AndroidSdkRoot:  "/sdk", // must be ignored — Emu is injected, not constructed from this
		APKPath:         "/releases/app-release.apk",
		PackageName:     "digital.vasic.lava.client",
		LaunchActivity:  ".MainActivity",
		AVD:             AVD{Name: "Pixel_8", APILevel: 35},
		EvidenceDir:     dir,
		ColdBoot:        true,
		ActivityTimeout: 2 * time.Second,
		LogcatWindow:    500 * time.Millisecond,
		Emu:             spy,
	}

	result, err := RunCanary(context.Background(), cfg)
	require.NoError(t, err)

	assert.Equal(t, 1, spy.bootCalls, "Boot call count")
	assert.Equal(t, 1, spy.waitCalls, "WaitForBoot call count")
	assert.Equal(t, 1, spy.installCalls, "Install call count")
	assert.Equal(t, 1, spy.teardownCalls, "Teardown call count")
	require.GreaterOrEqual(t, len(spy.adbCommandCalls), 3,
		"expected at least 3 RunADBCommand calls: am start, dumpsys poll, logcat capture")

	// The FIRST adb command RunCanary issues must be the activity launch.
	assert.Equal(t, []string{"shell", "am", "start", "-n", "digital.vasic.lava.client/.MainActivity"},
		spy.adbCommandCalls[0], "first RunADBCommand call must be the am start launch")

	assert.True(t, result.ActivityResumed)
	assert.False(t, result.FatalDetected)
	assert.True(t, result.Passed)
}

// TestRunCanary_InjectedEmulator_DetectsFatal proves the injected-Emu
// path preserves the anti-bluff primary assertion (crash detection),
// not just plumbing — a canary that "installed fine" through an
// injected Emulator but crashed on launch must still FAIL.
func TestRunCanary_InjectedEmulator_DetectsFatal(t *testing.T) {
	dir := t.TempDir()
	spy := &canarySpyEmulator{
		dumpsysOut: []byte("mResumedActivity: digital.vasic.lava.client/.MainActivity"),
		logcatOut: []byte(
			"05-01 12:00:01.000  1234 1234 E AndroidRuntime: FATAL EXCEPTION: main\n" +
				"05-01 12:00:01.001  1234 1234 E AndroidRuntime: java.lang.NullPointerException\n"),
	}
	cfg := CanaryConfig{
		APKPath:         "/releases/app-release.apk",
		PackageName:     "digital.vasic.lava.client",
		LaunchActivity:  ".MainActivity",
		AVD:             AVD{Name: "Pixel_8", APILevel: 35},
		EvidenceDir:     dir,
		ColdBoot:        true,
		ActivityTimeout: 2 * time.Second,
		LogcatWindow:    500 * time.Millisecond,
		Emu:             spy,
	}

	result, err := RunCanary(context.Background(), cfg)
	require.NoError(t, err)
	assert.True(t, result.FatalDetected, "FATAL EXCEPTION in logcat must be detected through the injected Emu path too")
	assert.False(t, result.Passed, "canary MUST fail on FATAL even via the injected-emulator path")
}

// --- RunADBCommand direct tests (AndroidEmulator + Containerized) ---

// TestAndroidEmulator_RunADBCommand_BuildsCorrectInvocation proves the
// host-direct implementation issues `adb -s localhost:<port> <args>`.
func TestAndroidEmulator_RunADBCommand_BuildsCorrectInvocation(t *testing.T) {
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{
			"/sdk/platform-tools/adb -s localhost:5555 shell getprop sys.boot_completed": {
				Out: []byte("1\n"),
			},
		},
	}
	emu := &AndroidEmulator{executor: exec, androidSdkRoot: "/sdk"}
	out, err := emu.RunADBCommand(context.Background(), 5555, "shell", "getprop", "sys.boot_completed")
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(out))
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "/sdk/platform-tools/adb", exec.calls[0].Name)
	assert.Equal(t, []string{"-s", "localhost:5555", "shell", "getprop", "sys.boot_completed"}, exec.calls[0].Args)
}

// TestContainerized_RunADBCommand_BuildsCorrectInvocation proves the
// containerized implementation issues the identical adb invocation
// shape as AndroidEmulator (same target-format convention), so
// runner-agnostic callers like RunCanary get consistent behavior
// regardless of which runner they were given.
func TestContainerized_RunADBCommand_BuildsCorrectInvocation(t *testing.T) {
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{
			"/usr/bin/adb -s localhost:7654 shell getprop sys.boot_completed": {
				Out: []byte("1\n"),
			},
		},
	}
	c := &Containerized{executor: exec, adbBinaryPath: "/usr/bin/adb"}
	out, err := c.RunADBCommand(context.Background(), 7654, "shell", "getprop", "sys.boot_completed")
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(out))
	require.Len(t, exec.calls, 1)
	assert.Equal(t, "/usr/bin/adb", exec.calls[0].Name)
	assert.Equal(t, []string{"-s", "localhost:7654", "shell", "getprop", "sys.boot_completed"}, exec.calls[0].Args)
}
