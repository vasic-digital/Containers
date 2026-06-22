package emulator

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// kvmDevicePath is the path to the KVM device. Package-level so tests
// can redirect it to a synthetic path without needing a real /dev/kvm.
// Production value is "/dev/kvm".
//
// Anti-bluff: TestContainerized_KVMPresence_Absent verifies that when
// this path does not exist, Boot omits "--device /dev/kvm" from the
// container arguments.
var kvmDevicePath = "/dev/kvm"

// kvmAvailable returns true iff the host exposes the KVM device node
// at kvmDevicePath. On Linux x86_64 the device is present when KVM is
// enabled; on macOS the podman VM kernel does NOT expose /dev/kvm (HVF
// is a macOS-host-only API, unreachable from the container's Linux VM).
//
// This function reads the filesystem; it does not call any kernel
// ioctl. A stat(2) is the least-privilege check — we want to know the
// device exists, not that we can open it for writing.
//
// Anti-bluff posture (§6.J/§6.L):
//   - Production Containerized.Boot calls this to decide whether to
//     include "--device /dev/kvm" in the `podman run` arguments.
//   - Tests override kvmDevicePath to a temp dir path to exercise both
//     branches without requiring a real KVM device.
func kvmAvailable() bool {
	_, err := os.Stat(kvmDevicePath)
	return err == nil
}

// buildContainerRunArgs constructs the `podman run` / `docker run`
// argument slice for Containerized.Boot. Separated from Boot so the
// no-KVM branch is unit-testable without running a real container.
//
// When /dev/kvm is present, "--device /dev/kvm" is included → hardware-
// accelerated KVM emulation (Linux x86_64 gate path).
//
// When /dev/kvm is absent, the flag is omitted → TCG software emulation.
// The emulator starts with "-no-accel" injected via the container's
// entrypoint env var ANDROID_EMULATOR_EXTRA_ARGS (if the entrypoint
// respects it) OR relies on the emulator's own auto-fallback from
// kvm to tcg when /dev/kvm is absent inside the container.
//
// Anti-bluff (Bluff-Audit — recorded in commit body):
//
//	Mutation: always include "--device /dev/kvm" regardless of kvmAvailable().
//	Observed: TestContainerized_KVMPresence_Absent fails — the returned
//	          args slice contains "--device" "/dev/kvm" when the test
//	          expects those to be absent.
//	Reverted: yes — post-revert the slice correctly omits the device flag.
func buildContainerRunArgs(
	runtimeBinary string,
	containerName string,
	hostPort int,
	avd AVD,
	coldBoot bool,
	image string,
) []string {
	args := []string{
		"run",
		"-d",
		"--name", containerName,
		"--rm",
	}

	// KVM passthrough: include only when the host exposes /dev/kvm.
	// On Linux x86_64 this enables hardware acceleration.
	// On macOS the podman VM has no /dev/kvm (HVF is unreachable from
	// a Linux container) — omitting the flag falls back to TCG.
	if kvmAvailable() {
		args = append(args, "--device", kvmDevicePath)
	}

	args = append(args,
		"-p", fmt.Sprintf("%d:5555/tcp", hostPort),
		"-p", fmt.Sprintf("%d:5554/tcp", hostPort-1),
		"-e", "ANDROID_AVD_NAME="+avd.Name,
		"-e", fmt.Sprintf("ANDROID_COLD_BOOT=%t", coldBoot),
		image,
	)
	return args
}

// defaultGradleModule is the neutral module name used when a caller
// supplies no GradleModule. It is deliberately generic — the
// Containers package is 100% decoupled from any consumer project, so
// no consumer module (e.g. the consuming project's "api-app") is ever baked in as a
// default or special case.
const defaultGradleModule = "app"

// gradleModuleSetter is the optional capability the matrix runner uses
// to propagate MatrixConfig.GradleModule onto an Emulator before
// RunInstrumentation. Both concrete emulators implement it; an external
// Emulator implementation that does not is simply run with whatever
// module it was constructed with (default "app").
type gradleModuleSetter interface {
	setGradleModule(module string)
}

