// Package cuttlefish provides a thin, project-agnostic lifecycle wrapper
// for Google's Cuttlefish (`cvd`) virtual-Android device, orchestrated as
// a rootful + privileged container per the §11.4.161 documented exception.
//
// Why this package exists
// -----------------------
// pkg/emulator orchestrates the Android SDK emulator (QEMU-backed) — high
// throughput, but it does NOT exercise the real Android A/B `update_engine`
// + AVB/dm-verity + auto-rollback apply path. Cuttlefish boots a real
// Android system image (built with A/B / Virtual A/B) and applies a real
// OTA `payload.bin` through the real `update_engine` daemon, so the slot
// switch, AVB verification, and auto-rollback genuinely happen. This
// package is the boot/health/teardown plumbing for that target; the
// OTA-domain logic stays in the consuming project (per §11.4.28 — no
// consumer-specific context leaks into this submodule).
//
// Honest boundary (§11.4.6 / §11.4.112)
// -------------------------------------
// Cuttlefish is host-gated, NOT structurally impossible. It requires a
// Linux host with KVM (`/dev/kvm`) and a privileged container with the
// vhost/vsock/tun device nodes; the official image mandates
// `--privileged` + `--network host` and a rootless runtime cannot create
// the `cvd-ebr` bridge — so this is the documented §11.4.161 rootless
// exception. On a host without `/dev/kvm` (e.g. macOS / Apple Silicon)
// the lifecycle cannot launch; callers SKIP-with-reason per §11.4.3 via
// the RequireKVM topology gate, never PASS-by-default.
//
// Anti-bluff posture (§11.4 / §6.J / §6.L)
// ----------------------------------------
// Every command-constructing function is pure and unit-tested against its
// produced argument slice (the user-visible behaviour: "the container is
// launched privileged with exactly the device nodes the host can provide,
// and falls back to an honest topology SKIP where it cannot"). The
// CommandExecutor seam lets the lifecycle be exercised with a fake that
// records invocations + returns canned output WITHOUT a real privileged
// container — the genuine `cvd` boot + OTA-apply path is an
// integration-pending layer that requires a Linux + nested-KVM host.
package cuttlefish

import (
	"context"
	"os/exec"
	"time"
)

// Sourced canonical defaults (FACT, from docs/design/CUTTLEFISH_TIER2.md
// §3 + §4 + RESEARCH FACTS). None of these is consumer-specific: the
// device-node list, the group set, and the AOSP target name are all
// upstream Cuttlefish/AOSP values, overridable per Config field.
const (
	// DefaultCuttlefishTarget is the upstream AOSP build target for an
	// x86_64 Cuttlefish phone image published on ci.android.com (no
	// credentials required). Consumers override via Config.Target.
	DefaultCuttlefishTarget = "aosp_cf_x86_64_only_phone-userdebug"

	// DefaultADBSerial is the canonical adb serial of the first
	// Cuttlefish instance over its vsock transport. Consumers override
	// via Config.ADBSerial when running a non-default instance number.
	DefaultADBSerial = "0.0.0.0:6520"

	// defaultStopGracePeriod is how long Stop waits after `stop_cvd`
	// before reaping orphan crosvm/run_cvd processes.
	defaultStopGracePeriod = 30 * time.Second
)

// DefaultDevices is the closed set of host device nodes a Cuttlefish
// container requires (FACT, RESEARCH). FilterPresentDevices gates each by
// host presence so the container is launched with exactly what the host
// can provide; any missing node is surfaced for an honest §11.4.3 SKIP.
func DefaultDevices() []string {
	return []string{
		"/dev/kvm",
		"/dev/vhost-vsock",
		"/dev/vhost-net",
		"/dev/vsock",
		"/dev/net/tun",
	}
}

// DefaultGroups is the supplementary-group set the cvd user requires on
// the host (FACT, RESEARCH): kvm (KVM access), cvdnetwork (the cvd-ebr
// bridge), render (GPU render node). Passed via `--group-add`.
func DefaultGroups() []string {
	return []string{"kvm", "cvdnetwork", "render"}
}

// CommandExecutor is the seam through which the lifecycle runs host
// commands (the container runtime CLI + host adb). The production impl
// shells out via os/exec; tests substitute a fake that records
// invocations and returns canned output.
//
// Execute is for short-lived synchronous commands (adb devices,
// `podman exec stop_cvd`, `podman rm`). Start is for detached launches
// (`podman run -d` returns immediately, but Start is provided for
// symmetry with pkg/emulator's seam so an alternate non-detached runner
// can be wired without changing the interface).
//
// Anti-bluff (§6.J): the seam exists ONLY for testing — production code
// uses the real os/exec impl. A test asserting "the runtime CLI was
// invoked with these privileged-container args" asserts on observable
// host-shell behaviour, not internal state.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
	Start(ctx context.Context, name string, args ...string) error
}

type osExecutor struct{}

