package cuttlefish

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cuttlefish.go — the Cuttlefish lifecycle: Launch / Status / Stop over a
// rootful + privileged container whose entrypoint runs `launch_cvd`, with
// readiness via `adb devices` and teardown via `stop_cvd` + orphan
// reaping.
//
// The command-constructing functions (buildContainerRunArgs,
// buildLaunchCvdArgs, buildStopCvdArgs) are pure: they take explicit
// inputs and return an argument slice, so the user-visible launch contract
// is unit-testable without a real privileged container. The lifecycle
// methods run those args through the CommandExecutor seam.

// Cuttlefish implements [Lifecycle] by running Google's Cuttlefish virtual
// Android device inside a rootful + privileged container. Per §11.4.161
// (rootless-container mandate) this is the DOCUMENTED rootless exception:
// the official Cuttlefish image mandates `--privileged`, the
// vhost/vsock/tun device nodes, and `--network host`; a rootless runtime
// cannot create the `cvd-ebr` bridge, so Cuttlefish is the platform-has-no-
// rootless-option case the §11.4.161 escape clause covers.
//
// One Cuttlefish instance manages exactly one container at a time.
type Cuttlefish struct {
	cfg           Config
	executor      CommandExecutor
	containerName string
}

// NewCuttlefish constructs a Cuttlefish lifecycle from cfg, applying the
// documented defaults and rejecting a config that cannot launch.
// Fail-loud per §6.J — no silent default that hides a misconfiguration.
func NewCuttlefish(cfg Config) (*Cuttlefish, error) {
	if cfg.RuntimeBinary == "" {
		return nil, fmt.Errorf("cuttlefish: Config.RuntimeBinary is required (e.g. \"podman\" or \"docker\")")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("cuttlefish: Config.Image is required (the Cuttlefish container image)")
	}
	// Privileged is mandatory for Cuttlefish (the §11.4.161 documented
	// exception). A caller that explicitly sets Privileged=false has asked
	// for a container that physically cannot make the cvd-ebr bridge —
	// reject it loudly rather than launch a guaranteed-to-fail container.
	if !cfg.Privileged {
		return nil, fmt.Errorf(
			"cuttlefish: Config.Privileged must be true — Cuttlefish requires a rootful + privileged " +
				"container (the §11.4.161 documented rootless exception); a non-privileged container " +
				"cannot create the cvd-ebr bridge and will fail to boot")
	}
	if cfg.Target == "" {
		cfg.Target = DefaultCuttlefishTarget
	}
	if cfg.InstanceName == "" {
		cfg.InstanceName = "default"
	}
	if len(cfg.Devices) == 0 {
		cfg.Devices = DefaultDevices()
	}
	if len(cfg.Groups) == 0 {
		cfg.Groups = DefaultGroups()
	}
	if cfg.ADBSerial == "" {
		cfg.ADBSerial = DefaultADBSerial
	}
	if cfg.ADBBinaryPath == "" {
		cfg.ADBBinaryPath = "adb"
	}
	if cfg.StopGracePeriod == 0 {
		cfg.StopGracePeriod = defaultStopGracePeriod
	}
	executor := cfg.Executor
	if executor == nil {
		executor = NewOSExecutor()
	}
	return &Cuttlefish{cfg: cfg, executor: executor}, nil
}

// containerNameFor produces the deterministic, stable container name for
// an instance (resolved by NAME, never an enumeration index, per
// §11.4.111).
func containerNameFor(instanceName string) string {
	return "cvd-" + sanitizeContainerName(instanceName)
}

