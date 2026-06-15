package applesim

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Captured verbatim from a real `xcrun simctl list devices -j` on macOS,
// Xcode 16.4 (operator host, 2026-06-15). The parser is pinned to THIS real
// format so a simctl output-format change is caught, not silently mis-parsed.
const realListJSON = `{
  "devices" : {
    "com.apple.CoreSimulator.SimRuntime.iOS-17-2" : [
      {
        "lastBootedAt" : "2025-10-27T17:13:18Z",
        "dataPath" : "/Users/op/Library/Developer/CoreSimulator/Devices/E868060E-3394-48BD-8B2E-987199B33F43/data",
        "dataPathSize" : 510234624,
        "udid" : "E868060E-3394-48BD-8B2E-987199B33F43",
        "isAvailable" : true,
        "deviceTypeIdentifier" : "com.apple.CoreSimulator.SimDeviceType.iPhone-15-Pro",
        "state" : "Shutdown",
        "name" : "iPhone 15 Pro"
      }
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-18-5" : [
      {
        "udid" : "93EE2C69-4BC3-4923-B651-654FBA1CDE45",
        "isAvailable" : true,
        "deviceTypeIdentifier" : "com.apple.CoreSimulator.SimDeviceType.iPhone-14",
        "state" : "Booted",
        "name" : "iPhone 14"
      }
    ]
  }
}`

func findByUDID(devices []Device, udid string) (Device, bool) {
	for _, d := range devices {
		if d.UDID == udid {
			return d, true
		}
	}
	return Device{}, false
}

func TestParseListJSON_realOutput(t *testing.T) {
	devices, err := parseListJSON(realListJSON)
	if err != nil {
		t.Fatalf("parseListJSON error: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d: %+v", len(devices), devices)
	}

	booted, ok := findByUDID(devices, "93EE2C69-4BC3-4923-B651-654FBA1CDE45")
	if !ok {
		t.Fatalf("iPhone 14 device not found in %+v", devices)
	}
	if booted.Name != "iPhone 14" {
		t.Errorf("Name = %q, want iPhone 14", booted.Name)
	}
	if !booted.IsBooted() {
		t.Errorf("IsBooted() = false, want true for state Booted")
	}
	if booted.Runtime != "com.apple.CoreSimulator.SimRuntime.iOS-18-5" {
		t.Errorf("Runtime = %q, want iOS-18-5", booted.Runtime)
	}
	if booted.DeviceTypeIdentifier != "com.apple.CoreSimulator.SimDeviceType.iPhone-14" {
		t.Errorf("DeviceTypeIdentifier = %q, want iPhone-14 type", booted.DeviceTypeIdentifier)
	}

	shutdown, ok := findByUDID(devices, "E868060E-3394-48BD-8B2E-987199B33F43")
	if !ok {
		t.Fatalf("iPhone 15 Pro device not found")
	}
	if shutdown.IsBooted() {
		t.Errorf("iPhone 15 Pro IsBooted() = true, want false for state Shutdown")
	}
	if !shutdown.IsAvailable {
		t.Errorf("iPhone 15 Pro IsAvailable = false, want true")
	}
}

