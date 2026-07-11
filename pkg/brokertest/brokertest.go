// Package brokertest provisions ephemeral message-broker containers for
// integration testing. It is built entirely on top of the decoupled
// digital.vasic.containers/pkg/runtime abstraction for runtime selection and
// container lifecycle (stop/remove/inspect). The only capability the runtime
// abstraction does not expose is "run a brand-new container from an image with
// published ports" (its ContainerRuntime interface only starts/stops EXISTING
// containers by ID); for that single step this package shells the detected
// runtime's CLI via os/exec, mirroring the precedent already established in
// pkg/crossbuild. All teardown and existence checks go back through
// pkg/runtime, so the lifecycle stays owned by the shared abstraction.
//
// The package depends ONLY on the standard library and
// digital.vasic.containers/pkg/runtime. It imports nothing from any consuming
// project.
package brokertest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// natsImage is the default NATS image used for the ephemeral broker. The alpine
// variant keeps the pull and memory footprint tiny. NOTE (BROK-5): a moving tag
// like "2.10-alpine" is NOT byte-for-byte reproducible — the registry can
// re-point it at a new digest at any time; only an immutable "@sha256:<digest>"
// reference is truly reproducible. Callers that need pinned reproducibility can
// pass WithImage("docker.io/library/nats@sha256:...").
const natsImage = "docker.io/library/nats:2.10-alpine"

// natsClientPort is the in-container port NATS listens on for clients.
const natsClientPort = "4222"

// defaultMemoryLimit caps the container's memory. Per the module's host-session
// safety mandate (a real OOM incident), every container we own MUST declare an
// explicit memory limit. NATS+JetStream idles comfortably under this.
const defaultMemoryLimit = "128m"

// nameCounter guarantees unique container names without relying on time or
// math/rand. Combined with the process PID it is unique across concurrent
// StartNATS calls within a process and across processes on the host.
var nameCounter uint64

// runtimeBinary maps a pkg/runtime runtime name to its CLI binary. Only the
// CLI-backed, docker-compatible runtimes are supported for one-off runs.
func runtimeBinary(name string) (string, bool) {
	switch name {
	case "podman", "docker", "nerdctl":
		return name, true
	default:
		return "", false
	}
}

// config holds resolved options for StartNATS.
type config struct {
	image       string
	jetStream   bool
	memoryLimit string
	hostPort    string // empty => let the runtime pick a free host port
}

func defaultConfig() *config {
	return &config{
		image:       natsImage,
		jetStream:   true,
		memoryLimit: defaultMemoryLimit,
		hostPort:    "",
	}
}

// Option configures StartNATS.
type Option func(*config)

// WithImage overrides the NATS image reference.
func WithImage(image string) Option {
	return func(c *config) {
		if image != "" {
			c.image = image
		}
	}
}

// WithJetStream toggles the JetStream (-js) flag. Default is enabled.
func WithJetStream(enabled bool) Option {
	return func(c *config) { c.jetStream = enabled }
}

// WithMemoryLimit overrides the container memory limit (e.g. "64m", "256m").
func WithMemoryLimit(limit string) Option {
	return func(c *config) {
		if limit != "" {
			c.memoryLimit = limit
		}
	}
}

// WithHostPort pins the host port the container's 4222 is mapped to. By
// default a free high port is chosen by the runtime. Mainly useful for tests
// that need a deterministic endpoint.
func WithHostPort(port string) Option {
	return func(c *config) {
		if port != "" {
			c.hostPort = port
		}
	}
}