// Launch starts the privileged Cuttlefish container. It first partitions
// the configured device nodes by host presence: only present nodes are
// passed to `--device`, and any missing node is surfaced in the result so
// the caller SKIPs-with-reason per §11.4.3 (a Cuttlefish container missing
// a required vhost/vsock node cannot boot). Launch returns once the
// container-run command completes — Android boot completion is observed
// via Status, never assumed here (mirrors pkg/emulator Boot vs WaitForBoot).
func (c *Cuttlefish) Launch(ctx context.Context) (LaunchResult, error) {
	startedAt := time.Now()
	present, missing := FilterPresentDevices(c.cfg.Devices)
	name := containerNameFor(c.cfg.InstanceName)

	args := buildContainerRunArgs(name, c.cfg.Image, present, c.cfg.Groups,
		c.cfg.Privileged, c.cfg.NetworkHost)

	out, err := c.executor.Execute(ctx, c.cfg.RuntimeBinary, args...)
	if err != nil {
		// CF-2 (§11.4.174 ownership hazard): do NOT commit c.containerName
		// on a failed run — no container was created. `name` is still
		// returned in LaunchResult.ContainerName as the diagnostic name
		// (unchanged public contract), but c.ContainerName() MUST report
		// empty so a caller's natural `res, _ := c.Launch(ctx); defer
		// c.Stop(ctx)` does not issue `<runtime> rm -f <name>` against a
		// nonexistent container — or, since containerNameFor is a pure
		// function of InstanceName, against a DIFFERENT concurrent
		// instance's real container.
		wrapped := fmt.Errorf("%s run: %w (output: %s)", c.cfg.RuntimeBinary, err, string(out))
		return LaunchResult{
			ContainerName:  name,
			Target:         c.cfg.Target,
			Started:        false,
			PresentDevices: present,
			MissingDevices: missing,
			Duration:       time.Since(startedAt),
			Error:          wrapped,
		}, wrapped
	}
	c.containerName = name
	return LaunchResult{
		ContainerName:  name,
		Target:         c.cfg.Target,
		Started:        true,
		PresentDevices: present,
		MissingDevices: missing,
		Duration:       time.Since(startedAt),
	}, nil
}

// Status runs `adb devices` via the host adb and reports readiness for the
// configured serial. Readiness is OBSERVED on the wire (the device is
// listed AND online), never assumed — a non-ready result is an honest
// "not yet booted", not a failure.
func (c *Cuttlefish) Status(ctx context.Context) (StatusResult, error) {
	out, err := c.executor.Execute(ctx, c.cfg.ADBBinaryPath, "devices")
	raw := strings.TrimSpace(string(out))
	if err != nil {
		return StatusResult{Raw: raw}, fmt.Errorf("adb devices: %w (output: %s)", err, raw)
	}
	devices := ParseADBDevices(out)
	return StatusResult{
		Devices: devices,
		Ready:   readinessForSerial(devices, c.cfg.ADBSerial),
		Raw:     raw,
	}, nil
}

// WaitForReady polls Status until the configured serial is online or the
// timeout elapses. Returns the elapsed duration. A non-nil error means the
// device was NOT observed online within the budget — this function does
// NOT report "probably ready" (composes §11.4.6 no-guessing).
//
// CF-1 (GENY-1 class, §11.4.108): every Status call is bound to a deadline
// derived from `timeout`, NOT the raw caller ctx. Without this, a caller
// passing context.Background() plus a wedged `adb devices` (stalled vsock
// transport / crashed crosvm / hung adb-server) hangs WaitForReady FOREVER:
// the underlying exec.CommandContext(ctx, ...) in Status->Execute never
// gets cancelled because the raw ctx never fires, so the `timeout` argument
// is silently not honored. Deriving cctx here (mirroring
// genymotion.Tool.StartAndWait's GENY-1 fix) guarantees the deadline
// cancels every in-flight Status exec even when the caller's ctx never
// would.
//
// The poll select deliberately distinguishes CALLER cancellation from our
// OWN internal deadline elapsing: cctx.Done() fires for either reason (it
// is derived from ctx via context.WithDeadline, so it also fires the
// instant the caller's own ctx is cancelled/expires — context.WithDeadline
// takes the EARLIER of the two deadlines). When it fires we check ctx.Err()
// directly — if the CALLER's ctx is what expired, we return that error
// promptly (unchanged contract: an operator-cancelled wait fails fast with
// the caller's own error, exercised by the WaitForReadyContextCancelled
// chaos case). If ctx is still alive (context.Background() or any ctx
// without its own earlier deadline never returns a non-nil Err()), only
// OUR internal `timeout` budget elapsed — we fall through instead of
// returning early, so the `for` loop condition below becomes false and the
// existing friendly, named-serial "timed out" error is returned, exactly
// as before this fix (exercised by the ProcessDeathDuringReadinessWait
// chaos case, which asserts on that exact message).
func (c *Cuttlefish) WaitForReady(ctx context.Context, timeout time.Duration) (time.Duration, error) {
	startedAt := time.Now()
	deadline := startedAt.Add(timeout)
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for time.Now().Before(deadline) {
		st, err := c.Status(cctx)
		if err == nil && st.Ready {
			return time.Since(startedAt), nil
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return time.Since(startedAt), ctx.Err()
			}
			// Our own internal deadline elapsed, not the caller's ctx —
			// fall through; the loop condition below is now false.
		case <-time.After(2 * time.Second):
		}
	}
	return time.Since(startedAt), fmt.Errorf(
		"cuttlefish: WaitForReady timed out after %s waiting for adb serial %q to come online",
		timeout, c.cfg.ADBSerial)
}

