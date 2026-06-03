package emulator

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/cache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExecutor records every (name, args) invocation and returns
// caller-supplied canned output. Anti-bluff posture (clauses 6.J/6.L
// inherited from Containers' parent constitution): the fake is
// behaviorally equivalent to os/exec for the assertions we care about
// — it captures the same arguments the real executor would receive,
// and it returns bytes the real executor would also return for the
// scripted scenario. A test that asserts on `fake.calls` is asserting
// on the same observable behaviour that a real-shell smoke test would
// see in the host's process accounting.
type fakeExecutor struct {
	calls   []fakeCall
	scripts map[string]fakeScript
	// sequencedScripts returns successive outputs for repeated calls
	// to the same command key, simulating real adb output that changes
	// between invocations (e.g. `adb devices` when a new emulator
	// appears mid-test). The last entry repeats after exhaustion.
	sequencedScripts map[string][]fakeScript
	seqIdx           map[string]int
}

type fakeCall struct {
	Name string
	Args []string
}

type fakeScript struct {
	Out []byte
	Err error
}

func (f *fakeExecutor) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{Name: name, Args: append([]string{}, args...)})
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	// Sequenced scripts take precedence — each call returns the next
	// entry in the sequence (last entry repeats forever).
	if seq, ok := f.sequencedScripts[key]; ok && len(seq) > 0 {
		if f.seqIdx == nil {
			f.seqIdx = map[string]int{}
		}
		idx := f.seqIdx[key]
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		f.seqIdx[key] = f.seqIdx[key] + 1
		return seq[idx].Out, seq[idx].Err
	}
	if seq, ok := f.sequencedScripts[name]; ok && len(seq) > 0 {
		if f.seqIdx == nil {
			f.seqIdx = map[string]int{}
		}
		idx := f.seqIdx[name]
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		f.seqIdx[name] = f.seqIdx[name] + 1
		return seq[idx].Out, seq[idx].Err
	}
	if s, ok := f.scripts[key]; ok {
		return s.Out, s.Err
	}
	if s, ok := f.scripts[name]; ok {
		return s.Out, s.Err
	}
	return nil, nil
}

// adbDevicesScript is a fakeExecutor convenience that returns the
// supplied serials as `adb devices` output. Used by tests that need to
// drive the Boot() port-discovery loop. Pass an EMPTY slice for the
// "no emulators yet" pre-snapshot, then the actual list for the post-
// launch state — the sequenced script will hand them back in order.
func adbDevicesScript(serials ...int) fakeScript {
	var sb strings.Builder
	sb.WriteString("List of devices attached\n")
	for _, s := range serials {
		fmt.Fprintf(&sb, "emulator-%d\tdevice\n", s)
	}
	return fakeScript{Out: []byte(sb.String())}
}

// firstCallMatching returns the index + call where Name == target,
// or panics with a descriptive message. Used by Boot tests after the
// 2026-05-04 architectural fix that introduced pre-snapshot adb-devices
// calls before the emulator-binary launch — calls[0] is no longer the
// launch call.
func firstCallMatching(t *testing.T, calls []fakeCall, target string) fakeCall {
	t.Helper()
	for _, c := range calls {
		if c.Name == target {
			return c
		}
	}
	t.Fatalf("no call to %s found in %d recorded calls", target, len(calls))
	return fakeCall{}
}

// disableADBHygiene replaces adbHygieneHook with a no-op for the duration of
// the test and restores the original on cleanup. Call this in Boot tests that
// use sequencedScripts for "adb devices" so the hygiene pre-call doesn't
// consume the first entry in the sequence.
//
// Tests that ARE specifically exercising ADB hygiene MUST NOT call this —
// they should call ResetADBHygiene (or the hook) directly.
func disableADBHygiene(t *testing.T) {
	t.Helper()
	prev := adbHygieneHook
	adbHygieneHook = func(_ context.Context, _ string, _ CommandExecutor) ADBHygieneResult {
		return ADBHygieneResult{} // no-op in tests
	}
	t.Cleanup(func() { adbHygieneHook = prev })
}

// Start mirrors the production Start contract — records the call and
// returns the script's err (if any). The production Start launches a
// long-running process and returns nil immediately on success; the
// fake matches that for short tests.
func (f *fakeExecutor) Start(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, fakeCall{Name: name, Args: append([]string{}, args...)})
	if s, ok := f.scripts[name]; ok {
		return s.Err
	}
	return nil
}

