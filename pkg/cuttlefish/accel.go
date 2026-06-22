package cuttlefish

import (
	"fmt"
	"os"
)

// accel.go — KVM-acceleration gating + host device-presence filtering for
// the Cuttlefish lifecycle.
//
// Cuttlefish, unlike the Android SDK emulator, has NO software-emulation
// fallback worth running: it boots a real Android VM via crosvm/QEMU and
// REQUIRES KVM hardware acceleration (`/dev/kvm`) plus the vhost/vsock/tun
// device nodes (FACT, docs/design/CUTTLEFISH_TIER2.md §3). The documented
// pre-flight is `find /dev -name kvm` / `grep -c -w "vmx\|svm" /proc/cpuinfo`.
//
// This file makes the host-capability check an explicit, pure-ish gate so
// callers SKIP-with-reason per §11.4.3 on a host that cannot run
// Cuttlefish (e.g. macOS / Apple Silicon — no `/dev/kvm`), instead of
// PASS-by-default or a guessed launch that fails opaquely.

// kvmDevicePath is the path to the KVM device. Package-level so tests can
// redirect it to a synthetic path without needing a real /dev/kvm.
// Production value is "/dev/kvm".
//
// Anti-bluff: TestRequireKVM_Absent verifies that when this path does not
// exist, RequireKVM returns a non-nil error so the caller SKIPs honestly.
var kvmDevicePath = "/dev/kvm"

// statDevice is the seam FilterPresentDevices uses to test device-node
// presence. Package-level so tests can inject a fixture set without
// creating real /dev nodes. Production value is os.Stat.
var statDevice = os.Stat

// KVMAvailable reports whether the host exposes the KVM device node at
// kvmDevicePath. A stat(2) is the least-privilege check — it asks whether
// the device exists, not whether it can be opened for writing.
//
// On Linux x86_64/arm64 with KVM enabled the node is present; on macOS the
// node does not exist (Apple HVF is a host-only API a Linux container
// cannot reach), so this returns false and the lifecycle cannot launch.
func KVMAvailable() bool {
	_, err := statDevice(kvmDevicePath)
	return err == nil
}

// RequireKVM returns nil when the host can hardware-accelerate Cuttlefish
// (KVM present) and a non-nil error otherwise. It is the topology gate
// (§11.4.3): a test or harness calls it at entry and, on a non-nil error,
// SKIPs-with-reason rather than reporting PASS or FAIL on a host that
// physically cannot run Cuttlefish.
//
// The error text names the exact missing capability + the documented
// pre-flight check so the operator can verify it directly.
func RequireKVM() error {
	if KVMAvailable() {
		return nil
	}
	return fmt.Errorf(
		"cuttlefish requires KVM hardware acceleration but %s is absent "+
			"(verify with: find /dev -name kvm; or: grep -c -w \"vmx\\|svm\" /proc/cpuinfo); "+
			"this host cannot run Cuttlefish — SKIP-with-reason per §11.4.3, never PASS-by-default",
		kvmDevicePath,
	)
}

// FilterPresentDevices partitions the requested device-node list into the
// subset the host actually exposes (present) and the subset it does not
// (missing). The lifecycle passes only `present` to `--device`, and
// surfaces `missing` so the caller SKIPs honestly when a required node is
// absent — a Cuttlefish container missing `/dev/vhost-vsock` cannot make
// the vsock transport and would fail opaquely.
//
// The order of `present` mirrors the input order so the produced
// `--device` flags are deterministic (no map iteration). devices is not
// mutated.
//
// Anti-bluff (Bluff-Audit — recorded in the implementing commit body):
//
//	Mutation: treat every device as present (skip the statDevice check).
//	Observed: TestFilterPresentDevices_PartialHost asserts that an absent
//	          node lands in `missing`; the mutated filter puts it in
//	          `present` and leaves `missing` empty, failing the test.
//	Reverted: yes.
func FilterPresentDevices(devices []string) (present, missing []string) {
	for _, d := range devices {
		if _, err := statDevice(d); err == nil {
			present = append(present, d)
		} else {
			missing = append(missing, d)
		}
	}
	return present, missing
}