// Stop tears the instance down: it runs `stop_cvd` inside the container for
// a graceful Cuttlefish shutdown, then force-removes the container (`rm -f`),
// which is the AUTHORITATIVE teardown — it tears down the container's PID
// namespace and kills every cvd process inside. A best-effort `stop_cvd`
// failure does NOT abort the force-remove — the container MUST be cleaned up
// regardless (§11.4.14 leave-the-target-quiescent). Per §11.4.174 Stop does
// NOT perform a host-wide cvd reap (no host-side ownership token exists); the
// container removal is the only signal that reaches cvd processes. The hard
// error (container removal) is returned.
func (c *Cuttlefish) Stop(ctx context.Context) error {
	if c.containerName == "" {
		// Launch was never called on this instance — defensive no-op
		// success (callers invoke Stop in a defer).
		return nil
	}

	// Graceful Cuttlefish shutdown via stop_cvd inside the container.
	// Best-effort: a non-zero exit (cvd already gone) is not fatal.
	_, _ = c.executor.Execute(ctx, c.cfg.RuntimeBinary,
		buildContainerExecArgs(c.containerName, buildStopCvdArgs())...)

	// Force-remove the container — the AUTHORITATIVE teardown. `rm -f` force-
	// stops + removes the container, tearing down its OWN PID namespace (the
	// container is launched WITHOUT `--pid host`, see buildContainerRunArgs),
	// which kills every cvd process inside it (crosvm / run_cvd /
	// cvd_internal_start).
	out, rmErr := c.executor.Execute(ctx, c.cfg.RuntimeBinary, "rm", "-f", c.containerName)

	// §11.4.174: do NOT reap cvd processes host-wide. The cvd processes run in
	// the container's PID namespace and carry NO host-visible ownership token
	// (the instance number is not plumbed from the host — buildLaunchCvdArgs is
	// unused in production and CF_BASE_INSTANCE is never set here — and their
	// argv references no container-name token). After `rm -f` above OUR cvd are
	// already dead with the container's PID namespace, so a host-wide
	// crosvm/run_cvd comm reap could ONLY hit a DIFFERENT (foreign / concurrent)
	// instance's cvd — the exact §11.4.174 violation. We therefore hand the
	// reaper an EMPTY ownership token, which makes it REFUSE (safe no-op); `rm
	// -f` above is the sole teardown.
	//
	// CF-3 (§11.4.124): call CleanupWithGracePeriod (not the fixed-window
	// Cleanup) so cfg.StopGracePeriod is genuinely threaded into the
	// mechanism its own doc comment describes ("how long Stop waits after
	// stop_cvd before reaping orphan crosvm/run_cvd processes"). The empty
	// ownerToken still makes this an immediate no-op today (the §11.4.174
	// safety property above is unchanged) — but the configured grace period
	// is no longer a dead field: it is the value this call would honor the
	// moment a host-visible ownership token is ever plumbed in, and it is
	// directly exercised by cleanupWithDepsGrace's own unit coverage.
	_, _ = CleanupWithGracePeriod(ctx, "", c.cfg.StopGracePeriod)

	c.containerName = ""

	if rmErr != nil {
		return fmt.Errorf("%s rm: %w (output: %s)", c.cfg.RuntimeBinary, rmErr, string(out))
	}
	return nil
}

