package cuttlefish

import "strings"

// health.go — Cuttlefish readiness via `adb devices` parsing.
//
// The Tier-2 readiness signal (FACT, docs/design/CUTTLEFISH_TIER2.md §4.3)
// is "the device appears in `adb devices`" after `launch_cvd --daemon`.
// This file parses that output and reduces it to a Ready boolean for the
// configured serial. It composes with pkg/health's readiness-probe
// discipline: readiness is OBSERVED on the wire (the device is listed AND
// in the online "device" state), never assumed.

// ParseADBDevices parses the output of `adb devices` into a slice of
// ADBDevice records. The first "List of devices attached" header line and
// any blank lines are skipped; every remaining line is split on
// whitespace into a serial + state pair. Lines that do not have at least a
// serial and a state are ignored (malformed/partial output is not a
// device).
//
// `adb devices` output shape:
//
//	List of devices attached
//	0.0.0.0:6520    device
//	emulator-5554   offline
//
// Anti-bluff (Bluff-Audit — recorded in the implementing commit body):
//
//	Mutation: treat any listed serial as online regardless of its state.
//	Observed: TestParseADBDevices_OfflineNotOnline asserts an "offline"
//	          serial's Online() is false; the mutated parser reports it
//	          online, failing the test.
//	Reverted: yes.
func ParseADBDevices(out []byte) []ADBDevice {
	var devices []ADBDevice
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip the header line emitted by `adb devices`.
		if strings.HasPrefix(line, "List of devices attached") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devices = append(devices, ADBDevice{
			Serial: fields[0],
			State:  fields[1],
		})
	}
	return devices
}

// readinessForSerial computes whether the parsed device list satisfies the
// readiness condition for the given serial:
//
//   - serial == "": ready iff ANY device is online ("device" state).
//   - serial != "": ready iff the named serial is present AND online.
//
// Resolving by the stable adb serial (NAME), never by list position, per
// §11.4.111 — the order of `adb devices` is not deterministic across runs.
func readinessForSerial(devices []ADBDevice, serial string) bool {
	for _, d := range devices {
		if serial == "" {
			if d.Online() {
				return true
			}
			continue
		}
		if d.Serial == serial && d.Online() {
			return true
		}
	}
	return false
}