func TestParseListJSON_emptyDevices(t *testing.T) {
	devices, err := parseListJSON(`{"devices":{}}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devices))
	}
}

func TestParseListJSON_malformed(t *testing.T) {
	if _, err := parseListJSON("not json"); err == nil {
		t.Error("expected error parsing malformed JSON, got nil")
	}
}

func TestParseVersion(t *testing.T) {
	v := parseVersion("Xcode 16.4\nBuild version 16F6")
	if v != "Xcode 16.4" {
		t.Errorf("parseVersion = %q, want \"Xcode 16.4\"", v)
	}
	if got := parseVersion("\n\n  Xcode 15.0  \n"); got != "Xcode 15.0" {
		t.Errorf("parseVersion trimmed = %q, want \"Xcode 15.0\"", got)
	}
	if got := parseVersion(""); got != "" {
		t.Errorf("parseVersion empty = %q, want empty", got)
	}
}

// fakeExec records the args it was called with and returns scripted output.
type fakeExec struct {
	gotArgs [][]string
	out     string
	err     error
}

func (f *fakeExec) run(_ context.Context, _ string, args ...string) (string, error) {
	f.gotArgs = append(f.gotArgs, args)
	return f.out, f.err
}

func newToolWithExec(f *fakeExec) *Tool {
	return &Tool{Path: "/usr/bin/xcrun", exec: f.run}
}

func TestTool_List_invokesSimctlListDevicesJSON(t *testing.T) {
	f := &fakeExec{out: realListJSON}
	tool := newToolWithExec(f)
	devices, err := tool.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if len(f.gotArgs) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(f.gotArgs))
	}
	want := []string{"simctl", "list", "devices", "-j"}
	if strings.Join(f.gotArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("invoked with %v, want %v", f.gotArgs[0], want)
	}
}

func TestTool_Running_filtersBooted(t *testing.T) {
	f := &fakeExec{out: realListJSON}
	booted, err := newToolWithExec(f).Running(context.Background())
	if err != nil {
		t.Fatalf("Running error: %v", err)
	}
	if len(booted) != 1 {
		t.Fatalf("expected 1 booted device, got %d: %+v", len(booted), booted)
	}
	if booted[0].UDID != "93EE2C69-4BC3-4923-B651-654FBA1CDE45" {
		t.Errorf("booted UDID = %q, want the iPhone 14", booted[0].UDID)
	}
}

func TestTool_Resolve_byUDIDThenName(t *testing.T) {
	tool := newToolWithExec(&fakeExec{out: realListJSON})
	byUDID, err := tool.Resolve(context.Background(), "E868060E-3394-48BD-8B2E-987199B33F43")
	if err != nil || byUDID.Name != "iPhone 15 Pro" {
		t.Fatalf("Resolve by UDID = %+v, err %v", byUDID, err)
	}
	byName, err := tool.Resolve(context.Background(), "iPhone 14")
	if err != nil || byName.UDID != "93EE2C69-4BC3-4923-B651-654FBA1CDE45" {
		t.Fatalf("Resolve by name = %+v, err %v", byName, err)
	}
	if _, err := tool.Resolve(context.Background(), "nonexistent-device"); err == nil {
		t.Error("expected error resolving nonexistent device, got nil")
	}
}

func TestTool_Boot_invokesSimctlBoot(t *testing.T) {
	f := &fakeExec{out: ""}
	if err := newToolWithExec(f).Boot(context.Background(), "UDID-1"); err != nil {
		t.Fatalf("Boot error: %v", err)
	}
	want := []string{"simctl", "boot", "UDID-1"}
	if strings.Join(f.gotArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("invoked with %v, want %v", f.gotArgs[0], want)
	}
}

func TestTool_Boot_alreadyBootedIsBenign(t *testing.T) {
	f := &fakeExec{out: "Unable to boot device in current state: Booted", err: errors.New("exit 164")}
	if err := newToolWithExec(f).Boot(context.Background(), "UDID-1"); err != nil {
		t.Errorf("Boot on already-booted device returned error %v, want nil (benign)", err)
	}
}

func TestTool_Boot_realErrorPropagates(t *testing.T) {
	f := &fakeExec{out: "Invalid device: bogus", err: errors.New("exit 164")}
	if err := newToolWithExec(f).Boot(context.Background(), "bogus"); err == nil {
		t.Error("expected error for invalid device, got nil")
	}
}

func TestTool_Install_invokesSimctlInstall(t *testing.T) {
	f := &fakeExec{}
	if err := newToolWithExec(f).Install(context.Background(), "UDID-1", "/tmp/App.app"); err != nil {
		t.Fatalf("Install error: %v", err)
	}
	want := []string{"simctl", "install", "UDID-1", "/tmp/App.app"}
	if strings.Join(f.gotArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("invoked with %v, want %v", f.gotArgs[0], want)
	}
}

func TestTool_Launch_returnsPIDLine(t *testing.T) {
	f := &fakeExec{out: "com.example.app: 38941\n"}
	pid, err := newToolWithExec(f).Launch(context.Background(), "UDID-1", "com.example.app")
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	if pid != "com.example.app: 38941" {
		t.Errorf("Launch returned %q, want trimmed PID line", pid)
	}
	want := []string{"simctl", "launch", "UDID-1", "com.example.app"}
	if strings.Join(f.gotArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("invoked with %v, want %v", f.gotArgs[0], want)
	}
}

func TestTool_Shutdown_alreadyShutdownIsBenign(t *testing.T) {
	f := &fakeExec{out: "Unable to shutdown device in current state: Shutdown", err: errors.New("exit 164")}
	if err := newToolWithExec(f).Shutdown(context.Background(), "UDID-1"); err != nil {
		t.Errorf("Shutdown on already-shutdown device returned error %v, want nil (benign)", err)
	}
}

func TestTool_Screenshot_invokesSimctlIoScreenshot(t *testing.T) {
	f := &fakeExec{}
	if err := newToolWithExec(f).Screenshot(context.Background(), "UDID-1", "/tmp/shot.png"); err != nil {
		t.Fatalf("Screenshot error: %v", err)
	}
	want := []string{"simctl", "io", "UDID-1", "screenshot", "/tmp/shot.png"}
	if strings.Join(f.gotArgs[0], " ") != strings.Join(want, " ") {
		t.Errorf("invoked with %v, want %v", f.gotArgs[0], want)
	}
}

func TestDevice_IsBooted(t *testing.T) {
	if !(Device{State: "Booted"}).IsBooted() {
		t.Error("Booted state should report IsBooted true")
	}
	if (Device{State: "Shutdown"}).IsBooted() {
		t.Error("Shutdown state should report IsBooted false")
	}
}

func TestBootAndWait_timeoutSurfacesError(t *testing.T) {
	// exec returns an error for bootstatus → BootAndWait must surface it.
	f := &fakeExec{out: "timed out", err: errors.New("context deadline exceeded")}
	_, err := newToolWithExec(f).BootAndWait(context.Background(), "UDID-1", 10*time.Millisecond)
	if err == nil {
		t.Error("expected BootAndWait to surface bootstatus error, got nil")
	}
}