// StartNATS launches an ephemeral NATS+JetStream container and returns its
// client endpoint ("nats://127.0.0.1:<port>"), a stop function that removes the
// container, and an error. It selects a container runtime via pkg/runtime
// (podman/docker/nerdctl auto-detect). It honors ctx for the startup timeout:
// readiness is gated on a real TCP dial to the published port that reads the
// NATS server's "INFO" greeting.
//
// The returned stop function is always non-nil and is safe to call exactly once
// or many times (idempotent). Callers should `defer stop()` immediately after a
// nil-error return to guarantee cleanup even if a later assertion fails.
func StartNATS(ctx context.Context, opts ...Option) (endpoint string, stop func(), err error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(cfg)
	}

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return "", noopStop, fmt.Errorf("brokertest: no container runtime: %w", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		return "", noopStop, fmt.Errorf(
			"brokertest: runtime %q does not support one-off image runs", rt.Name())
	}

	name := fmt.Sprintf("brokertest-nats-%d-%d", os.Getpid(), atomic.AddUint64(&nameCounter, 1))

	args := []string{
		"run", "-d",
		"--name", name,
		"--memory", cfg.memoryLimit,
		// Map container 4222 to a host port. Empty host port => runtime picks a
		// free high port, which we then resolve via pkg/runtime Status.
		"-p", hostPortSpec(cfg.hostPort) + natsClientPort,
	}
	args = appendImagePositional(args, cfg.image)
	if cfg.jetStream {
		args = append(args, "-js")
	}

	// The one capability pkg/runtime's interface lacks: run a NEW container from
	// an image with published ports. Done via the detected runtime's CLI. On a
	// run failure runNewContainer best-effort removes any container the failed
	// run may have created before returning the error (BROK-2).
	runOut, runErr := runNewContainer(ctx, rt, execRun, binary, name, args)
	if runErr != nil {
		return "", noopStop, fmt.Errorf(
			"brokertest: %s run failed: %w: %s", binary, runErr, strings.TrimSpace(string(runOut)))
	}

	// From here on, ALL lifecycle goes back through pkg/runtime so the shared
	// abstraction owns teardown. Build the stop closure first so any later
	// failure path can still clean up.
	stop = makeStop(rt, name)

	// Bound the resolve + readiness phase so a container that starts but never
	// serves fails with a clear timeout instead of hanging on a deadline-less
	// ctx (BROK-1, §11.4.108). A caller-supplied deadline is respected as-is.
	phaseCtx, cancelPhase := startupContext(ctx, defaultStartupTimeout)
	defer cancelPhase()

	hostPort, err := resolveHostPort(phaseCtx, rt, name, cfg.hostPort)
	if err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: resolve host port: %w", err)
	}

	endpoint = fmt.Sprintf("nats://127.0.0.1:%s", hostPort)
	if err := waitReady(phaseCtx, hostPort); err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: nats not ready at %s: %w", endpoint, err)
	}

	return endpoint, stop, nil
}

// hostPortSpec returns the "host:" prefix for a -p mapping. With an empty host
// port the result is "127.0.0.1::" so the runtime binds a free high port on
// loopback only.
func hostPortSpec(hostPort string) string {
	if hostPort == "" {
		return "127.0.0.1::"
	}
	return "127.0.0.1:" + hostPort + ":"
}

// noopStop is the stop function returned on the error paths where nothing was
// created. It is safe to call.
func noopStop() {}

// makeStop returns an idempotent teardown closure that removes the container
// via pkg/runtime.
func makeStop(rt runtime.ContainerRuntime, name string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { removeContainer(rt, name) })
	}
}

// removeContainer force-removes a container by name via pkg/runtime, using a
// fresh, bounded context so cleanup still runs even if the caller's ctx is
// already cancelled/expired. Force-remove handles both running and stopped
// containers in one call and is what guarantees no leak per host-session safety.
// It is shared by the teardown closure (makeStop) and the BROK-2 run-error
// cleanup path (runNewContainer).
func removeContainer(rt runtime.ContainerRuntime, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = rt.Remove(ctx, name, runtime.WithForceRemove(true), runtime.WithRemoveVolumes(true))
}

// defaultStartupTimeout bounds the resolve-host-port + readiness phase when the
// caller's context carries no deadline of its own. Without this bound (BROK-1,
// §11.4.108), a container that is created but never serves — crash-looping,
// wrong entrypoint, or a host port bound by the runtime forwarder with nothing
// listening behind it — would make the resolve or readiness loop spin until the
// process exits. With it, a wedged container fails with a clear "not ready"
// error instead of hanging forever. 90s comfortably covers a cold image pull +
// first-boot of the supported single-node brokers.
const defaultStartupTimeout = 90 * time.Second

// startupContext returns the context under which the resolve-host-port +
// readiness phase runs. If parent already carries its own deadline the caller's
// bound wins and parent is returned unchanged (with a no-op cancel). Otherwise a
// bounded child is derived so a deadline-less caller (e.g. context.Background())
// cannot hang forever on a container that never becomes ready (BROK-1).
func startupContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// exitedState reports whether a container status indicates the container has
// already exited or died and will therefore never become ready — so the
// resolve/readiness loop should fail fast (BROK-4) instead of spinning until the
// startup timeout. pkg/runtime models an exited/dead container as StateStopped
// or StateDead (its ContainerState enum has no "exited" member).
func exitedState(st *runtime.ContainerStatus) bool {
	if st == nil {
		return false
	}
	return st.State == runtime.StateStopped || st.State == runtime.StateDead
}

// failIfExited extends the BROK-4 exited-container fast-fail to the pinned
// host-port branch of every resolver (BROK2-1). The auto-port branch inspects
// the container in its resolve loop and already fast-fails on an exited/dead
// container; the pinned branch returned the caller's port WITHOUT any status
// check, so a pinned-port caller whose container exited immediately would spin
// the downstream readiness loop all the way to the 90s startup timeout — the
// exact "spinning only delays a certain failure" case BROK-4 was added to kill,
// left uncovered for pinned ports. This helper closes that gap conservatively:
// it fast-fails ONLY when a status inspection SUCCEEDS and reports an exited
// container. A status error is ignored (nil returned) so the caller's pin is
// still trusted exactly as before — the fix is a strict superset that changes
// behaviour solely for a confirmed-exited pinned container.
func failIfExited(ctx context.Context, rt runtime.ContainerRuntime, name string) error {
	if st, err := rt.Status(ctx, name); err == nil && exitedState(st) {
		return fmt.Errorf(
			"container %s exited before becoming ready (state=%s, exit_code=%d)",
			name, st.State, st.ExitCode)
	}
	return nil
}

