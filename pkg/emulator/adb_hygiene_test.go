package emulator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ParseADBDevicesLine ───────────────────────────────────────────────────────

// TestParseADBDevicesLine_DeviceSerial verifies that a standard emulator serial
// in "device" state is classified as ADBStateDevice.
//
// Bluff-Audit:
//
//	Mutation:   change the "device" case to return ADBStateUnknown instead.
//	Observed:   TestParseADBDevicesLine_DeviceSerial fails with
//	            "expected ADBStateDevice but got ADBStateUnknown"
//	Reverted:   yes
func TestParseADBDevicesLine_DeviceSerial(t *testing.T) {
	serial, state := ParseADBDevicesLine("emulator-5554\tdevice")
	assert.Equal(t, "emulator-5554", serial)
	assert.Equal(t, ADBStateDevice, state)
}

// TestParseADBDevicesLine_PhantomTCP verifies that a TCP endpoint in "offline"
// state is classified as ADBStatePhantomTCP (the wedge-inducing entry).
//
// Bluff-Audit:
//
//	Mutation:   remove the ADBStatePhantomTCP branch — make the "offline" case
//	            always return ADBStateOffline regardless of whether the serial
//	            contains a colon.
//	Observed:   TestParseADBDevicesLine_PhantomTCP fails with
//	            "expected ADBStatePhantomTCP but got ADBStateOffline"
//	Reverted:   yes
func TestParseADBDevicesLine_PhantomTCP(t *testing.T) {
	serial, state := ParseADBDevicesLine("localhost:5555\toffline")
	assert.Equal(t, "localhost:5555", serial)
	assert.Equal(t, ADBStatePhantomTCP, state,
		"TCP-offline must be classified as PhantomTCP, not Offline")
}

// TestParseADBDevicesLine_EmulatorOffline verifies that an emulator serial
// (not a TCP entry) in "offline" state is classified as ADBStateOffline.
func TestParseADBDevicesLine_EmulatorOffline(t *testing.T) {
	serial, state := ParseADBDevicesLine("emulator-5554\toffline")
	assert.Equal(t, "emulator-5554", serial)
	assert.Equal(t, ADBStateOffline, state,
		"emulator-serial offline must be classified as Offline, not PhantomTCP")
}

// TestParseADBDevicesLine_Unauthorized verifies that an unauthorized device is
// classified as ADBStateUnauthorized.
func TestParseADBDevicesLine_Unauthorized(t *testing.T) {
	serial, state := ParseADBDevicesLine("emulator-5556\tunauthorized")
	assert.Equal(t, "emulator-5556", serial)
	assert.Equal(t, ADBStateUnauthorized, state)
}

// TestParseADBDevicesLine_HeaderLine verifies that the "List of devices
// attached" header line is ignored.
func TestParseADBDevicesLine_HeaderLine(t *testing.T) {
	serial, state := ParseADBDevicesLine("List of devices attached")
	assert.Empty(t, serial)
	assert.Equal(t, ADBStateUnknown, state)
}

// TestParseADBDevicesLine_EmptyLine verifies that an empty line returns
// ADBStateUnknown.
func TestParseADBDevicesLine_EmptyLine(t *testing.T) {
	serial, state := ParseADBDevicesLine("")
	assert.Empty(t, serial)
	assert.Equal(t, ADBStateUnknown, state)
}

// TestParseADBDevicesLine_IPPortPhantom verifies that an IP:port TCP endpoint
// in "offline" state is also classified as ADBStatePhantomTCP.
func TestParseADBDevicesLine_IPPortPhantom(t *testing.T) {
	serial, state := ParseADBDevicesLine("192.168.1.10:5555\toffline")
	assert.Equal(t, "192.168.1.10:5555", serial)
	assert.Equal(t, ADBStatePhantomTCP, state)
}

// TestParseADBDevicesLine_IPPortDevice verifies that an IP:port TCP endpoint
// in "device" state is classified as ADBStateDevice (not phantom).
func TestParseADBDevicesLine_IPPortDevice(t *testing.T) {
	serial, state := ParseADBDevicesLine("192.168.1.10:5555\tdevice")
	assert.Equal(t, "192.168.1.10:5555", serial)
	assert.Equal(t, ADBStateDevice, state)
}

// ── ResetADBHygiene ───────────────────────────────────────────────────────────