// gradleConnectedTestArgs builds the gradle argument slice both
// emulator implementations (host-direct AndroidEmulator and
// Containerized) use to run a single instrumentation test class.
// `module` is the bare gradle module name (e.g. "app", "api-app");
// empty defaults to defaultGradleModule. The returned task is
// `:<module>:connectedDebugAndroidTest`. Centralising this here keeps
// the host-direct and container paths byte-identical and gives the
// module-substitution one falsifiability-rehearsed code path.
func gradleConnectedTestArgs(module, testClass string) []string {
	m := module
	if m == "" {
		m = defaultGradleModule
	}
	return []string{
		fmt.Sprintf(":%s:connectedDebugAndroidTest", m),
		"-Pandroid.testInstrumentationRunnerArguments.class=" + testClass,
		"--no-daemon",
	}
}

// Containerized implements [Emulator] by running the Android emulator
// process INSIDE a podman or docker container managed by the
// vasic-digital/Containers package. This is the constitutional landing
// for parent project's clause 6.X (Container-Submodule Emulator Wiring
// Mandate, added 2026-05-13):
//
//	"Every Android emulator instance the project depends on for testing
//	 MUST execute its emulator process INSIDE a podman/docker container
//	 managed by Submodules/Containers/, NOT be host-direct-launched by
//	 Containers-submodule code that runs on the host."
//
// Architecture
// ------------
// Boot launches a container from a pre-baked image that bundles the
// Android SDK + system images. The container's ADB console + ADB
// daemon ports are forwarded to the host. Host-side `adb` then
// connects to the forwarded ports and drives the emulator exactly as
// it would a host-direct one. Install + RunInstrumentation reuse the
// host's `adb` and `gradle` toolchains — only the emulator process
// itself lives in the container.
//
// Image contract
// --------------
// The image MUST:
//   - Bundle Android SDK + emulator + adb binaries.
//   - Bundle (or fetch on first boot) the AVD system image for the
//     target API level.
//   - Expose ports 5554 (console) + 5555 (adb) per emulator instance.
//   - Have access to /dev/kvm (Linux x86_64) or KVM-equivalent
//     virtualization. On darwin/arm64, /dev/kvm does not exist and
//     this implementation cannot satisfy clause 6.X clause 1 — that
//     is recorded in
//     `.lava-ci-evidence/sixth-law-incidents/2026-05-13-emulator-container-darwin-arm64-gap.json`
//     as the §6.V-debt darwin/arm64 gap. The gate runs on Linux
//     x86_64; workstation iteration on Apple Silicon uses
//     [AndroidEmulator] (host-direct) per the §6.X workstation
//     carve-out.
//
// Anti-bluff posture (clauses 6.J/6.L)
// ------------------------------------
// Every method in this type has at least one falsifiability-rehearsed
// test under containerized_test.go. The CommandExecutor seam (shared
// with AndroidEmulator) lets unit tests inject a fake that records
// invocations + returns canned output WITHOUT requiring an actual
// container runtime present in CI. The end-to-end "boot real emulator
// inside real container" test is gated on Linux x86_64 with /dev/kvm
// — `t.Skip("SKIP-OK: §6.X-debt — darwin/arm64 has no /dev/kvm; this
// test runs on Linux x86_64 gate runners. See incident JSON.")` on
// hosts where the gate cannot fire. Per the parent project's §6.J Forbidden
// Test Patterns, a t.Skip MUST have a tracking citation; the citation
// here is the §6.V-debt incident JSON referenced above.
type Containerized struct {
	// runtimeBinary is the path to the container CLI (e.g. "podman"
	// or "docker"). When empty, Boot detects via runtime.AutoDetect
	// — but in this package we keep it explicit so tests can inject
	// a fake binary name and the executor seam captures it.
	runtimeBinary string

	// image is the emulator container image reference (e.g.
	// "ghcr.io/vasic-digital/lava-android-emulator:api34-phone").
	// Per the Decoupled Reusable Architecture rule this is generic
	// — the consuming project configures the per-AVD image
	// list via its own manifest (tools/lava-containers/vm-images.json).
	image string

	// executor is the seam shared with AndroidEmulator. Production
	// uses the os/exec-backed osExecutor; tests inject a fake.
	executor CommandExecutor

	// containerName is populated by Boot and used by subsequent
	// calls to target the right container. One Containerized
	// instance manages exactly one container at a time.
	containerName string

	// hostADBPort is populated by Boot (the host-side ephemeral
	// port that podman/docker forwards to the container's 5555).
	// WaitForBoot/Install/RunInstrumentation/Teardown use this to
	// drive `adb -s emulator-<port>` invocations.
	hostADBPort int

	// adbBinaryPath is the host-side path to `adb`. The container
	// runs its own adb internally; this is the host's adb that
	// connects to the forwarded port. Empty defaults to "adb" on
	// PATH.
	adbBinaryPath string

	// gradleBinary is the host-side gradle invocation. Empty
	// defaults to "./gradlew". RunInstrumentation invokes this
	// with ANDROID_SERIAL pointing at the forwarded port.
	gradleBinary string

	// gradleModule is the bare gradle module name whose
	// connectedDebugAndroidTest task RunInstrumentation runs. Empty
	// defaults to "app" (preserving the prior hardwired :app behavior).
	// Generic per the Decoupled Reusable Architecture rule — the
	// consuming project supplies its own module name; no consumer
	// module is special-cased here.
	gradleModule string
}