// containerRunner runs a brand-new detached container from an image with
// published ports — the single capability pkg/runtime's ContainerRuntime
// interface does not expose (it only start/stop/removes EXISTING containers).
// execRun is the production implementation (shells the detected runtime CLI);
// the hardening guard suite substitutes a fake so the run-error cleanup path
// (BROK-2) is exercised without a real container (§11.4.27).
type containerRunner func(ctx context.Context, binary string, args []string) ([]byte, error)

// execRun is the production containerRunner: it shells the detected runtime CLI.
// appendImagePositional appends QEMU/docker/podman's end-of-options `--`
// delimiter followed by the image to a `run` args slice, so a leading-dash
// image (e.g. supplied via WithImage) is parsed as the IMAGE positional and
// never as a run flag (Wave-20 SITE-10a ARGSWEEP argv flag-injection; mirrors
// the emulator EMU2-1 / cuttlefish CF2 / genymotion GENY2 `--`-before-image
// precedent). Any container command + args are appended by the caller AFTER
// the image. Pure function so the built args are unit testable without a live
// runtime.
func appendImagePositional(args []string, image string) []string {
	return append(args, "--", image)
}

func execRun(ctx context.Context, binary string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, args...).CombinedOutput()
}

// runNewContainer runs a one-off detached container and, on failure, best-effort
// removes the container by its (deterministic, already-known) name before
// returning the error. This closes the BROK-2 leak: `run -d` can CREATE the
// container and THEN exit non-zero (host-port bind conflict, OCI reject,
// entrypoint dies immediately), and a ctx cancellation can kill the CLI after
// the daemon already created the container — both leave a created/exited
// container behind unless it is removed (§11.4.14). The removal goes back
// through pkg/runtime so the shared abstraction still owns teardown.
func runNewContainer(
	ctx context.Context, rt runtime.ContainerRuntime, run containerRunner,
	binary, name string, args []string,
) ([]byte, error) {
	out, err := run(ctx, binary, args)
	if err != nil {
		removeContainer(rt, name)
		return out, err
	}
	return out, nil
}

// resolveHostPort returns the host port mapped to the container's client port.
// When the caller pinned a port we trust it; otherwise we ask pkg/runtime to
// inspect the container and report the runtime-assigned port.
func resolveHostPort(
	ctx context.Context, rt runtime.ContainerRuntime, name, pinned string,
) (string, error) {
	if pinned != "" {
		if err := failIfExited(ctx, rt, name); err != nil { // BROK2-1
			return "", err
		}
		return pinned, nil
	}
	// Inspect can briefly race container creation; retry within ctx.
	var lastErr error
	for {
		st, err := rt.Status(ctx, name)
		if err == nil {
			// Fail fast if the container has already exited/died (BROK-4): it
			// will never publish a working port, so spinning until the startup
			// timeout only delays a certain failure.
			if exitedState(st) {
				return "", fmt.Errorf(
					"container %s exited before becoming ready (state=%s, exit_code=%d)",
					name, st.State, st.ExitCode)
			}
			if p := clientHostPort(st); p != "" {
				return p, nil
			}
			lastErr = errors.New("no host port published for container port " + natsClientPort)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// clientHostPort extracts the host port mapped to the NATS client port from a
// container status reported by pkg/runtime.
func clientHostPort(st *runtime.ContainerStatus) string {
	if st == nil {
		return ""
	}
	for _, pm := range st.Ports {
		if pm.ContainerPort == natsClientPort && pm.HostPort != "" {
			return pm.HostPort
		}
	}
	return ""
}

// waitReady blocks until a TCP connection to the host port yields the NATS
// server greeting (an "INFO {...}" line), or ctx expires. NATS sends INFO
// immediately on connect, so reading it proves the broker is actually serving —
// not merely that the port is bound.
func waitReady(ctx context.Context, hostPort string) error {
	addr := net.JoinHostPort("127.0.0.1", hostPort)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		ok, err := readsNATSGreeting(ctx, addr)
		if ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// readsNATSGreeting dials addr and returns true only if the server sends a line
// beginning with "INFO". This is the positive, sink-side evidence that the NATS
// broker is operational, as required by the module's anti-bluff mandate.
func readsNATSGreeting(ctx context.Context, addr string) (bool, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 5)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return false, err
	}
	if strings.HasPrefix(string(buf[:n]), "INFO") {
		return true, nil
	}
	return false, fmt.Errorf("unexpected greeting %q", string(buf[:n]))
}