// TestAndroidEmulator_Boot_PassesExpectedFlagsToEmulatorBinary verifies
// the production code path passes the documented flags to the SDK's
// emulator binary. Falsifiability:
//
//   Mutation: in android.go Boot, replace `-no-snapshot` with
//             `-snapshot-load`. Re-run this test.
//   Observed-Failure: assertion on the args slice fires because the
//             flag string changes.
//   Reverted: yes (see git log).
func TestAndroidEmulator_Boot_PassesExpectedFlagsToEmulatorBinary(t *testing.T) {
	// Disable the pre-boot ADB hygiene reset so it doesn't consume the first
	// entry in the sequenced "adb devices" script before Boot's pre-snapshot
	// call does. Tests that exercise ADB hygiene do so directly.
	disableADBHygiene(t)

	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		sequencedScripts: map[string][]fakeScript{
			// Pre-snapshot: empty. Post-launch: emulator-5554 appears.
			// The Boot() port-discovery loop diffs these to find the
			// new serial.
			"/sdk/platform-tools/adb devices": {
				adbDevicesScript(),
				adbDevicesScript(5554),
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)

	avd := AVD{Name: "Pixel_9a", APILevel: 36, FormFactor: "phone"}
	result, err := emu.Boot(context.Background(), avd, true)

	require.NoError(t, err)
	assert.True(t, result.Started)
	assert.Equal(t, 5554, result.ConsolePort,
		"Boot MUST capture the discovered console port (5554) from adb devices, not hardcoded")
	assert.Equal(t, 5555, result.ADBPort)
	// Find the launch call — it is no longer calls[0] because the
	// pre-snapshot adb-devices call comes first now.
	call := firstCallMatching(t, exec.calls, "/sdk/emulator/emulator")
	assert.Contains(t, call.Args, "-avd")
	assert.Contains(t, call.Args, "Pixel_9a")
	assert.Contains(t, call.Args, "-no-window")
	assert.Contains(t, call.Args, "-no-audio")
	assert.Contains(t, call.Args, "-no-snapshot",
		"clause 6.I clause 6 — cold-boot MUST set -no-snapshot")
}

// TestAndroidEmulator_Boot_WithoutColdBoot_OmitsNoSnapshotFlag pins
// the ColdBoot=false branch. Falsifiability:
//
//   Mutation: in Boot, always append `-no-snapshot` regardless of
//             coldBoot. Re-run.
//   Observed-Failure: this test fails because -no-snapshot would be
//             present even though ColdBoot was false.
func TestAndroidEmulator_Boot_WithoutColdBoot_OmitsNoSnapshotFlag(t *testing.T) {
	disableADBHygiene(t)

	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		sequencedScripts: map[string][]fakeScript{
			"/sdk/platform-tools/adb devices": {
				adbDevicesScript(),
				adbDevicesScript(5554),
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	_, err := emu.Boot(context.Background(), AVD{Name: "X"}, false)
	require.NoError(t, err)
	launch := firstCallMatching(t, exec.calls, "/sdk/emulator/emulator")
	for _, a := range launch.Args {
		assert.NotEqual(t, "-no-snapshot", a)
	}
}

// TestAndroidEmulator_Boot_PropagatesExecutorError pins the error path.
func TestAndroidEmulator_Boot_PropagatesExecutorError(t *testing.T) {
	exec := &fakeExecutor{scripts: map[string]fakeScript{
		"/sdk/emulator/emulator": {Err: errors.New("emulator not found")},
	}}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	result, err := emu.Boot(context.Background(), AVD{Name: "X"}, true)
	require.Error(t, err)
	assert.False(t, result.Started)
	assert.NotNil(t, result.Error)
}

// TestAndroidEmulator_WaitForBoot_ReturnsImmediatelyWhenBootCompleted
// pins the happy path.
func TestAndroidEmulator_WaitForBoot_ReturnsImmediatelyWhenBootCompleted(t *testing.T) {
	exec := &fakeExecutor{scripts: map[string]fakeScript{
		"/sdk/platform-tools/adb -s localhost:5555 shell getprop sys.boot_completed": {
			Out: []byte("1\n"),
		},
	}}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	dur, err := emu.WaitForBoot(context.Background(), 5555, 30*time.Second)
	require.NoError(t, err)
	assert.True(t, dur < 30*time.Second)
}

// TestAndroidEmulator_WaitForBoot_TimesOutWhenNeverBoots pins the
// timeout path. Uses a very short timeout to keep the test fast.
func TestAndroidEmulator_WaitForBoot_TimesOutWhenNeverBoots(t *testing.T) {
	exec := &fakeExecutor{scripts: map[string]fakeScript{
		"/sdk/platform-tools/adb -s localhost:5555 shell getprop sys.boot_completed": {
			Out: []byte("0\n"),
		},
	}}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	_, err := emu.WaitForBoot(context.Background(), 5555, 1*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boot not completed")
}

// TestAndroidEmulator_Install_RejectsMissingAPK pins the precondition
// check. Falsifiability: drop the os.Stat check → this test passes
// the missing-apk path through to adb (where it would fail late and
// confusingly).
func TestAndroidEmulator_Install_RejectsMissingAPK(t *testing.T) {
	exec := &fakeExecutor{scripts: map[string]fakeScript{}}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	err := emu.Install(context.Background(), 5555, "/nonexistent/path.apk")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apk not found")
	// adb MUST NOT be invoked when the apk is missing — early-return
	// preserves the user-visible error context.
	assert.Empty(t, exec.calls,
		"adb invocation MUST NOT happen when apk file is missing")
}

// TestAndroidEmulator_Teardown_InvokesAdbEmuKill pins the teardown
// path. Falsifiability: change "kill" to anything else → this test
// fails because the kill-call args no longer contain "kill".
func TestAndroidEmulator_Teardown_InvokesAdbEmuKill(t *testing.T) {
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		// 2026-05-05 wait-for-exit: simulate the emulator disappearing
		// from `adb devices` immediately after `adb emu kill`. Without
		// this, the polling loop in Teardown would time out.
		sequencedScripts: map[string][]fakeScript{
			"/sdk/platform-tools/adb devices": {
				{Out: []byte("List of devices attached\n")},
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	err := emu.Teardown(context.Background(), 5555)
	require.NoError(t, err)
	// Expect the kill call AND at least one adb-devices poll. The
	// kill must come first so the device is on its way out before we poll.
	require.GreaterOrEqual(t, len(exec.calls), 1)
	first := exec.calls[0]
	assert.Equal(t, "/sdk/platform-tools/adb", first.Name)
	expected := []string{"-s", "localhost:5555", "emu", "kill"}
	assert.Equal(t, expected, first.Args, "first call MUST be the kill")
}

// TestAndroidEmulator_Teardown_WaitsForEmulatorToActuallyExit is the
// regression test for the 2026-05-05 Teardown wait-for-exit fix.
//
// Forensic anchor: `adb -s localhost:5555 emu kill` returns "OK"
// almost immediately, but the qemu-system process can take 10-30s to
// actually exit. The pre-fix Teardown returned immediately, causing
// the matrix runner's next iteration to start while the previous
// emulator was still alive on 5554/5555. The new emulator landed on
// a different port — which the discovery fix (commit 648a4bb) handles
// correctly — but 5 simultaneous emulators caused CPU/RAM contention
// that produced flakes in late iterations.
//
// This test simulates: kill returns OK → first poll shows device
// still alive → second poll shows device gone. Teardown MUST return
// only AFTER the second poll, NOT after the first.
//
// Falsifiability:
//   Mutation: revert Teardown to the pre-fix form (return immediately
//             after kill, no polling).
//   Observed-Failure: this test would fail because the call count
//             would be 1 (kill only), not 2+ (kill + at least one poll).
//   Reverted: yes — post-revert this test passes.
func TestAndroidEmulator_Teardown_WaitsForEmulatorToActuallyExit(t *testing.T) {
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		sequencedScripts: map[string][]fakeScript{
			"/sdk/platform-tools/adb devices": {
				// First poll: emulator still alive (qemu hasn't exited yet)
				{Out: []byte("List of devices attached\nemulator-5554\tdevice\nlocalhost:5555\tdevice\n")},
				// Second poll: emulator gone
				{Out: []byte("List of devices attached\n")},
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)
	err := emu.Teardown(context.Background(), 5555)
	require.NoError(t, err)
	// We expect 1 kill call + at least 2 poll calls (one that saw
	// "still alive", one that saw "gone"). Total >= 3.
	devicesCalls := 0
	for _, c := range exec.calls {
		if c.Name == "/sdk/platform-tools/adb" && len(c.Args) == 1 && c.Args[0] == "devices" {
			devicesCalls++
		}
	}
	assert.GreaterOrEqual(t, devicesCalls, 2,
		"Teardown MUST poll adb devices at least twice (once seeing 'alive', once seeing 'gone'); got %d", devicesCalls)
}

// TestAndroidEmulator_RunInstrumentation_RejectsEmptyTestClass pins
// the precondition. Anti-bluff: a runner that silently runs all tests
// when testClass is empty would mask a configuration bug.
func TestAndroidEmulator_RunInstrumentation_RejectsEmptyTestClass(t *testing.T) {
	emu := NewAndroidEmulator("/sdk")
	_, _, err := emu.RunInstrumentation(
		context.Background(), 5555, "", 30*time.Second,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "testClass MUST be non-empty")
}

func init() {
	_ = fmt.Sprintf // keep "fmt" usable for future test extensions
}

// TestAndroidEmulator_Boot_DiscoversNewSerial_WhenPriorEmulatorPersists
// is the regression test for the 2026-05-04 ultrathink-discovered
// architectural bluff in the multi-AVD matrix runner.
//
// Forensic anchor (verbatim from the diagnostic output captured this
// session):
//
//   [matrix-diag] target=localhost:5555 adb-devices=
//     "List of devices attached\nemulator-5554\tdevice\nlocalhost:5555\tdevice"
//     ro.product.model="Android SDK built for x86_64"
//   [matrix-diag] target=localhost:5555 adb-devices=
//     "...emulator-5554\tdevice\nemulator-5556\toffline..."
//     ro.product.model="Android SDK built for x86_64"
//   ... (5 iterations, all targeting localhost:5555, all reporting
//        the same model — every iteration tested the FIRST AVD's
//        emulator while the matrix attestation file claimed each row
//        was a different AVD)
//
// Pre-fix Boot() hardcoded ADBPort=5555 unconditionally. When the
// previous AVD's Teardown left the first emulator alive on 5554/5555
// (which Teardown did fail to kill in this session — 5 orphans
// observed across iterations), the next emulator launched on
// 5556/5557 but the matrix runner kept polling 5555 — getting the
// OLD AVD's "boot complete" signal instantly and running the test
// against the OLD AVD.
//
// This test pins the post-fix behaviour: when `adb devices` already
// shows an emulator on 5554 (a leaked previous emulator), Boot() MUST
// detect that the NEW emulator landed on a different port and return
// THAT port in BootResult.ADBPort.
//
// Falsifiability rehearsal (clause 6.I clause 5 + Sixth Law clause 2):
//
//   Mutation: revert this commit's Boot() to the pre-fix form
//             (hardcoded ConsolePort=5554, ADBPort=5555).
//   Observed-Failure: this test fails with
//             "expected 5557 but got 5555" — the assertion below.
//   Reverted: yes, post-revert this test passes again.
func TestAndroidEmulator_Boot_DiscoversNewSerial_WhenPriorEmulatorPersists(t *testing.T) {
	disableADBHygiene(t)

	// Simulate the bug scenario: a prior matrix iteration's emulator
	// is STILL running on 5554 (Teardown failed silently). The new
	// AVD's emulator lands on 5556 because 5554/5555 is busy.
	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		sequencedScripts: map[string][]fakeScript{
			"/sdk/platform-tools/adb devices": {
				// Pre-snapshot: prior emulator on 5554 is still alive.
				adbDevicesScript(5554),
				// Post-launch: new emulator on 5556 has appeared.
				adbDevicesScript(5554, 5556),
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)

	avd := AVD{Name: "CZ_API30_Phone", APILevel: 30, FormFactor: "phone"}
	result, err := emu.Boot(context.Background(), avd, true)

	require.NoError(t, err)
	require.True(t, result.Started)
	// PRIMARY: the discovered console port MUST be 5556 (the new one),
	// NOT 5554 (the leaked previous one). Falsifiable: revert Boot()'s
	// dynamic discovery → result.ConsolePort would be 5554 (or 5555
	// for the pre-fix hardcode).
	assert.Equal(t, 5556, result.ConsolePort,
		"clause 6.I architectural fix — Boot MUST detect the NEW emulator's "+
			"port via adb-devices diff, NOT return the leaked previous emulator's port")
	assert.Equal(t, 5557, result.ADBPort,
		"ADB port = console port + 1, dynamically computed from discovery")
	// SECONDARY: the AVD metadata is preserved through the discovery
	// step (catches a regression where Boot returns an empty AVD on
	// the discovery path).
	assert.Equal(t, "CZ_API30_Phone", result.AVD.Name)
	assert.Equal(t, 30, result.AVD.APILevel)
}

// TestAndroidEmulator_Boot_FailsWhenNoNewSerialAppears pins the
// failure path of the discovery: if `adb devices` never registers a
// new emulator (kvm denied, zygote crash, etc.), Boot MUST return an
// error rather than silently mis-targeting later calls. Without this
// check, the matrix runner would proceed to Install + RunInstrumentation
// against whatever happened to be on 5555 (the pre-fix bug).
//
// Note: this test passes a custom-shortened context to keep test time
// reasonable. The production timeout is 60s; we simulate it as
// "discovery exhausted" via a deadline-bounded context.
// ---------------------------------------------------------------------
// Teardown fast-path tests — Group B
//
// After the existing 30s `adb emu kill` grace expires, Teardown invokes
// emulator.KillByPort(consolePort) to attempt a port-strict force-kill.
// On match, Teardown returns nil (the emulator was stuck but is now
// gone). On mismatch (Matched=0), Teardown returns the original
// "emulator did not exit" error — concurrent emulators on other ports
// are untouched.
//
// SAFETY: these tests verify the skip-on-mismatch invariant. The
// production code MUST NOT broaden the kill criterion to any process
// that "looks like" a stuck emulator.
// ---------------------------------------------------------------------

// fakeAdbExecutorAlwaysAlive is a CommandExecutor whose Execute() returns
// `adb devices` output that always includes the target localhost:<port>
// in "device" state — simulating a stuck emulator that ignores `adb emu
// kill`.
type fakeAdbExecutorAlwaysAlive struct {
	port int
}

func (f fakeAdbExecutorAlwaysAlive) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	// adb -s localhost:<port> emu kill — pretend it succeeds
	if len(args) >= 4 && args[2] == "emu" && args[3] == "kill" {
		return []byte("OK: killing emulator, bye bye\n"), nil
	}
	// adb devices — always report localhost:<port> as alive
	if len(args) == 1 && args[0] == "devices" {
		return []byte(fmt.Sprintf("List of devices attached\nlocalhost:%d\tdevice\n", f.port)), nil
	}
	return nil, nil
}

func (f fakeAdbExecutorAlwaysAlive) Start(_ context.Context, _ string, _ ...string) error {
	return nil
}

func TestTeardown_FastPath_SkipsOnMismatch(t *testing.T) {
	// Save and replace package-level KillByPort hook so the test is
	// hermetic. The production implementation walks /proc.
	prev := killByPortHook
	killByPortHook = func(_ context.Context, _ int) (KillReport, error) {
		// Mismatch: no /proc entry has -port <port> adjacent.
		return KillReport{Matched: 0}, nil
	}
	defer func() { killByPortHook = prev }()

	// Use a short test-only Teardown timeout so the 30s grace is
	// compressed for the test.
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 200 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	a := NewAndroidEmulatorWithExecutor("/opt/android-sdk", fakeAdbExecutorAlwaysAlive{port: 5554})
	err := a.Teardown(context.Background(), 5554)
	if err == nil {
		t.Fatalf("expected Teardown to return an error when KillByPort.Matched==0 and emulator persists, got nil")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("expected error to mention 'did not exit', got: %v", err)
	}
}

// fakeAdbExecutorStuckThenGone reports the target as alive on the first
// `adb devices` call and gone on subsequent calls — simulating a stuck
// emulator that the KillByPort fast-path successfully clears.
type fakeAdbExecutorStuckThenGone struct {
	port  int
	calls int
}

func (f *fakeAdbExecutorStuckThenGone) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) >= 4 && args[2] == "emu" && args[3] == "kill" {
		return []byte("OK: killing emulator, bye bye\n"), nil
	}
	if len(args) == 1 && args[0] == "devices" {
		f.calls++
		if f.calls <= 2 {
			return []byte(fmt.Sprintf("List of devices attached\nlocalhost:%d\tdevice\n", f.port)), nil
		}
		return []byte("List of devices attached\n"), nil
	}
	return nil, nil
}

func (f *fakeAdbExecutorStuckThenGone) Start(_ context.Context, _ string, _ ...string) error {
	return nil
}

func TestTeardown_FastPath_SucceedsAfterKillByPort(t *testing.T) {
	prev := killByPortHook
	killByPortHook = func(_ context.Context, _ int) (KillReport, error) {
		return KillReport{Matched: 1, Sigtermed: []int{12345}}, nil
	}
	defer func() { killByPortHook = prev }()
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 200 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	a := NewAndroidEmulatorWithExecutor("/opt/android-sdk", &fakeAdbExecutorStuckThenGone{port: 5554})
	err := a.Teardown(context.Background(), 5554)
	if err != nil {
		t.Fatalf("expected Teardown to succeed after KillByPort cleared the stuck emulator, got: %v", err)
	}
}

func TestAndroidEmulator_Boot_FailsWhenNoNewSerialAppears(t *testing.T) {
	disableADBHygiene(t)

	exec := &fakeExecutor{
		scripts: map[string]fakeScript{},
		sequencedScripts: map[string][]fakeScript{
			"/sdk/platform-tools/adb devices": {
				// Pre and post: same set. No new emulator ever appears.
				adbDevicesScript(5554),
				adbDevicesScript(5554),
			},
		},
	}
	emu := NewAndroidEmulatorWithExecutor("/sdk", exec)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := emu.Boot(ctx, AVD{Name: "X"}, true)

	require.Error(t, err)
	// The wrapped error chain contains either "port discovery cancelled"
	// (ctx deadline beat the 60s timeout) or "no new emulator serial
	// appeared" (60s timeout fired first). Both are correct
	// constitutional behaviour — the runner refused to silently
	// mis-target subsequent calls.
	msg := err.Error()
	assert.True(t,
		strings.Contains(msg, "port discovery") || strings.Contains(msg, "no new emulator serial"),
		"error message MUST mention discovery failure, got: %s", msg)
	assert.True(t, result.Started, "Started=true even on discovery fail — the launch itself succeeded")
	assert.NotNil(t, result.Error)
}

// countingStore is a minimal cache.Store fake that records every Get
// invocation. Used by the Phase B routing-decision test to assert that
// the missing-system-image path consults the cache.
//
// Anti-bluff posture (clauses 6.J/6.L): the fake's Get does NOT touch
// the network or the filesystem — the test is verifying the ROUTING
// decision (does the production code reach the cache?), not the full
// fetch-and-extract path. The fake returns a sentinel error so the
// caller's error-propagation code is exercised too.
type countingStore struct {
	getCalls []countingGetCall
}

type countingGetCall struct {
	ImageID string
}

func (c *countingStore) Get(_ context.Context, _ *cache.Manifest, imageID string) (string, error) {
	c.getCalls = append(c.getCalls, countingGetCall{ImageID: imageID})
	return "", errors.New("counting-store: Get not actually performed (test fake)")
}

func (c *countingStore) Verify(_ context.Context, _ *cache.Manifest, _ string) error {
	return nil
}

func (c *countingStore) Refresh(_ context.Context, _ *cache.Manifest, _ string) error {
	return nil
}

// TestEnsureSystemImageViaCache_RoutesMissingImageThroughCache is the
// Phase B routing-decision test for the helper itself. It verifies the
// helper's branches in isolation:
//
//   - (a) Empty ImageManifestPath → no-op (pre-Phase-B byte-equivalent),
//     cache Store is NEVER consulted.
//   - (b) Set ImageManifestPath + missing image dir → cache consulted via
//     Store.Get with imageID == "android-<api>-<formFactor>". The
//     counting fake records the call; the test asserts it happened
//     exactly once and the imageID composition is correct.
//   - (b') Set ImageManifestPath + present image dir → no-op, cache MUST
//     NOT be consulted.
//
// NOTE: this test exercises the helper directly, NOT through the matrix
// runner. The end-to-end test that drives the production code path
// (RunMatrix → runOne → helper) is
// TestRunMatrix_RoutesMissingSystemImageThroughCache_WhenImageManifestPathIsSet
// — that one is the load-bearing C1 fix proof.
//
// Falsifiability rehearsal (Sixth Law clause 2):
//
//	Mutation: in android.go ensureSystemImageViaCache, drop the
//	          `store := cacheStoreFactory(defaultCacheRoot())` +
//	          `store.Get(...)` call (replace with `var store cache.Store`).
//	Run:      go test ./pkg/emulator/... -run TestEnsureSystemImageViaCache_RoutesMissingImageThroughCache
//	Observed-Failure: the (b) branch's `require.Len(t, store.getCalls, 1)`
//	          assertion fails because Get was never invoked.
//	Reverted: yes — post-revert this test passes again.
func TestEnsureSystemImageViaCache_RoutesMissingImageThroughCache(t *testing.T) {
	// --- (a) Empty ImageManifestPath: cache MUST NOT be consulted. ---
	store := &countingStore{}
	prevFactory := cacheStoreFactory
	cacheStoreFactory = func(_ string) cache.Store { return store }
	defer func() { cacheStoreFactory = prevFactory }()

	emu := NewAndroidEmulator("/sdk")
	avd := AVD{Name: "Pixel_API28", APILevel: 28, FormFactor: "phone"}

	err := emu.ensureSystemImageViaCache(context.Background(), avd, "")
	require.NoError(t, err,
		"empty ImageManifestPath MUST be a no-op (pre-Phase-B byte-equivalent)")
	assert.Empty(t, store.getCalls,
		"cache.Store.Get MUST NOT be invoked when ImageManifestPath is empty")

	// --- (b) Set ImageManifestPath + missing image dir → cache consulted. ---
	// Use a temp dir as fake ANDROID_SDK_ROOT so system-images/android-28
	// is provably absent (we don't create it).
	tmpSdk := t.TempDir()
	emu2 := NewAndroidEmulator(tmpSdk)

	// Substitute the manifest loader with one that returns a fixed manifest
	// (we don't need to write a real JSON file for the routing-decision test).
	// URL uses the canonical .../sys-img/<tag>/<abi>-<api>_r<rev>.zip pattern
	// so parseSystemImageURL succeeds and the test reaches the cache.Get
	// call (which is the routing-decision the test pins).
	prevLoader := loadManifestHook
	loadManifestHook = func(path string) (*cache.Manifest, error) {
		return &cache.Manifest{
			Version: 1,
			Images: []cache.ImageEntry{{
				ID:     "android-28-phone",
				URL:    "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-28_r12.zip",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Size:   1,
				Format: "android-system-image",
			}},
		}, nil
	}
	defer func() { loadManifestHook = prevLoader }()

	// Pre-condition: the system-images dir for API 28 is absent.
	_, statErr := os.Stat(filepath.Join(tmpSdk, "system-images", "android-28"))
	require.True(t, os.IsNotExist(statErr),
		"test pre-condition: system-images/android-28 MUST be absent under tmp SDK root")

	manifestPath := filepath.Join(t.TempDir(), "vm-images.json")
	// Path content is irrelevant — loadManifestHook returns a canned
	// manifest. The path just has to be non-empty.
	err = emu2.ensureSystemImageViaCache(context.Background(), avd, manifestPath)
	// The helper returns the explicit "extraction: not implemented in v0.1"
	// error after a successful cache.Get attempt. Our counting fake's Get
	// returns its own sentinel error, which surfaces as the wrapped
	// "fetch ...: counting-store: Get not actually performed" — both are
	// expected error states for this routing-decision test (extraction is
	// stubbed per the v0.1 honesty mandate).
	require.Error(t, err,
		"helper MUST return an error in v0.1 (extraction not implemented OR fetch fake-failed)")
	require.Len(t, store.getCalls, 1,
		"cache.Store.Get MUST be invoked exactly once when manifest is set AND image is missing")
	assert.Equal(t, "android-28-phone", store.getCalls[0].ImageID,
		"imageID composition MUST be 'android-<APILevel>-<FormFactor>' "+
			"(stable contract; downstream manifests pin entries by this ID)")

	// --- (b'): present extracted target → cache MUST NOT be consulted. ---
	// Pre-populate the per-AVD target dir with the canonical pair
	// (system.img + build.prop) — the SOLE post-Phase-3 idempotence
	// check. An empty parent dir is no longer enough; the helper now
	// requires the actual extracted artifacts to be present.
	target := filepath.Join(tmpSdk, "system-images", "android-28", "google_apis", "x86_64")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "system.img"), []byte("FAKE"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(target, "build.prop"), []byte("FAKE"), 0o644))
	store.getCalls = nil
	err = emu2.ensureSystemImageViaCache(context.Background(), avd, manifestPath)
	require.NoError(t, err,
		"helper MUST be a no-op when target dir already has the canonical pair")
	assert.Empty(t, store.getCalls,
		"cache.Store.Get MUST NOT be invoked when extraction is already on disk")
}

// TestBootResult_AttestationSchemaUnchanged pins BootResult's field-set
// against pre-Phase-B drift. The Phase B refactor is API-preserving by
// spec; any new field is a constitutional violation that breaks the
// attestation schema downstream consumers depend on (matrix.go's
// writeAttestation, scripts/tag.sh's gating logic).
//
// Falsifiability rehearsal (Sixth Law clause 2):
//
//	Mutation: add a new field `CacheUsed bool` to BootResult struct in
//	          types.go.
//	Run:      go test ./pkg/emulator/... -run TestBootResult_AttestationSchemaUnchanged
//	Observed-Failure: reflection sees the unexpected new field; both the
//	          field-count and field-set assertions fire.
//	Reverted: yes — post-revert this test passes again.
func TestBootResult_AttestationSchemaUnchanged(t *testing.T) {
	// BootDiag was added 2026-06-03 for boot-timeout forensic capture (per
	// clause 6.I Group-B diag intent). It is a pointer field that is only
	// set on error paths — nil on every successful boot — so it does NOT
	// appear in attestation rows for passing runs. Its presence here is
	// intentional: adding a nil-safe diagnostic field is not a breaking
	// schema change for existing consumers.
	expectedFields := []string{
		"AVD", "ADBPort", "BootCompleted", "BootDiag", "BootDuration",
		"ConsolePort", "Error", "Started",
	}
	bootResultType := reflect.TypeOf(BootResult{})
	require.Equal(t, len(expectedFields), bootResultType.NumField(),
		"BootResult field count drifted; expected %d, got %d. "+
			"Adding fields breaks downstream attestation schema consumers "+
			"(matrix.go writeAttestation + scripts/tag.sh).",
		len(expectedFields), bootResultType.NumField())
	got := make([]string, 0, bootResultType.NumField())
	for i := 0; i < bootResultType.NumField(); i++ {
		got = append(got, bootResultType.Field(i).Name)
	}
	sort.Strings(got)
	wantSorted := append([]string{}, expectedFields...)
	sort.Strings(wantSorted)
	assert.Equal(t, wantSorted, got,
		"BootResult field-set drifted. The Phase B refactor "+
			"is API-preserving by spec; any new field is a constitutional "+
			"violation that breaks the attestation schema downstream consumers depend on.")
}

// TestErrExtractionNotImplemented_BackwardCompatExport verifies the
// v0.1 sentinel remains exported after the Phase 3 extraction work. The
// production path no longer emits this sentinel (extraction is now
// implemented), but downstream v0.1 callers may still do
// `errors.Is(err, ErrExtractionNotImplemented)`. Removing the symbol
// would be a compile-break we must not ship.
//
// Anti-bluff posture (clauses 6.J/6.L): this test guards the contract
// surface, NOT a production behaviour — so it is paired with
// TestEnsureSystemImageViaCache_ExtractsZipSuccessfully (which proves
// the production path now actually extracts files) and
// TestEnsureSystemImageViaCache_RejectsMalformedZip (which proves the
// new ErrSystemImageMalformed sentinel fires honestly). Together those
// three tests pin the new contract: extraction works, malformed input
// is rejected, and the legacy sentinel survives as a compile symbol.
func TestErrExtractionNotImplemented_BackwardCompatExport(t *testing.T) {
	require.NotNil(t, ErrExtractionNotImplemented,
		"ErrExtractionNotImplemented MUST remain exported for v0.1 backward compat; "+
			"downstream callers using errors.Is would compile-break otherwise")
	require.NotEmpty(t, ErrExtractionNotImplemented.Error(),
		"sentinel error message MUST remain non-empty so downstream debug logs stay legible")
}

// pathStore is a cache.Store fake that returns a fixed local path
// (typically pointing to a synthetic ZIP fixture written under
// t.TempDir()). Used by the Phase 3 extraction tests to drive the
// helper through the real archive/zip code path without making any
// network calls.
//
// Anti-bluff posture (clauses 6.J/6.L): the fake is behaviorally
// equivalent to FilesystemStore.Get for the assertions we care about
// — it returns a string path, callers feed that path to
// archive/zip.OpenReader, and the production extraction code runs
// unchanged. A test that uses this fake is exercising the SAME
// extraction code path the production matrix runner exercises after a
// real cache fetch.
type pathStore struct {
	path string
	err  error
}

func (p *pathStore) Get(_ context.Context, _ *cache.Manifest, _ string) (string, error) {
	return p.path, p.err
}

func (p *pathStore) Verify(_ context.Context, _ *cache.Manifest, _ string) error {
	return nil
}

func (p *pathStore) Refresh(_ context.Context, _ *cache.Manifest, _ string) error {
	return nil
}

// writeSystemImageZip constructs a real archive/zip blob at t.TempDir()
// containing the supplied entries (relative to the inner directory) and
// returns the absolute path. Mirrors the upstream Android system-image
// ZIP layout: `<inner>/system.img`, `<inner>/build.prop`, ... where
// <inner> is typically the abi name (e.g. "x86_64").
//
// Anti-bluff posture: the ZIP is a real archive/zip file, not a
// pre-made byte slice — the same archive/zip reader the production
// extractor uses also reads it. Mutating the production extractor to
// not actually write any files to disk produces a real test failure
// because the post-extraction file-existence asserts cannot be passed
// without bytes hitting the filesystem.
func writeSystemImageZip(t *testing.T, innerDir string, entries map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "sysimage.zip")
	f, err := os.Create(zipPath)
	require.NoError(t, err)
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	for name, body := range entries {
		var entryName string
		if innerDir == "" {
			entryName = name
		} else {
			entryName = innerDir + "/" + name
		}
		hdr := &zip.FileHeader{
			Name:   entryName,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		zw, err := w.CreateHeader(hdr)
		require.NoError(t, err)
		_, err = zw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return zipPath
}

// TestEnsureSystemImageViaCache_ExtractsZipSuccessfully is the Phase 3
// end-to-end extraction test. A synthetic ZIP carrying the canonical
// upstream layout (system.img + build.prop + a few siblings under the
// abi inner-dir) is fed through a pathStore fake; the helper unzips it
// into the expected ANDROID_SDK_ROOT/system-images/android-<api>/<tag>/<abi>/
// target dir, the inner-dir prefix is stripped, and the result on disk
// is asserted byte-for-byte (NOT just "function returned nil").
//
// Falsifiability rehearsal (Sixth Law clause 2 + clause 6.J/6.L
// "primary assertion on user-visible state"):
//
//	Mutation: in android.go extractSystemImageZip, replace the
//	          `for _, f := range r.File { ... }` body with a no-op
//	          (continue) so no files are written.
//	Run:      go test ./pkg/emulator/... -run TestEnsureSystemImageViaCache_ExtractsZipSuccessfully
//	Observed-Failure: the post-extraction validation in the production
//	          code returns ErrSystemImageMalformed (because system.img
//	          doesn't appear at the target path). The test fails with
//	          the require.NoError on err, AND the file-existence
//	          asserts would fail too if we got past the err check.
//	Reverted: yes — post-revert this test passes again.
func TestEnsureSystemImageViaCache_ExtractsZipSuccessfully(t *testing.T) {
	systemImgBytes := []byte("FAKE-SYSTEM-IMG-BYTES-FOR-TEST")
	buildPropBytes := []byte("ro.product.device=fake_device\nro.build.version.sdk=28\n")
	ramdiskBytes := []byte("FAKE-RAMDISK")
	sourcePropBytes := []byte("Pkg.Desc=Fake System Image\n")

	// Real archive/zip with canonical upstream layout: every file
	// under "x86_64/" inner dir.
	zipPath := writeSystemImageZip(t, "x86_64", map[string][]byte{
		"system.img":        systemImgBytes,
		"build.prop":        buildPropBytes,
		"ramdisk.img":       ramdiskBytes,
		"source.properties": sourcePropBytes,
	})

	tmpSdk := t.TempDir()
	emu := NewAndroidEmulator(tmpSdk)
	avd := AVD{Name: "Pixel_API28", APILevel: 28, FormFactor: "phone"}

	prevFactory := cacheStoreFactory
	cacheStoreFactory = func(_ string) cache.Store {
		return &pathStore{path: zipPath}
	}
	defer func() { cacheStoreFactory = prevFactory }()

	prevLoader := loadManifestHook
	loadManifestHook = func(_ string) (*cache.Manifest, error) {
		return &cache.Manifest{
			Version: 1,
			Images: []cache.ImageEntry{{
				ID:     "android-28-phone",
				URL:    "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-28_r12.zip",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Size:   1,
				Format: "android-system-image",
			}},
		}, nil
	}
	defer func() { loadManifestHook = prevLoader }()

	manifestPath := filepath.Join(t.TempDir(), "vm-images.json")
	err := emu.ensureSystemImageViaCache(context.Background(), avd, manifestPath)
	require.NoError(t, err,
		"helper MUST extract a well-formed ZIP and return nil; got: %v", err)

	// PRIMARY: assert on user-visible filesystem state. The matrix
	// runner's NEXT step (emulator.Boot) reads exactly these paths;
	// the test asserts they exist with the right bytes.
	target := filepath.Join(tmpSdk, "system-images", "android-28", "google_apis", "x86_64")
	for relPath, expected := range map[string][]byte{
		"system.img":        systemImgBytes,
		"build.prop":        buildPropBytes,
		"ramdisk.img":       ramdiskBytes,
		"source.properties": sourcePropBytes,
	} {
		got, rerr := os.ReadFile(filepath.Join(target, relPath))
		require.NoError(t, rerr,
			"extracted file %s MUST exist under %s (anti-bluff: silent-success-on-empty-zip is a §6.J violation)",
			relPath, target)
		assert.Equal(t, expected, got,
			"extracted file %s bytes MUST match the ZIP entry's bytes", relPath)
	}
}

// TestEnsureSystemImageViaCache_RejectsMalformedZip is the Phase 3
// negative test. A synthetic ZIP missing system.img must trigger the
// ErrSystemImageMalformed error AND leave NO partial files under the
// target dir (cleanup-on-failure is the difference between an
// idempotent retry and a poisoned-state retry — see §6.J).
//
// Falsifiability rehearsal (Sixth Law clause 2):
//
//	Mutation: in android.go extractSystemImageZip, drop the
//	          post-extraction validation block (the
//	          `if !hasExtractedSystemImage(targetDir) { ... }` branch).
//	Run:      go test ./pkg/emulator/... -run TestEnsureSystemImageViaCache_RejectsMalformedZip
//	Observed-Failure: the helper now silently returns nil for the
//	          malformed ZIP. require.Error fires; errors.Is check
//	          fires too; the cleanup assertion would also fire because
//	          the build.prop file would now be left on disk.
//	Reverted: yes — post-revert this test passes again.
func TestEnsureSystemImageViaCache_RejectsMalformedZip(t *testing.T) {
	// ZIP missing system.img. build.prop is present but on its own is
	// not a valid system-image — emulator boot would later fail with
	// "system.img not found" and the operator would chase ghosts. The
	// extraction validation surfaces the failure at the right layer.
	zipPath := writeSystemImageZip(t, "x86_64", map[string][]byte{
		"build.prop":        []byte("ro.product.device=fake_device\n"),
		"source.properties": []byte("Pkg.Desc=Fake\n"),
	})

	tmpSdk := t.TempDir()
	emu := NewAndroidEmulator(tmpSdk)
	avd := AVD{Name: "Pixel_API30", APILevel: 30, FormFactor: "phone"}

	prevFactory := cacheStoreFactory
	cacheStoreFactory = func(_ string) cache.Store {
		return &pathStore{path: zipPath}
	}
	defer func() { cacheStoreFactory = prevFactory }()

	prevLoader := loadManifestHook
	loadManifestHook = func(_ string) (*cache.Manifest, error) {
		return &cache.Manifest{
			Version: 1,
			Images: []cache.ImageEntry{{
				ID:     "android-30-phone",
				URL:    "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-30_r12.zip",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Size:   1,
				Format: "android-system-image",
			}},
		}, nil
	}
	defer func() { loadManifestHook = prevLoader }()

	manifestPath := filepath.Join(t.TempDir(), "vm-images.json")
	err := emu.ensureSystemImageViaCache(context.Background(), avd, manifestPath)

	// PRIMARY: typed-error assertion. errors.Is must walk through the
	// fmt.Errorf wraps and find ErrSystemImageMalformed at the bottom
	// of the chain.
	require.Error(t, err,
		"helper MUST surface a malformed-ZIP error; silent success would be a §6.J bluff")
	assert.True(t, errors.Is(err, ErrSystemImageMalformed),
		"error chain MUST wrap ErrSystemImageMalformed; got: %v", err)

	// PRIMARY (cleanup-on-failure): the target dir MUST contain NO
	// partial extracted files. A leftover build.prop would poison the
	// next retry — the helper's cleanup-on-failure is what makes
	// extraction idempotent.
	target := filepath.Join(tmpSdk, "system-images", "android-30", "google_apis", "x86_64")
	if _, err := os.Stat(filepath.Join(target, "build.prop")); err == nil {
		t.Fatalf("build.prop MUST NOT survive a failed extraction; cleanup-on-failure is mandatory (§6.J)")
	}
	if _, err := os.Stat(filepath.Join(target, "source.properties")); err == nil {
		t.Fatalf("source.properties MUST NOT survive a failed extraction; cleanup-on-failure is mandatory (§6.J)")
	}
}

// TestEnsureSystemImageViaCache_IdempotentOnExistingExtraction pins
// Branch 4 (fine-grained idempotence): when the per-AVD target dir
// already contains the canonical pair (system.img + build.prop), the
// helper MUST NOT consult the cache at all (no SHA-256 work, no zip
// re-extraction). Pre-populates the target, then asserts the helper
// returns nil AND no Get() call was recorded by a counting fake.
//
// Falsifiability rehearsal (Sixth Law clause 2):
//
//	Mutation: in android.go ensureSystemImageViaCache, drop the
//	          `if hasExtractedSystemImage(targetDir) { return nil }`
//	          early-return.
//	Run:      go test ./pkg/emulator/... -run TestEnsureSystemImageViaCache_IdempotentOnExistingExtraction
//	Observed-Failure: store.getCalls becomes 1 (the cache was
//	          consulted unnecessarily); the assertion on len(getCalls)
//	          fires. Bonus: redundant SHA-256 work would slow every
//	          matrix iteration's start-up.
//	Reverted: yes — post-revert this test passes again.
func TestEnsureSystemImageViaCache_IdempotentOnExistingExtraction(t *testing.T) {
	tmpSdk := t.TempDir()
	avd := AVD{Name: "Pixel_API34", APILevel: 34, FormFactor: "phone"}
	target := filepath.Join(tmpSdk, "system-images", "android-34", "google_apis", "x86_64")
	require.NoError(t, os.MkdirAll(target, 0o755))
	preExistingSystemImg := []byte("PRE-EXISTING-SYSTEM-IMG-BYTES")
	preExistingBuildProp := []byte("ro.product.device=pre_existing\n")
	require.NoError(t, os.WriteFile(filepath.Join(target, "system.img"), preExistingSystemImg, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(target, "build.prop"), preExistingBuildProp, 0o644))

	// Counting store — Get() MUST NOT be invoked when the extraction
	// is already on disk.
	store := &countingStore{}
	prevFactory := cacheStoreFactory
	cacheStoreFactory = func(_ string) cache.Store { return store }
	defer func() { cacheStoreFactory = prevFactory }()

	prevLoader := loadManifestHook
	loadManifestHook = func(_ string) (*cache.Manifest, error) {
		return &cache.Manifest{
			Version: 1,
			Images: []cache.ImageEntry{{
				ID:     "android-34-phone",
				URL:    "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-34_r12.zip",
				SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
				Size:   1,
				Format: "android-system-image",
			}},
		}, nil
	}
	defer func() { loadManifestHook = prevLoader }()

	// Post-Phase-3 there is NO coarse API-level dir short-circuit;
	// the SOLE idempotence check is the fine-grained one that
	// requires system.img + build.prop in the per-AVD target. Dropping
	// that check is the canonical falsifiability rehearsal for this
	// test (see KDoc above).
	emu := NewAndroidEmulator(tmpSdk)
	manifestPath := filepath.Join(t.TempDir(), "vm-images.json")
	err := emu.ensureSystemImageViaCache(context.Background(), avd, manifestPath)
	require.NoError(t, err,
		"helper MUST be a no-op when target dir already has system.img + build.prop")
	require.Empty(t, store.getCalls,
		"cache.Store.Get MUST NOT be invoked when extraction is already on disk; "+
			"redundant fetch would slow every matrix iteration's start-up")

	// Belt-and-suspenders: the pre-existing files MUST NOT have been
	// overwritten by a redundant extraction.
	got, rerr := os.ReadFile(filepath.Join(target, "system.img"))
	require.NoError(t, rerr)
	assert.Equal(t, preExistingSystemImg, got,
		"pre-existing system.img MUST NOT be overwritten by redundant extraction")
	got, rerr = os.ReadFile(filepath.Join(target, "build.prop"))
	require.NoError(t, rerr)
	assert.Equal(t, preExistingBuildProp, got,
		"pre-existing build.prop MUST NOT be overwritten by redundant extraction")
}

// TestParseSystemImageURL_HappyPathsAndRejections pins the URL parser's
// contract: canonical upstream URLs yield (tag, abi); anything else is
// an error rather than a silent mis-parse. This is a unit-level pin of
// the helper used by ensureSystemImageViaCache; the higher-level
// integration tests above exercise it transitively.
func TestParseSystemImageURL_HappyPathsAndRejections(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantTag string
		wantABI string
		wantErr bool
	}{
		{
			name:    "canonical google_apis x86_64 28",
			url:     "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-28_r12.zip",
			wantTag: "google_apis",
			wantABI: "x86_64",
		},
		{
			name:    "google_apis_playstore arm64-v8a",
			url:     "https://dl.google.com/android/repository/sys-img/google_apis_playstore/arm64-v8a-34_r3.zip",
			wantTag: "google_apis_playstore",
			wantABI: "arm64-v8a",
		},
		{
			name:    "android-tv x86",
			url:     "https://dl.google.com/android/repository/sys-img/android-tv/x86-30_r4.zip",
			wantTag: "android-tv",
			wantABI: "x86",
		},
		{
			name:    "missing sys-img segment",
			url:     "https://example.invalid/sysimage.zip",
			wantErr: true,
		},
		{
			name:    "non-zip filename",
			url:     "https://dl.google.com/android/repository/sys-img/google_apis/x86_64-28_r12.tar",
			wantErr: true,
		},
		{
			name:    "filename without revision suffix",
			url:     "https://dl.google.com/android/repository/sys-img/google_apis/x86_64.zip",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, abi, err := parseSystemImageURL(tc.url)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTag, tag)
			assert.Equal(t, tc.wantABI, abi)
		})
	}
}
