package emulator

import "fmt"

// accel.go — per-OS hardware-acceleration model for the Android
// emulator runner.
//
// Why this file exists
// --------------------
// containerized.go's Boot() hardcodes `--device /dev/kvm` in the
// `podman run` arguments. `/dev/kvm` is the Linux KVM (Kernel-based
// Virtual Machine) device interface. It is a fact, not a guess, that:
//
//   - Linux x86_64 hosts expose `/dev/kvm`; a Linux container can be
//     granted access to it via `--device /dev/kvm`, so the
//     Containerized runner is the OS-correct accelerated path on
//     Linux.
//   - macOS hosts have no `/dev/kvm`. macOS hardware acceleration is
//     Apple HVF (Hypervisor.framework), a macOS-host-only API. A
//     Linux container running under podman/docker on macOS executes
//     inside a Linux VM and cannot reach the host's HVF interface.
//     The Android emulator uses HVF automatically when launched as a
//     native macOS process — which is exactly what AndroidEmulator
//     (the host-direct runner in android.go) does. Therefore on macOS
//     the host-direct runner is the only accelerated path AND the
//     gate-eligible runner.
//   - Windows hosts have no `/dev/kvm`. Windows hardware acceleration
//     is WHPX (Windows Hypervisor Platform), a Windows-host-only API,
//     unreachable from a Linux container for the same reason as HVF.
//     The host-direct runner is the OS-correct path on Windows.
//
// This file makes that OS→accel→runner mapping an explicit, pure,
// deterministic function so callers (cmd/emulator-matrix, the consuming project's
// run-challenge-matrix.sh) can resolve the correct runner instead of
// hardcoding one. It does NOT change containerized.go: Linux still
// uses the existing `--device /dev/kvm` path verbatim.

// AccelBackend identifies a host's hardware-virtualization backend
// for the Android emulator. The Android emulator selects the backend
// from the host OS automatically; this type makes the host's backend
// explicit so the runner choice can be derived from it.
type AccelBackend string

const (
	// AccelKVM is the Linux Kernel-based Virtual Machine backend,
	// exposed to user space as the `/dev/kvm` device. A Linux
	// container can be granted access via `--device /dev/kvm`.
	AccelKVM AccelBackend = "kvm"

	// AccelHVF is the Apple Hypervisor.framework backend used on
	// macOS. HVF is a macOS-host-only API; it is not exposed inside a
	// Linux container running under podman/docker on macOS.
	AccelHVF AccelBackend = "hvf"

	// AccelWHPX is the Windows Hypervisor Platform backend used on
	// Windows. WHPX is a Windows-host-only API; it is not exposed
	// inside a Linux container running under podman/docker on Windows.
	AccelWHPX AccelBackend = "whpx"

	// AccelNone marks a host whose acceleration backend is not known
	// to this package (any GOOS other than linux/darwin/windows).
	// The emulator may still run in software mode, but no accelerated
	// container path can be guaranteed.
	AccelNone AccelBackend = "none"
)

// RunnerKind identifies which emulator runner implementation is the
// OS-correct choice for a given host.
type RunnerKind string

const (
	// RunnerContainerized selects the Containerized runner
	// (containerized.go): the emulator process runs inside a
	// podman/docker container with `--device /dev/kvm`. This is the
	// OS-correct runner only on Linux, where `/dev/kvm` exists.
	RunnerContainerized RunnerKind = "containerized"

	// RunnerHostDirect selects the AndroidEmulator runner
	// (android.go): the emulator runs as a native host process. On
	// macOS and Windows this is the OS-correct runner because the
	// host-only acceleration backend (HVF / WHPX) is reachable only
	// by a native host process, not by a Linux container.
	RunnerHostDirect RunnerKind = "host-direct"
)

// OSAccelProfile records, for one host OS, which acceleration backend
// the Android emulator uses and which runner is therefore the
// OS-correct choice. The Rationale states the reason in plain terms
// so callers and reviewers can verify the mapping without re-deriving
// it.
type OSAccelProfile struct {
	// GOOS is the Go runtime.GOOS value this profile describes.
	GOOS string
	// Accel is the hardware-virtualization backend the Android
	// emulator uses on this OS.
	Accel AccelBackend
	// Runner is the OS-correct emulator runner for this OS.
	Runner RunnerKind
	// Rationale states, as fact, why this OS maps to this Accel and
	// Runner.
	Rationale string
}