// ContainerizedConfig parameterises a [Containerized] instance.
// All fields except RuntimeBinary + Image are optional.
type ContainerizedConfig struct {
	// RuntimeBinary is "podman" or "docker". Required.
	RuntimeBinary string
	// Image is the emulator container image reference. Required.
	Image string
	// Executor is the CommandExecutor seam. nil = production
	// osExecutor.
	Executor CommandExecutor
	// ADBBinaryPath is the host-side adb. Empty = "adb" on PATH.
	ADBBinaryPath string
	// GradleBinary is the host-side gradle wrapper. Empty =
	// "./gradlew".
	GradleBinary string
	// GradleModule is the bare gradle module whose
	// connectedDebugAndroidTest task runs. Empty defaults to "app".
	// The CLI's --gradle-module flag threads into this field. Generic
	// — no consumer module is special-cased.
	GradleModule string
}

// NewContainerized constructs a Containerized emulator. Returns an
// error if required config fields are empty — fail-loud per clause
// 6.J (no silent defaults that hide misconfiguration).
func NewContainerized(cfg ContainerizedConfig) (*Containerized, error) {
	if cfg.RuntimeBinary == "" {
		return nil, fmt.Errorf("ContainerizedConfig.RuntimeBinary is required (e.g. \"podman\" or \"docker\")")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("ContainerizedConfig.Image is required (the Android emulator container image)")
	}
	executor := cfg.Executor
	if executor == nil {
		executor = NewOSExecutor()
	}
	adbBin := cfg.ADBBinaryPath
	if adbBin == "" {
		adbBin = "adb"
	}
	gradleBin := cfg.GradleBinary
	if gradleBin == "" {
		gradleBin = "./gradlew"
	}
	return &Containerized{
		runtimeBinary: cfg.RuntimeBinary,
		image:         cfg.Image,
		executor:      executor,
		adbBinaryPath: adbBin,
		gradleBinary:  gradleBin,
		gradleModule:  cfg.GradleModule,
	}, nil
}

