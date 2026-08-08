package emulator

// Wave-18c FB-2 — network-shaping + failure-screenshot MUST target the adb
// transport port, not the emulator console port.
//
// §11.4.115 RED-on-broken: matrix.go builds the adb serial for network shaping
// (runOne network block) and for the failure screenshot from
// `boot.ConsolePort`, but an emulator on console port N exposes adb on port
// N+1 — its adb transport is `localhost:<ADBPort>`, never `localhost:<ConsolePort>`
// (the console port speaks the telnet console protocol, not adb). Every other
// adb-driving path (captureDiagnostic, RunInstrumentation) correctly uses
// `boot.ADBPort`. Feeding the console port makes `adb -s localhost:<console>`
// resolve to "device not found", so network shaping and the forensic
// failure-screenshot silently no-op in production (both are best-effort and do
// not flip the row). The existing TestRunOne_NetworkConditionsApplied asserts
// the `emu network` call HAPPENED but never checks its `-s` target port — the
// coverage gap that let this ship. These tests pin the target port.
//
// adbStubEmulator.Boot returns ConsolePort=5554, ADBPort=5555, so the correct
// target is `localhost:5555`; the pre-fix code emits `localhost:5554`.

import (
	"strings"
	"testing"
)

func TestWave18c_NetworkShaping_TargetsADBPortNotConsolePort(t *testing.T) {
	exec := &fakeExecutor{}
	stub := &adbStubEmulator{
		exec:   exec,
		adbBin: "/sdk/platform-tools/adb",
		passed: true,
		runOut: "BUILD SUCCESSFUL",
	}
	res := runMatrixWithAdbStub(t, stub, MatrixConfig{NetworkProfile: "4g"})
	if !res.AllPassed() {
		t.Fatalf("expected matrix to pass; got %+v", res)
	}
	var sawNetwork bool
	for _, c := range exec.calls {
		argString := strings.Join(c.Args, " ")
		if !strings.Contains(argString, "emu network") {
			continue
		}
		sawNetwork = true
		// ConsolePort=5554, ADBPort=5555 — the adb transport is 5555.
		if strings.Contains(argString, "-s localhost:5554") {
			t.Fatalf("network shaping targets the CONSOLE port (localhost:5554) — must target the adb transport localhost:5555; args=%v", c.Args)
		}
		if !strings.Contains(argString, "-s localhost:5555") {
			t.Fatalf("network shaping MUST target the adb transport localhost:5555; args=%v", c.Args)
		}
	}
	if !sawNetwork {
		t.Fatalf("expected an `adb emu network` invocation; got %d calls", len(exec.calls))
	}
}

func TestWave18c_FailureScreenshot_TargetsADBPortNotConsolePort(t *testing.T) {
	exec := &fakeExecutor{}
	stub := &adbStubEmulator{
		exec:   exec,
		adbBin: "/sdk/platform-tools/adb",
		passed: false, // fail the row so the failure-screenshot path runs
		runOut: "BUILD FAILED",
	}
	res := runMatrixWithAdbStub(t, stub, MatrixConfig{
		CaptureScreenshotOnFailure: true,
	})
	if res.AllPassed() {
		t.Fatalf("expected the row to fail so the screenshot path runs; got %+v", res)
	}
	var sawScreencap bool
	for _, c := range exec.calls {
		argString := strings.Join(c.Args, " ")
		if !strings.Contains(argString, "screencap") {
			continue
		}
		sawScreencap = true
		if strings.Contains(argString, "-s localhost:5554") {
			t.Fatalf("failure screenshot targets the CONSOLE port (localhost:5554) — must target the adb transport localhost:5555; args=%v", c.Args)
		}
		if !strings.Contains(argString, "-s localhost:5555") {
			t.Fatalf("failure screenshot MUST target the adb transport localhost:5555; args=%v", c.Args)
		}
	}
	if !sawScreencap {
		t.Fatalf("expected an `adb ... screencap` invocation on the failed row; got %d calls", len(exec.calls))
	}
}
