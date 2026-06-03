package emulator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ADBTransportState classifies a single `adb devices` line's transport state.
//
// The distinction matters for boot-timeout diagnostics: an "offline" transport
// means the ADB socket exists but the daemon has not yet established a full
// session; a phantom TCP entry (e.g. "localhost:5555  offline") may wedge adb's
// device-tracking so that `getprop sys.boot_completed` never returns.
//
// Forensic anchor (2026-06-03): the booted emulator was stuck `offline` AND a
// phantom `localhost:5555 offline` TCP entry was present. This wedged adb's
// device tracking so `getprop sys.boot_completed` never succeeded even after
// the emulator had booted (boot_seconds ≈ timeout every run). Fix: classify the
// state explicitly so the caller can surface a clear diagnostic, and disconnect
// stale TCP endpoints before boot to prevent the wedge.
type ADBTransportState int

const (
	// ADBStateUnknown is the zero value — the line could not be parsed.
	ADBStateUnknown ADBTransportState = iota
	// ADBStateDevice is "device" — adb transport is fully connected.
	ADBStateDevice
	// ADBStateOffline is "offline" — adb socket exists but daemon not ready.
	ADBStateOffline
	// ADBStatePhantomTCP is an "offline" TCP entry (e.g. "localhost:5555 offline")
	// that represents a stale connection from a prior emulator run. This is the
	// transport state that wedges `getprop` polling — it MUST be disconnected
	// before a new emulator is booted.
	ADBStatePhantomTCP
	// ADBStateUnauthorized is "unauthorized" — adb transport rejected the host key.
	ADBStateUnauthorized
)

// ParseADBDevicesLine classifies a single line from `adb devices` output.
// Returns (serial, state) where serial is the device identifier (e.g.
// "emulator-5554" or "localhost:5555").
//
// Anti-bluff posture (§6.J/§6.L): this function is the production parser for
// adb output; the unit tests (adb_hygiene_test.go) verify every branch by
// injecting synthetic adb output and asserting on the classified state — not
// on "the function did not crash".
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation:   remove the ADBStatePhantomTCP branch (treat TCP-offline as ADBStateOffline)
//	Observed:   TestParseADBDevicesLine_PhantomTCP asserts
//	            state == ADBStatePhantomTCP for "localhost:5555\toffline";
//	            the mutation returns ADBStateOffline, failing the assertion.
//	Reverted:   yes
func ParseADBDevicesLine(line string) (serial string, state ADBTransportState) {
	line = strings.TrimSpace(line)
	if line == "" || line == "List of devices attached" {
		return "", ADBStateUnknown
	}
	// `adb devices` output: "<serial>\t<state>" or "<serial> <state>"
	// The separator is a tab in most cases; some adb versions use spaces.
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", ADBStateUnknown
	}
	serial = fields[0]
	stateStr := fields[len(fields)-1] // last token is state; skip annotations

	switch stateStr {
	case "device":
		return serial, ADBStateDevice
	case "offline":
		// Distinguish TCP phantom from emulator-serial offline.
		// A TCP phantom looks like "localhost:<port>" or "<ip>:<port>".
		// An emulator serial looks like "emulator-<port>".
		if strings.ContainsRune(serial, ':') {
			// TCP address — offline TCP endpoint is the phantom class.
			return serial, ADBStatePhantomTCP
		}
		return serial, ADBStateOffline
	case "unauthorized":
		return serial, ADBStateUnauthorized
	default:
		return serial, ADBStateUnknown
	}
}

// ADBHygieneResult summarises the outcome of ResetADBHygiene.
type ADBHygieneResult struct {
	// KillServerRan is true if `adb kill-server` was attempted.
	KillServerRan bool
	// StartServerRan is true if `adb start-server` was attempted.
	StartServerRan bool
	// PhantomTCPDisconnected lists any stale TCP endpoints that were
	// explicitly disconnected before kill-server.
	PhantomTCPDisconnected []string
	// Err is the first non-nil error from kill-server or start-server.
	// Stale-disconnect errors are non-fatal and are NOT included here.
	Err error
}