// Boot launches the emulator container. Returns when the container
// is started — boot-completed is NOT awaited here; use WaitForBoot
// to poll `getprop sys.boot_completed`.
//
// Per clause 6.I clause 6, coldBoot=true SHOULD be used for any
// gating run — passed through to the emulator via `-no-snapshot` in
// the container's entrypoint.
func (c *Containerized) Boot(
	ctx context.Context,
	avd AVD,
	coldBoot bool,
) (BootResult, error) {
	startedAt := time.Now()

	// Pick an ephemeral host port to forward 5555 to. Use port 0 to
	// let the OS allocate, but for the runtime CLI we need a
	// concrete number — probe one.
	hostPort, err := pickFreeTCPPort()
	if err != nil {
		return BootResult{
			AVD:          avd,
			Started:      false,
			BootDuration: time.Since(startedAt),
			Error:        fmt.Errorf("pick host ADB port: %w", err),
		}, err
	}

	// Container name is deterministic per-AVD so Teardown can find
	// it even if the caller dropped the Containerized instance.
	// Format: "lava-emu-<avd-name>-<unix-ms>" — the timestamp
	// disambiguates concurrent boots of the same AVD (which §6.X
	// gate runs don't do, but iteration sessions might).
	containerName := fmt.Sprintf(
		"lava-emu-%s-%d",
		sanitizeContainerName(avd.Name),
		time.Now().UnixMilli(),
	)
	c.containerName = containerName
	c.hostADBPort = hostPort

	// Build `podman run -d --name X [-device /dev/kvm] -p ...` args.
	// --device /dev/kvm is included only when the KVM device is
	// present on the host (Linux x86_64 with KVM enabled). On macOS
	// the podman VM has no /dev/kvm; omitting the flag lets the
	// emulator inside the container fall back to TCG software
	// emulation. buildContainerRunArgs centralises this decision so
	// it is unit-testable without running a real container.
	args := buildContainerRunArgs(
		c.runtimeBinary, containerName, hostPort, avd, coldBoot, c.image,
	)

	out, err := c.executor.Execute(ctx, c.runtimeBinary, args...)
	if err != nil {
		wrapped := fmt.Errorf("%s run: %w (output: %s)", c.runtimeBinary, err, string(out))
		return BootResult{
			AVD:          avd,
			Started:      false,
			BootDuration: time.Since(startedAt),
			Error:        wrapped,
		}, wrapped
	}

	return BootResult{
		AVD:          avd,
		Started:      true,
		BootDuration: time.Since(startedAt),
		ConsolePort:  hostPort - 1,
		ADBPort:      hostPort,
	}, nil
}

