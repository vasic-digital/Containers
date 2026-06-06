package genymotion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Captured verbatim from a real `gmtool admin list` on macOS, Genymotion 3.10.0
// (operator host, 2026-06-06). The parser is pinned to THIS real format so a
// gmtool output-format change is caught, not silently mis-parsed.
const realAdminList = ` State    |   ADB Serial    |                UUID                |      Name
----------+-----------------+------------------------------------+---------------
       On |  127.0.0.1:6555 |ed811e5e-df4d-46d8-ab1f-4250058e57e3| Google Pixel 9
`

const realVersion = `Version  : 3.10.0
Revision : 20260331-81a66429d0`

func TestParseList_realOutput(t *testing.T) {
	devices := parseList(realAdminList)
	if len(devices) != 1 {
		t.Fatalf("expected exactly 1 device, got %d: %+v", len(devices), devices)
	}
	d := devices[0]
	if d.State != "On" {
		t.Errorf("State = %q, want On", d.State)
	}
	if d.ADBSerial != "127.0.0.1:6555" {
		t.Errorf("ADBSerial = %q, want 127.0.0.1:6555", d.ADBSerial)
	}
	if d.UUID != "ed811e5e-df4d-46d8-ab1f-4250058e57e3" {
		t.Errorf("UUID = %q, want ed811e5e-df4d-46d8-ab1f-4250058e57e3", d.UUID)
	}
	if d.Name != "Google Pixel 9" {
		t.Errorf("Name = %q, want \"Google Pixel 9\"", d.Name)
	}
	if !d.IsOn() {
		t.Errorf("IsOn() = false, want true for state On")
	}
}

func TestParseList_skipsHeaderSeparatorAndOffState(t *testing.T) {
	out := ` State    |   ADB Serial    |                UUID                |      Name
----------+-----------------+------------------------------------+---------------
      Off |                 |aaaa1111-2222-3333-4444-555566667777| Pixel Tablet
       On |  127.0.0.1:6557 |bbbb1111-2222-3333-4444-555566667777| Pixel 9 Pro
`
	devices := parseList(out)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices (header+separator skipped), got %d: %+v", len(devices), devices)
	}
	if devices[0].Name != "Pixel Tablet" || devices[0].IsOn() {
		t.Errorf("row 0 = %+v, want Off Pixel Tablet", devices[0])
	}
	if devices[1].ADBSerial != "127.0.0.1:6557" || !devices[1].IsOn() {
		t.Errorf("row 1 = %+v, want On with serial 127.0.0.1:6557", devices[1])
	}
}

func TestParseVersion_realOutput(t *testing.T) {
	if got := parseVersion(realVersion); got != "3.10.0" {
		t.Errorf("parseVersion = %q, want 3.10.0", got)
	}
}

func TestCandidatePathsForOS(t *testing.T) {
	darwin := CandidatePathsForOS("darwin", "/Users/op")
	if len(darwin) == 0 || darwin[0] != "/Applications/Genymotion.app/Contents/MacOS/gmtool" {
		t.Errorf("darwin candidates wrong: %v", darwin)
	}
	linux := CandidatePathsForOS("linux", "/home/op")
	foundOpt := false
	for _, p := range linux {
		if p == "/opt/genymobile/genymotion/gmtool" {
			foundOpt = true
		}
	}
	if !foundOpt {
		t.Errorf("linux candidates missing /opt/genymobile/genymotion/gmtool: %v", linux)
	}
	if CandidatePathsForOS("windows", "/home/op") != nil {
		t.Errorf("unsupported OS should return nil candidates")
	}
}

// stubTool returns a Tool whose exec records the args it was called with and
// returns the supplied output/err — no live gmtool needed.
func stubTool(output string, execErr error, recorder *[][]string) *Tool {
	return &Tool{
		Path: "/fake/gmtool",
		exec: func(_ context.Context, _ string, args ...string) (string, error) {
			*recorder = append(*recorder, args)
			return output, execErr
		},
	}
}

func TestList_invokesAdminListAndParses(t *testing.T) {
	var calls [][]string
	tool := stubTool(realAdminList, nil, &calls)
	devices, err := tool.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "admin list" {
		t.Errorf("expected one `admin list` call, got %v", calls)
	}
	if len(devices) != 1 || devices[0].ADBSerial != "127.0.0.1:6555" {
		t.Errorf("parsed devices wrong: %+v", devices)
	}
}

func TestStartStop_commandConstruction(t *testing.T) {
	var calls [][]string
	tool := stubTool("", nil, &calls)
	if err := tool.Start(context.Background(), "Google Pixel 9"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tool.Stop(context.Background(), "Google Pixel 9"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
	if strings.Join(calls[0], " ") != "admin start Google Pixel 9" {
		t.Errorf("Start args = %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "admin stop Google Pixel 9" {
		t.Errorf("Stop args = %v", calls[1])
	}
}

func TestStart_propagatesExecError(t *testing.T) {
	var calls [][]string
	tool := stubTool("Error: device not found", errors.New("exit 1"), &calls)
	err := tool.Start(context.Background(), "Nope")
	if err == nil {
		t.Fatal("expected Start to propagate gmtool failure, got nil")
	}
	if !strings.Contains(err.Error(), "device not found") {
		t.Errorf("error should include gmtool stderr, got: %v", err)
	}
}

func TestStartAndWait_returnsRunningDeviceWithSerial(t *testing.T) {
	// First List → device still Off (no serial); second List → On with serial.
	outputs := []string{
		` State | ADB Serial | UUID | Name
------+------------+------+-----
  Off |            | u1   | Google Pixel 9
`,
		realAdminList,
	}
	var idx int
	tool := &Tool{Path: "/fake/gmtool", exec: func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "list" {
			out := outputs[idx]
			if idx < len(outputs)-1 {
				idx++
			}
			return out, nil
		}
		return "", nil // start
	}}
	d, err := tool.StartAndWait(context.Background(), "Google Pixel 9", 10*time.Second)
	if err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	if d.ADBSerial != "127.0.0.1:6555" || !d.IsOn() {
		t.Errorf("resolved device = %+v, want On with serial 127.0.0.1:6555", d)
	}
}