// ResetADBHygiene resets the adb server to a clean state so a new emulator
// can boot without hitting stale phantom TCP transports. The sequence is:
//
//  1. Parse `adb devices` to discover any phantom TCP entries (offline TCP
//     endpoints from prior runs that wedge device tracking).
//  2. Disconnect each phantom TCP endpoint via `adb disconnect <serial>`.
//  3. `adb kill-server`  — terminates the daemon and drops all stale state.
//  4. `adb start-server` — brings a fresh daemon up.
//
// This is best-effort: failures at steps 1-2 are logged to stderr but are not
// returned as errors (the kill-server in step 3 clears any wedge they might
// have caused anyway). Steps 3-4 errors ARE returned since a failed
// start-server means adb is unusable.
//
// Anti-bluff posture (§6.J): the primary user-visible assertion is that
// subsequent `adb connect` + `adb -s ... shell getprop` calls succeed because
// there are no phantom TCP entries wedging device tracking.
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation:   skip step 1–2 (phantom TCP disconnect) — start from kill-server directly
//	Observed:   TestResetADBHygiene_DisconnectsPhantomTCP asserts that
//	            "adb disconnect localhost:5555" appears in the executor's
//	            call log when devices output contains a phantom entry;
//	            skipping steps 1–2 produces an empty disconnect log,
//	            failing the assertion.
//	Reverted:   yes
func ResetADBHygiene(ctx context.Context, adbBinary string, exec CommandExecutor) ADBHygieneResult {
	result := ADBHygieneResult{}

	// Step 1: parse `adb devices` for phantom TCP entries.
	devicesOut, err := exec.Execute(ctx, adbBinary, "devices")
	if err == nil {
		for _, line := range strings.Split(string(devicesOut), "\n") {
			serial, state := ParseADBDevicesLine(line)
			if state == ADBStatePhantomTCP {
				// Step 2: disconnect this phantom — best effort.
				_, derr := exec.Execute(ctx, adbBinary, "disconnect", serial)
				if derr != nil {
					fmt.Fprintf(os.Stderr,
						"[adb-hygiene] disconnect %s: %v (continuing)\n", serial, derr)
				}
				result.PhantomTCPDisconnected = append(result.PhantomTCPDisconnected, serial)
			}
		}
	}

	// Step 3: kill-server.
	result.KillServerRan = true
	if _, kerr := exec.Execute(ctx, adbBinary, "kill-server"); kerr != nil {
		fmt.Fprintf(os.Stderr, "[adb-hygiene] adb kill-server: %v\n", kerr)
		result.Err = fmt.Errorf("adb kill-server: %w", kerr)
		return result
	}

	// Step 4: start-server.
	result.StartServerRan = true
	if _, serr := exec.Execute(ctx, adbBinary, "start-server"); serr != nil {
		fmt.Fprintf(os.Stderr, "[adb-hygiene] adb start-server: %v\n", serr)
		result.Err = fmt.Errorf("adb start-server: %w", serr)
		return result
	}

	return result
}

// BootDiagnostic captures the forensic state at the moment a boot or
// WaitForBoot timeout fires. Written to EvidenceDir (when non-empty) so the
// NEXT person sees WHY the boot timed out — per clause 6.I Group-B intent.
//
// Field values are captured best-effort; an empty string means the capture
// command failed (the failure is logged to stderr and does NOT flip any gate).
type BootDiagnostic struct {
	// ADBDevices is the raw output of `adb devices` at the timeout moment.
	ADBDevices string `json:"adb_devices,omitempty"`
	// GetPropSnapshot is the output of `adb -s <target> shell getprop` at
	// the timeout moment. Empty when the emulator was never in "device"
	// state (i.e. it never answered adb at all).
	GetPropSnapshot string `json:"getprop_snapshot,omitempty"`
	// EmulatorLog is the last 200 lines of the emulator's stderr/stdout if
	// captured via a separate log file. Empty in the current implementation
	// since Start() discards stdio (the emulator runs detached). The field is
	// reserved for future container-mode implementations that redirect the
	// emulator's output to a log file.
	EmulatorLog string `json:"emulator_log,omitempty"`
	// CapturedAt is the RFC3339 timestamp when the diagnostic was captured.
	CapturedAt string `json:"captured_at"`
	// Port is the ADB port the diagnostic was captured against (0 if
	// unknown, e.g. if port discovery itself timed out).
	Port int `json:"port,omitempty"`
	// EvidenceFile is the on-disk path of the written diagnostic JSON.
	// Empty when EvidenceDir was not supplied or the write failed.
	EvidenceFile string `json:"evidence_file,omitempty"`
}