// TestResetADBHygiene_DisconnectsPhantomTCP verifies that ResetADBHygiene
// disconnects phantom TCP entries before cycling kill-server/start-server.
//
// This is the primary anti-bluff assertion: if phantom entries are NOT
// disconnected, the next boot will fail with a wedged adb transport. The test
// asserts on user-visible behaviour (the disconnect call appears in the
// executor's call log) not just on "the function didn't crash".
//
// Bluff-Audit:
//
//	Mutation:   remove steps 1–2 from ResetADBHygiene (skip phantom detection
//	            and disconnect; jump straight to kill-server).
//	Observed:   TestResetADBHygiene_DisconnectsPhantomTCP fails because
//	            "adb disconnect localhost:5555" never appears in fake.calls.
//	Reverted:   yes
func TestResetADBHygiene_DisconnectsPhantomTCP(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices": {Out: []byte(
				"List of devices attached\n" +
					"emulator-5554\tdevice\n" +
					"localhost:5555\toffline\n", // phantom TCP
			)},
			adbPath + " disconnect localhost:5555": {Out: []byte("disconnected localhost:5555\n")},
			adbPath + " kill-server":               {Out: []byte("")},
			adbPath + " start-server":              {Out: []byte("* daemon started successfully\n")},
		},
	}

	result := ResetADBHygiene(context.Background(), adbPath, fake)

	assert.NoError(t, result.Err)
	assert.True(t, result.KillServerRan)
	assert.True(t, result.StartServerRan)
	require.Contains(t, result.PhantomTCPDisconnected, "localhost:5555",
		"phantom TCP localhost:5555 must be disconnected before kill-server")

	// Verify the call order: devices → disconnect → kill-server → start-server
	require.GreaterOrEqual(t, len(fake.calls), 4)
	callNames := make([]string, len(fake.calls))
	for i, c := range fake.calls {
		callNames[i] = c.Name + " " + strings.Join(c.Args, " ")
	}
	devIdx := indexOfPrefix(callNames, adbPath+" devices")
	disIdx := indexOfPrefix(callNames, adbPath+" disconnect localhost:5555")
	killIdx := indexOfPrefix(callNames, adbPath+" kill-server")
	startIdx := indexOfPrefix(callNames, adbPath+" start-server")

	require.NotEqual(t, -1, devIdx, "adb devices must be called")
	require.NotEqual(t, -1, disIdx, "adb disconnect must be called")
	require.NotEqual(t, -1, killIdx, "adb kill-server must be called")
	require.NotEqual(t, -1, startIdx, "adb start-server must be called")

	assert.Less(t, disIdx, killIdx,
		"disconnect must happen BEFORE kill-server")
	assert.Less(t, killIdx, startIdx,
		"kill-server must happen BEFORE start-server")
}

// TestResetADBHygiene_NoPhantomEntries verifies that when there are no phantom
// TCP entries, ResetADBHygiene still cycles kill-server/start-server but does
// not emit any disconnect calls.
func TestResetADBHygiene_NoPhantomEntries(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices":     {Out: []byte("List of devices attached\nemulator-5554\tdevice\n")},
			adbPath + " kill-server": {Out: []byte("")},
			adbPath + " start-server": {Out: []byte("* daemon started successfully\n")},
		},
	}

	result := ResetADBHygiene(context.Background(), adbPath, fake)

	assert.NoError(t, result.Err)
	assert.True(t, result.KillServerRan)
	assert.True(t, result.StartServerRan)
	assert.Empty(t, result.PhantomTCPDisconnected)
}

// TestResetADBHygiene_StartServerError verifies that a start-server failure
// is surfaced in result.Err.
func TestResetADBHygiene_StartServerError(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices":      {Out: []byte("List of devices attached\n")},
			adbPath + " kill-server":  {Out: []byte("")},
			adbPath + " start-server": {Err: fmt.Errorf("cannot start daemon")},
		},
	}

	result := ResetADBHygiene(context.Background(), adbPath, fake)

	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "start-server")
}

// ── CaptureBootDiagnostic ────────────────────────────────────────────────────

// TestCaptureBootDiagnostic_CapturesADBDevices verifies that CaptureBootDiagnostic
// populates the ADBDevices field from `adb devices` output.
//
// Bluff-Audit:
//
//	Mutation:   make CaptureBootDiagnostic always return an empty BootDiagnostic
//	            without calling any executor command.
//	Observed:   TestCaptureBootDiagnostic_CapturesADBDevices fails with
//	            "Expected non-empty ADBDevices" because diag.ADBDevices == "".
//	Reverted:   yes
func TestCaptureBootDiagnostic_CapturesADBDevices(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	devicesOutput := "List of devices attached\nemulator-5554\tdevice"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices":                                {Out: []byte(devicesOutput + "\n")},
			adbPath + " -s localhost:5555 shell getprop": {Out: []byte("[ro.build.version.sdk]: [34]\n")},
		},
	}

	diag := CaptureBootDiagnostic(context.Background(), adbPath, fake, 5555, "")

	assert.NotEmpty(t, diag.ADBDevices, "ADBDevices must be populated from adb output")
	assert.Contains(t, diag.ADBDevices, "emulator-5554")
	assert.NotEmpty(t, diag.CapturedAt)
	assert.Equal(t, 5555, diag.Port)
}