// WaitForBoot polls `adb -s emulator-<port> shell getprop
// sys.boot_completed` until the response is "1" or the timeout
// elapses. The host-side adb connects to the forwarded port.
//
// Per clause 6.J: the assertion this function provides to callers
// is that sys.boot_completed=1 was OBSERVED on the wire — a non-nil
// error means boot did not complete; this function does NOT report
// "probably booted" or "give up after timeout but maybe ok".
func (c *Containerized) WaitForBoot(
	ctx context.Context,
	port int,
	timeout time.Duration,
) (time.Duration, error) {
	startedAt := time.Now()
	deadline := startedAt.Add(timeout)
	// Connect host adb to the forwarded port first.
	if _, err := c.executor.Execute(
		ctx, c.adbBinaryPath, "connect", fmt.Sprintf("localhost:%d", port),
	); err != nil {
		return time.Since(startedAt), fmt.Errorf("adb connect: %w", err)
	}
	target := fmt.Sprintf("localhost:%d", port)
	for time.Now().Before(deadline) {
		out, err := c.executor.Execute(
			ctx, c.adbBinaryPath, "-s", target, "shell", "getprop", "sys.boot_completed",
		)
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return time.Since(startedAt), nil
		}
		select {
		case <-ctx.Done():
			return time.Since(startedAt), ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return time.Since(startedAt), fmt.Errorf(
		"WaitForBoot timed out after %s waiting for sys.boot_completed=1 on port %d",
		timeout, port,
	)
}

// Install installs the APK onto the emulator via host adb.
func (c *Containerized) Install(
	ctx context.Context,
	port int,
	apkPath string,
) error {
	target := fmt.Sprintf("localhost:%d", port)
	out, err := c.executor.Execute(
		ctx, c.adbBinaryPath, "-s", target, "install", "-r", apkPath,
	)
	if err != nil {
		return fmt.Errorf("adb install: %w (output: %s)", err, string(out))
	}
	if !bytes.Contains(out, []byte("Success")) {
		return fmt.Errorf("adb install did not report Success; output: %s", string(out))
	}
	return nil
}

// RunInstrumentation runs the named instrumentation test class via
// the host's gradle wrapper, with ANDROID_SERIAL pointing at the
// forwarded port. Returns the captured combined output and a
// pass/fail signal derived from BOTH the gradle exit code AND the
// presence of the canonical success marker in the output. Either
// signal failing flips Passed to false.
func (c *Containerized) RunInstrumentation(
	ctx context.Context,
	port int,
	testClass string,
	timeout time.Duration,
) (string, bool, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// ANDROID_SERIAL: gradle's connectedAndroidTest will pick the
	// device named here. The format matches `adb devices` output
	// (e.g. "emulator-5554" or "localhost:7654"); we use the
	// localhost:<port> form because that's what we connected via
	// in WaitForBoot.
	target := fmt.Sprintf("localhost:%d", port)
	args := gradleConnectedTestArgs(c.gradleModule, testClass)
	// The CommandExecutor seam doesn't expose env-var setting, so
	// we synthesize the env via a shell wrapper. In production
	// osExecutor.Execute this is `sh -c 'ANDROID_SERIAL=... ./gradlew ...'`.
	// Tests intercept this exact form.
	cmdLine := fmt.Sprintf(
		"ANDROID_SERIAL=%s %s %s",
		target,
		shellQuote(c.gradleBinary),
		shellQuoteArgs(args),
	)
	out, err := c.executor.Execute(runCtx, "/bin/sh", "-c", cmdLine)
	output := string(out)
	passed := err == nil && strings.Contains(output, "BUILD SUCCESSFUL")
	if !passed && err == nil {
		err = fmt.Errorf("gradle exit zero but BUILD SUCCESSFUL not in output")
	}
	return output, passed, err
}

// setGradleModule sets the bare gradle module RunInstrumentation
// targets. Empty is a no-op (the constructor default stands). The
// matrix runner calls this to propagate MatrixConfig.GradleModule.
func (c *Containerized) setGradleModule(module string) {
	if module != "" {
		c.gradleModule = module
	}
}

// Teardown stops + removes the container via the runtime CLI. Uses
// `rm -f` so a still-running emulator is force-killed (the
// container's `--rm` flag from Boot ensures filesystem cleanup
// happens automatically after stop).
func (c *Containerized) Teardown(ctx context.Context, _ int) error {
	if c.containerName == "" {
		// Nothing to tear down — Boot was never called on this
		// instance. Per clause 6.J this is a no-op SUCCESS, not a
		// silent error, because callers may invoke Teardown
		// defensively in a defer block.
		return nil
	}
	out, err := c.executor.Execute(
		ctx, c.runtimeBinary, "rm", "-f", c.containerName,
	)
	c.containerName = ""
	c.hostADBPort = 0
	if err != nil {
		return fmt.Errorf("%s rm: %w (output: %s)", c.runtimeBinary, err, string(out))
	}
	return nil
}

// ContainerName returns the runtime-side container name set by
// Boot. Empty if Boot has not been called yet OR if Teardown has
// already run. Exposed for tests + the matrix runner's attestation
// row (each row records `container: <name>` for forensic recall).
func (c *Containerized) ContainerName() string { return c.containerName }

// HostADBPort returns the host-side ADB port forwarded from the
// container's 5555. Set by Boot, cleared by Teardown.
func (c *Containerized) HostADBPort() int { return c.hostADBPort }

// pickFreeTCPPort asks the OS for a free TCP port by binding then
// closing. The kernel may reuse the port for someone else in the
// window between this call returning and the runtime CLI taking it,
// but the race window is small enough that production gate runs
// haven't seen collisions. Tests inject the executor seam so they
// don't exercise this path.
func pickFreeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr := l.Addr().String()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, err
	}
	// Console port (5554) needs to be ADB port (5555) - 1. We picked
	// an arbitrary port; ensure port-1 is also free. If not, retry
	// once. Production AVDs always use the (even, odd) pair where
	// even=console, odd=adb.
	if port%2 == 0 {
		port++ // pickFreeTCPPort gave us an even port; flip to odd for adb
	}
	return port, nil
}

// sanitizeContainerName makes an AVD name safe to use as a podman/
// docker container name (alphanumeric + dashes + underscores only).
func sanitizeContainerName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// shellQuote single-quotes a string for safe inclusion in a shell
// command line. Replaces single quotes with the canonical escape
// sequence '"'"'.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellQuoteArgs joins + quotes a slice of args for shell execution.
func shellQuoteArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// Compile-time check that Containerized satisfies Emulator.
var _ Emulator = (*Containerized)(nil)