func (osExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (osExecutor) Start(_ context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// NewOSExecutor returns the real os/exec-based executor used by
// production code.
func NewOSExecutor() CommandExecutor { return osExecutor{} }

// Config parameterises a Cuttlefish lifecycle instance. Every value that
// could be consumer-specific (image, target, instance dir, evidence dir)
// is a field — nothing is baked into the package (§11.4.28).
type Config struct {
	// RuntimeBinary is the container CLI ("podman" or "docker").
	// Required. Cuttlefish requires the ROOTFUL + privileged variant per
	// the §11.4.161 documented exception (rootless cannot create the
	// cvd-ebr bridge).
	RuntimeBinary string

	// Image is the Cuttlefish container image reference (built from
	// Containerfile in this package, or a published image). Required.
	Image string

	// Target is the AOSP build target the image carries (e.g.
	// DefaultCuttlefishTarget). Recorded into LaunchResult for evidence;
	// it does not change the run args. Empty defaults to
	// DefaultCuttlefishTarget.
	Target string

	// InstanceName is a stable, human-readable container name suffix. The
	// produced container name is "cvd-<sanitised-InstanceName>". Empty
	// defaults to "default". Resolved by stable NAME, never an
	// enumeration index, per §11.4.111.
	InstanceName string

	// Devices is the host device-node list to pass via `--device`. Empty
	// defaults to DefaultDevices(). FilterPresentDevices gates each by
	// host presence at Launch time.
	Devices []string

	// Groups is the supplementary-group set to pass via `--group-add`.
	// Empty defaults to DefaultGroups().
	Groups []string

	// NetworkHost requests `--network host`. Cuttlefish requires it
	// (FACT, RESEARCH); default true. Set explicitly to false only for a
	// test that asserts the negative.
	NetworkHost bool

	// Privileged requests `--privileged`. Cuttlefish REQUIRES it (the
	// §11.4.161 documented exception); default true. NewCuttlefish
	// rejects a Config with Privileged explicitly disabled AND no
	// override acknowledgement, because a non-privileged Cuttlefish
	// container cannot make the cvd-ebr bridge and will fail to boot.
	Privileged bool

	// ADBSerial is the adb serial the readiness probe waits for. Empty
	// defaults to DefaultADBSerial.
	ADBSerial string

	// ADBBinaryPath is the host-side adb. Empty defaults to "adb" on PATH.
	ADBBinaryPath string

	// EvidenceDir is where the caller's harness writes captured evidence
	// (Tier-2 OTA apply/rollback proofs per §11.4.69). Recorded into
	// results; this package does not write into it (the consumer owns the
	// OTA evidence flow).
	EvidenceDir string

	// Executor is the CommandExecutor seam. nil = production osExecutor.
	Executor CommandExecutor

	// StopGracePeriod is how long Stop waits after `stop_cvd` before
	// reaping orphan crosvm/run_cvd processes. Zero defaults to
	// defaultStopGracePeriod.
	StopGracePeriod time.Duration
}

// LaunchResult captures the outcome of a single Launch attempt.
type LaunchResult struct {
	// ContainerName is the runtime-side container name the lifecycle
	// created. Empty when Started is false.
	ContainerName string
	// Target echoes the resolved Config.Target for the evidence row.
	Target string
	// Started is true once the container-run command returned 0. It does
	// NOT imply Android booted — use Status (adb-device-online) for that,
	// exactly as pkg/emulator separates Boot from WaitForBoot.
	Started bool
	// PresentDevices is the subset of configured devices the host
	// actually exposes — the set passed to `--device`.
	PresentDevices []string
	// MissingDevices is the subset NOT present on the host. Non-empty
	// means the caller should SKIP-with-reason per §11.4.3, never PASS.
	MissingDevices []string
	// Duration is the wall-clock the run command took.
	Duration time.Duration
	// Error is the launch error, if any.
	Error error
}

// ADBDevice is one parsed line of `adb devices` output.
type ADBDevice struct {
	// Serial is the device serial (e.g. "0.0.0.0:6520").
	Serial string
	// State is the adb connection state ("device", "offline",
	// "unauthorized", "bootloader", ...). Only "device" is online/ready.
	State string
}

// Online reports whether the device is in the ready "device" state.
func (d ADBDevice) Online() bool { return d.State == "device" }

// StatusResult captures one readiness probe (`adb devices`).
type StatusResult struct {
	// Devices is every parsed entry from `adb devices`.
	Devices []ADBDevice
	// Ready is true iff the configured ADBSerial is present AND online
	// (or, when ADBSerial is empty, iff ANY device is online).
	Ready bool
	// Raw is the trimmed `adb devices` output, retained as captured
	// evidence for the readiness assertion (§11.4.69).
	Raw string
}

// CleanupReport summarises an orphan-reaping pass. Mirrors
// pkg/emulator.CleanupReport's shape for cross-package familiarity.
type CleanupReport struct {
	// Found are PIDs whose comm matched the Cuttlefish process allowlist.
	Found []int
	// TerminatedTERM are PIDs that exited within the SIGTERM grace window.
	TerminatedTERM []int
	// KilledKILL are PIDs that required SIGKILL.
	KilledKILL []int
	// Surviving are PIDs still alive after SIGKILL (rare; permission).
	Surviving []int
	// SkippedReadErr are PIDs whose comm could not be read.
	SkippedReadErr []int
}

// Lifecycle is the contract this package's Cuttlefish type satisfies. It
// mirrors the boot/health/teardown surface the rest of the submodule
// exposes (pkg/emulator.Emulator, pkg/health). Methods are safe to call
// sequentially (Launch → Status → Stop); concurrent calls against the
// same instance are NOT supported.
type Lifecycle interface {
	// Launch starts the privileged Cuttlefish container. Returns once the
	// run command completes — Android boot completion is observed via
	// Status, never assumed here.
	Launch(ctx context.Context) (LaunchResult, error)

	// Status runs `adb devices` and reports readiness.
	Status(ctx context.Context) (StatusResult, error)

	// Stop runs `stop_cvd` in the container, then force-removes the container
	// (`rm -f`) as the authoritative teardown — tearing down the container's PID
	// namespace and killing every cvd process inside. Per §11.4.174 Stop performs
	// NO host-wide cvd reap.
	Stop(ctx context.Context) error
}