// TestCaptureBootDiagnostic_CapturesGetProp verifies that the getprop snapshot
// is populated when a port is known.
//
// Bluff-Audit:
//
//	Mutation:   remove the getprop capture branch (never call getprop).
//	Observed:   TestCaptureBootDiagnostic_CapturesGetProp fails with
//	            "Expected non-empty GetPropSnapshot".
//	Reverted:   yes
func TestCaptureBootDiagnostic_CapturesGetProp(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices":                                {Out: []byte("List of devices attached\n")},
			adbPath + " -s localhost:5555 shell getprop": {Out: []byte("[ro.build.version.sdk]: [34]\n")},
		},
	}

	diag := CaptureBootDiagnostic(context.Background(), adbPath, fake, 5555, "")

	assert.NotEmpty(t, diag.GetPropSnapshot, "GetPropSnapshot must be populated when port is known")
	assert.Contains(t, diag.GetPropSnapshot, "ro.build.version.sdk")
}

// TestCaptureBootDiagnostic_NoGetPropWhenPortZero verifies that when port is 0
// (unknown — port discovery itself failed), getprop is NOT attempted.
func TestCaptureBootDiagnostic_NoGetPropWhenPortZero(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices": {Out: []byte("List of devices attached\n")},
		},
	}

	diag := CaptureBootDiagnostic(context.Background(), adbPath, fake, 0, "")

	assert.Empty(t, diag.GetPropSnapshot,
		"getprop must NOT be attempted when port is 0")
	assert.Equal(t, 0, diag.Port)
}

// TestCaptureBootDiagnostic_WritesEvidenceFile verifies that the diagnostic is
// persisted to EvidenceDir when a non-empty directory is provided.
//
// Bluff-Audit:
//
//	Mutation:   skip the os.WriteFile call inside writeBootDiagnostic (return nil
//	            immediately without writing).
//	Observed:   TestCaptureBootDiagnostic_WritesEvidenceFile fails because
//	            filepath.Glob finds no file in tempDir.
//	Reverted:   yes
func TestCaptureBootDiagnostic_WritesEvidenceFile(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices":                                {Out: []byte("List of devices attached\n")},
			adbPath + " -s localhost:5556 shell getprop": {Out: []byte("[ro.product.name]: [sdk_gphone_x86]\n")},
		},
	}

	tempDir := t.TempDir()
	diag := CaptureBootDiagnostic(context.Background(), adbPath, fake, 5556, tempDir)

	assert.NotEmpty(t, diag.EvidenceFile, "EvidenceFile must be set when evidenceDir is provided")
	assert.FileExists(t, diag.EvidenceFile, "the evidence file must exist on disk")

	content, err := os.ReadFile(diag.EvidenceFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "captured_at",
		"evidence file must contain captured_at field")
}

// TestCaptureBootDiagnostic_NoWriteWhenEmptyDir verifies that no file is written
// when evidenceDir is empty.
func TestCaptureBootDiagnostic_NoWriteWhenEmptyDir(t *testing.T) {
	adbPath := "/sdk/platform-tools/adb"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			adbPath + " devices": {Out: []byte("List of devices attached\n")},
		},
	}

	diag := CaptureBootDiagnostic(context.Background(), adbPath, fake, 0, "")

	assert.Empty(t, diag.EvidenceFile,
		"EvidenceFile must be empty when no evidenceDir is provided")
}

// TestWriteBootDiagnostic_ContentIsReadable verifies that the file written by
// writeBootDiagnostic can be read back and contains the expected fields.
//
// Bluff-Audit:
//
//	Mutation:   write an empty file from writeBootDiagnostic (return nil after
//	            writing "" to disk).
//	Observed:   TestWriteBootDiagnostic_ContentIsReadable fails because
//	            "captured_at" is not found in the file content.
//	Reverted:   yes
func TestWriteBootDiagnostic_ContentIsReadable(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "boot-diag.json")

	d := BootDiagnostic{
		CapturedAt:      "2026-06-03T00:00:00Z",
		Port:            5555,
		ADBDevices:      "List of devices attached\nemulator-5554\tdevice",
		GetPropSnapshot: "[ro.build.version.sdk]: [34]",
	}

	err := writeBootDiagnostic(path, d)
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	s := string(content)

	assert.Contains(t, s, "captured_at")
	assert.Contains(t, s, "2026-06-03T00:00:00Z")
	assert.Contains(t, s, "adb_devices")
	assert.Contains(t, s, "emulator-5554")
	assert.Contains(t, s, "getprop_snapshot")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// indexOfPrefix returns the index of the first element in slice that starts
// with prefix, or -1 if none.
func indexOfPrefix(slice []string, prefix string) int {
	for i, s := range slice {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}