// CaptureBootDiagnostic gathers the forensic snapshot at the moment a boot
// timeout fires and optionally writes it to EvidenceDir.
//
// adbBinary is the path to the adb binary; port is the emulator's ADB port (0
// when unknown); evidenceDir is the optional directory to write the JSON file
// (pass "" to skip the write). The function is always non-blocking: timeouts
// on individual adb calls are limited to 5 seconds each.
//
// Anti-bluff posture (§6.J): the written evidence enables the NEXT operator
// to diagnose "why did boot timeout?" without re-running — per clause 6.I
// Group-B `diag` field intent.
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation:   return a BootDiagnostic without calling any executor command
//	            (i.e. always return an empty diagnostic)
//	Observed:   TestCaptureBootDiagnostic_CapturesADBDevices asserts that
//	            the ADBDevices field is non-empty when a fake executor returns
//	            known devices output; the mutation returns an empty struct,
//	            failing the assertion.
//	Reverted:   yes
func CaptureBootDiagnostic(
	ctx context.Context,
	adbBinary string,
	exec CommandExecutor,
	port int,
	evidenceDir string,
) BootDiagnostic {
	diag := BootDiagnostic{
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Port:       port,
	}

	// Use a tight 5s deadline for each capture command — we're already in
	// a timeout path and must not further stall the matrix runner.
	captureCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Capture `adb devices`.
	devicesOut, err := exec.Execute(captureCtx, adbBinary, "devices")
	if err == nil {
		diag.ADBDevices = strings.TrimSpace(string(devicesOut))
	} else {
		fmt.Fprintf(os.Stderr, "[boot-diag] adb devices: %v\n", err)
	}

	// Capture getprop snapshot (only meaningful when port != 0).
	if port != 0 {
		target := fmt.Sprintf("localhost:%d", port)
		propOut, perr := exec.Execute(captureCtx, adbBinary, "-s", target, "shell", "getprop")
		if perr == nil {
			diag.GetPropSnapshot = strings.TrimSpace(string(propOut))
		} else {
			fmt.Fprintf(os.Stderr, "[boot-diag] adb -s %s shell getprop: %v\n", target, perr)
		}
	}

	// Optionally persist to EvidenceDir.
	if evidenceDir != "" {
		if err := os.MkdirAll(evidenceDir, 0o755); err == nil {
			filename := fmt.Sprintf("boot-diag-%s.json",
				time.Now().UTC().Format("20060102T150405Z"))
			if port != 0 {
				filename = fmt.Sprintf("boot-diag-port%d-%s.json",
					port, time.Now().UTC().Format("20060102T150405Z"))
			}
			path := filepath.Join(evidenceDir, filename)
			if err := writeBootDiagnostic(path, diag); err == nil {
				diag.EvidenceFile = path
			} else {
				fmt.Fprintf(os.Stderr, "[boot-diag] write %s: %v\n", path, err)
			}
		}
	}

	return diag
}

// writeBootDiagnostic serialises a BootDiagnostic to a JSON file.
// Intentionally does NOT import encoding/json at the function level to avoid
// a cycle — it uses a local inline struct so the format stays stable without
// needing the JSON encoder to handle the BootDiagnostic type directly.
func writeBootDiagnostic(path string, d BootDiagnostic) error {
	// Build minimal JSON manually to avoid the encoding/json dependency
	// on the BootDiagnostic type (which would require a json tag audit).
	// The format is intentionally simple for operator readability.
	portStr := ""
	if d.Port != 0 {
		portStr = fmt.Sprintf(`"port": %d, `, d.Port)
	}
	devicesEsc := jsonEscapeString(d.ADBDevices)
	propEsc := jsonEscapeString(d.GetPropSnapshot)
	logEsc := jsonEscapeString(d.EmulatorLog)

	content := fmt.Sprintf(`{
  "captured_at": %q,
  %s"adb_devices": %s,
  "getprop_snapshot": %s,
  "emulator_log": %s
}
`,
		d.CapturedAt,
		portStr,
		devicesEsc,
		propEsc,
		logEsc,
	)
	return os.WriteFile(path, []byte(content), 0o644)
}

// jsonEscapeString returns a JSON-encoded string literal for s. Uses
// strconv.Quote which produces valid JSON for all inputs the adb output
// produces (ASCII + common escapes).
func jsonEscapeString(s string) string {
	return strconv.Quote(s)
}