// AccelProfileForOS returns the OSAccelProfile for the given
// runtime.GOOS value. The function is pure and deterministic: the
// same goos always produces the same profile, and it consults no
// host state.
//
// Mapping (each entry is a fact about the named OS):
//
//   - "linux"   → Accel KVM,  Runner containerized. Linux exposes
//     `/dev/kvm`; a container can be granted access via
//     `--device /dev/kvm`, so the emulator runs accelerated INSIDE
//     the container.
//   - "darwin"  → Accel HVF,  Runner host-direct. macOS has no
//     `/dev/kvm`; its HVF backend is a macOS-host-only API a Linux
//     container cannot reach. A native macOS emulator process uses
//     HVF automatically, so host-direct is the only accelerated path.
//   - "windows" → Accel WHPX, Runner host-direct. Windows has no
//     `/dev/kvm`; its WHPX backend is a Windows-host-only API a Linux
//     container cannot reach. A native Windows emulator process uses
//     WHPX automatically, so host-direct is the only accelerated path.
//   - any other GOOS → Accel none, Runner host-direct. This package
//     does not know an accelerated container path for other OSes;
//     host-direct is the conservative default.
func AccelProfileForOS(goos string) OSAccelProfile {
	switch goos {
	case "linux":
		return OSAccelProfile{
			GOOS:   "linux",
			Accel:  AccelKVM,
			Runner: RunnerContainerized,
			Rationale: "Linux exposes /dev/kvm; a podman/docker container can be " +
				"granted access via --device /dev/kvm, so the Android emulator runs " +
				"hardware-accelerated INSIDE the container (containerized runner).",
		}
	case "darwin":
		return OSAccelProfile{
			GOOS:   "darwin",
			Accel:  AccelHVF,
			Runner: RunnerHostDirect,
			Rationale: "macOS has no /dev/kvm; its acceleration backend is Apple HVF " +
				"(Hypervisor.framework), a macOS-host-only API unreachable from a Linux " +
				"container. A native macOS emulator process uses HVF automatically, so " +
				"host-direct is the only accelerated and therefore gate-eligible runner.",
		}
	case "windows":
		return OSAccelProfile{
			GOOS:   "windows",
			Accel:  AccelWHPX,
			Runner: RunnerHostDirect,
			Rationale: "Windows has no /dev/kvm; its acceleration backend is WHPX " +
				"(Windows Hypervisor Platform), a Windows-host-only API unreachable from " +
				"a Linux container. A native Windows emulator process uses WHPX " +
				"automatically, so host-direct is the OS-correct runner.",
		}
	default:
		return OSAccelProfile{
			GOOS:   goos,
			Accel:  AccelNone,
			Runner: RunnerHostDirect,
			Rationale: "This package knows no accelerated container path for GOOS " +
				goos + "; host-direct is the conservative default (the emulator may " +
				"still run in software mode).",
		}
	}
}

// ResolveRunner converts a requested runner choice into a concrete
// RunnerKind. It supports three forms of `requested`:
//
//   - "auto"          → resolves to AccelProfileForOS(goos).Runner,
//     i.e. the OS-correct runner for the host (containerized on
//     Linux, host-direct on macOS/Windows/other).
//   - "containerized" → returns RunnerContainerized verbatim. The
//     caller has explicitly chosen the container path; this function
//     does not second-guess it (a Linux x86_64 gate-host is the
//     correct place for that choice).
//   - "host-direct"   → returns RunnerHostDirect verbatim.
//
// Any other value is a configuration error and is returned as a
// non-nil error so the caller fails loudly rather than silently
// defaulting.
func ResolveRunner(requested, goos string) (RunnerKind, error) {
	switch requested {
	case "auto":
		return AccelProfileForOS(goos).Runner, nil
	case string(RunnerContainerized):
		return RunnerContainerized, nil
	case string(RunnerHostDirect):
		return RunnerHostDirect, nil
	default:
		return "", fmt.Errorf(
			"invalid runner %q: must be one of auto, %s, %s",
			requested, RunnerContainerized, RunnerHostDirect,
		)
	}
}

// GateEligibleForOS reports whether the given runner is the OS-correct,
// hardware-accelerated, gate-eligible runner for the given runtime.GOOS.
//
// It is true exactly when runner == AccelProfileForOS(goos).Runner:
//
//   - host-direct on darwin  → true  (HVF is reachable only by a native
//     macOS process; host-direct IS the accelerated gate runner there).
//   - host-direct on windows → true  (WHPX is reachable only by a native
//     Windows process; host-direct IS the accelerated gate runner there).
//   - containerized on linux → true  (`/dev/kvm` is grantable to a Linux
//     container; containerized IS the accelerated gate runner there).
//   - host-direct on linux   → false (skips KVM-in-container; the
//     OS-correct runner on Linux is containerized — host-direct there is
//     a workstation-iteration choice, not a gate run).
//   - containerized on darwin/windows → false (the container cannot
//     reach the host-only HVF/WHPX accelerator).
//
// The function is pure and deterministic: it consults no host state and
// derives its answer solely from AccelProfileForOS.
func GateEligibleForOS(runner RunnerKind, goos string) bool {
	return runner == AccelProfileForOS(goos).Runner
}