// ContainerName returns the runtime-side container name set by Launch.
// Empty before Launch or after Stop. Exposed for the caller's evidence row.
func (c *Cuttlefish) ContainerName() string { return c.containerName }

// buildContainerRunArgs constructs the `podman run` / `docker run` argument
// slice for a privileged Cuttlefish container. It is pure: present is the
// already-host-presence-filtered device list (see FilterPresentDevices),
// so this function does not touch the filesystem and is fully unit-testable.
//
// Cuttlefish requires (FACT, docs/design/CUTTLEFISH_TIER2.md §3 + RESEARCH):
//   - --privileged                 (the §11.4.161 documented exception)
//   - --network host               (cvd networking)
//   - --device <node> per present  (/dev/kvm, /dev/vhost-vsock, ...)
//   - --group-add <grp> per group  (kvm, cvdnetwork, render)
//
// Anti-bluff (Bluff-Audit — recorded in the implementing commit body):
//
//	Mutation: drop "--privileged" from the produced args.
//	Observed: TestBuildContainerRunArgs_Privileged asserts the slice
//	          contains "--privileged"; the mutated builder omits it,
//	          failing the test (a non-privileged Cuttlefish cannot boot).
//	Reverted: yes.
func buildContainerRunArgs(
	containerName string,
	image string,
	presentDevices []string,
	groups []string,
	privileged bool,
	networkHost bool,
) []string {
	args := []string{
		"run",
		"-d",
		"--name", containerName,
		"--rm",
	}
	if privileged {
		args = append(args, "--privileged")
	}
	if networkHost {
		args = append(args, "--network", "host")
	}
	for _, d := range presentDevices {
		args = append(args, "--device", d)
	}
	for _, g := range groups {
		args = append(args, "--group-add", g)
	}
	args = append(args, image)
	return args
}

// buildContainerExecArgs constructs `exec <container> <cmd...>` for running
// a command (e.g. stop_cvd) inside a running Cuttlefish container.
func buildContainerExecArgs(containerName string, cmd []string) []string {
	args := []string{"exec", containerName}
	return append(args, cmd...)
}

// buildLaunchCvdArgs constructs the `launch_cvd --daemon` argument slice
// the container entrypoint runs (FACT, docs/design/CUTTLEFISH_TIER2.md
// §4.3). `instanceNum` (1-based) selects the cvd instance; <=0 omits the
// flag so the cvd default (instance 1) is used.
//
// Pure + exported-shape so the entrypoint contract is unit-tested without
// a real cvd present.
func buildLaunchCvdArgs(instanceNum int) []string {
	args := []string{"launch_cvd", "--daemon"}
	if instanceNum > 0 {
		args = append(args, fmt.Sprintf("--base_instance_num=%d", instanceNum))
	}
	return args
}

// buildStopCvdArgs constructs the `stop_cvd` argument slice for graceful
// teardown (FACT, docs/design/CUTTLEFISH_TIER2.md §4.4).
func buildStopCvdArgs() []string {
	return []string{"stop_cvd"}
}

// sanitizeContainerName makes an instance name safe as a container name
// (alphanumeric + dash + underscore; everything else becomes a dash).
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

// Compile-time check that Cuttlefish satisfies Lifecycle.
var _ Lifecycle = (*Cuttlefish)(nil)
