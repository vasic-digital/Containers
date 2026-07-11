package brokertest

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// postgresImage is the default single-node PostgreSQL image. The alpine variant
// keeps the pull and memory footprint tiny while still serving real SQL.
// NOTE (BROK-5): a moving tag like "16-alpine" is NOT byte-for-byte reproducible
// — the registry can re-point it at a new digest at any time; only an immutable
// "@sha256:<digest>" reference is. Callers needing pinned reproducibility can
// pass WithImage("docker.io/library/postgres@sha256:...").
const postgresImage = "docker.io/library/postgres:16-alpine"

// postgresPort is the in-container port PostgreSQL listens on for clients.
const postgresPort = "5432"

// postgresPassword is the superuser password set via POSTGRES_PASSWORD and
// embedded in the returned DSN. It is a fixed test credential, never a real
// secret (the container is ephemeral and bound to 127.0.0.1 only).
const postgresPassword = "helix"

// postgresDB is the default database created via POSTGRES_DB and named in the
// returned DSN.
const postgresDB = "postgres"

// postgresUser is the superuser PostgreSQL creates by default.
const postgresUser = "postgres"

// postgresDefaultMemoryLimit caps the PostgreSQL container's memory. Per the
// module's host-session safety mandate (a real OOM incident) every container we
// own MUST declare an explicit memory limit. A single-node Postgres idles well
// under this.
const postgresDefaultMemoryLimit = "256m"

// postgresNameCounter guarantees unique container names without relying on time
// or math/rand. Combined with the process PID it is unique across concurrent
// StartPostgres calls within a process and across processes on the host.
var postgresNameCounter uint64

// postgresConfig holds resolved options for StartPostgres.
type postgresConfig struct {
	image       string
	memoryLimit string
	hostPort    string // empty => let the runtime pick a free host port
}

func defaultPostgresConfig() *postgresConfig {
	return &postgresConfig{
		image:       postgresImage,
		memoryLimit: postgresDefaultMemoryLimit,
		hostPort:    "",
	}
}

// applyPostgresOptions translates the shared Option set onto a postgresConfig.
// It reuses the same WithImage / WithMemoryLimit / WithHostPort options as
// StartNATS by replaying them against a throwaway config seeded with sentinel
// values, so any field the caller actually set becomes distinguishable from one
// left untouched — regardless of whether the caller's value happens to equal a
// NATS default. WithJetStream is silently ignored for Postgres.
//
// The sentinels are non-empty strings no real caller would pass; WithImage and
// WithMemoryLimit ignore empty input, so an empty sentinel could not be
// overwritten — hence the distinctive markers below.
func applyPostgresOptions(opts []Option) *postgresConfig {
	const (
		sentinelImage  = "\x00brokertest-unset-image\x00"
		sentinelMemory = "\x00brokertest-unset-memory\x00"
	)
	probe := &config{
		image:       sentinelImage,
		memoryLimit: sentinelMemory,
		hostPort:    "",
	}
	for _, o := range opts {
		o(probe)
	}
	cfg := defaultPostgresConfig()
	if probe.image != sentinelImage {
		cfg.image = probe.image
	}
	if probe.memoryLimit != sentinelMemory {
		cfg.memoryLimit = probe.memoryLimit
	}
	if probe.hostPort != "" {
		cfg.hostPort = probe.hostPort
	}
	return cfg
}

// StartPostgres launches an ephemeral PostgreSQL and returns a DSN
// ("postgres://postgres:<pw>@127.0.0.1:<port>/postgres?sslmode=disable"), a stop
// func that removes the container, and an error. It selects a container runtime
// via pkg/runtime (podman/docker/nerdctl auto-detect). It honors ctx for the
// startup timeout.
//
// Readiness is CRITICAL: PostgreSQL accepts TCP connections (and even completes
// a first-init restart) before it can actually serve queries. So readiness is
// gated on `pg_isready -U postgres` executed INSIDE the container via the
// detected runtime, retried until it reports ready — not merely on a TCP dial.
// If the runtime cannot exec, it falls back to a TCP dial plus a short grace.
//
// The returned stop function is always non-nil and is safe to call exactly once
// or many times (idempotent). Callers should `defer stop()` immediately after a
// nil-error return to guarantee cleanup even if a later assertion fails.
func StartPostgres(ctx context.Context, opts ...Option) (dsn string, stop func(), err error) {
	cfg := applyPostgresOptions(opts)

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return "", noopStop, fmt.Errorf("brokertest: no container runtime: %w", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		return "", noopStop, fmt.Errorf(
			"brokertest: runtime %q does not support one-off image runs", rt.Name())
	}

	name := fmt.Sprintf("brokertest-postgres-%d-%d", os.Getpid(), atomic.AddUint64(&postgresNameCounter, 1))

	args := []string{
		"run", "-d",
		"--name", name,
		"--memory", cfg.memoryLimit,
		"-e", "POSTGRES_PASSWORD=" + postgresPassword,
		"-e", "POSTGRES_DB=" + postgresDB,
		// Publish the client port to a host loopback port.
		"-p", hostPortSpec(cfg.hostPort) + postgresPort,
		cfg.image,
	}

	// The one capability pkg/runtime's interface lacks: run a NEW container from
	// an image with published ports. Done via the detected runtime's CLI,
	// mirroring StartNATS / StartEtcd. On a run failure runNewContainer
	// best-effort removes any container the failed run may have created (BROK-2).
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

	hostPort, err := resolvePostgresHostPort(phaseCtx, rt, name, cfg.hostPort)
	if err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: resolve host port: %w", err)
	}

	dsn = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		postgresUser, postgresPassword, hostPort, postgresDB)

	if err := waitPostgresReady(phaseCtx, binary, name, hostPort); err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: postgres not ready (%s): %w", dsn, err)
	}

	return dsn, stop, nil
}

// resolvePostgresHostPort returns the host port mapped to the Postgres client
// port. When the caller pinned a port we trust it; otherwise we ask pkg/runtime
// to inspect the container and report the runtime-assigned port.
func resolvePostgresHostPort(
	ctx context.Context, rt runtime.ContainerRuntime, name, pinned string,
) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	var lastErr error
	for {
		st, err := rt.Status(ctx, name)
		if err == nil {
			// Fail fast if the container has already exited/died (BROK-4).
			if exitedState(st) {
				return "", fmt.Errorf(
					"container %s exited before becoming ready (state=%s, exit_code=%d)",
					name, st.State, st.ExitCode)
			}
			if p := postgresClientHostPort(st); p != "" {
				return p, nil
			}
			lastErr = fmt.Errorf("no host port published for container port %s", postgresPort)
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

// postgresClientHostPort extracts the host port mapped to the Postgres client
// port from a container status reported by pkg/runtime.
func postgresClientHostPort(st *runtime.ContainerStatus) string {
	if st == nil {
		return ""
	}
	for _, pm := range st.Ports {
		if pm.ContainerPort == postgresPort && pm.HostPort != "" {
			return pm.HostPort
		}
	}
	return ""
}

// waitPostgresReady blocks until PostgreSQL inside the container actually
// accepts queries, or ctx expires. It runs `pg_isready -U postgres` inside the
// container via the detected runtime in a retry loop. pg_isready reports "ready"
// ONLY once the server is accepting connections — which is the positive,
// sink-side evidence that the broker is operational, NOT merely that the host
// TCP port is bound (Postgres binds TCP before, and again during first-init
// restart, when it cannot yet serve queries).
//
// If the runtime cannot exec into the container (exec unavailable), it falls
// back to a TCP dial against the published host port plus a short grace period
// so the server has time to finish its init-restart before the DSN is returned.
func waitPostgresReady(ctx context.Context, binary, name, hostPort string) error {
	execAvailable := true
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}

		if execAvailable {
			ready, unsupported, err := pgIsReady(ctx, binary, name)
			if ready {
				return nil
			}
			if unsupported {
				// exec is not usable on this runtime — drop to TCP fallback for
				// the rest of the loop.
				execAvailable = false
			} else {
				lastErr = err
			}
		}

		if !execAvailable {
			// BROK-3: exec is unavailable, so prove PostgreSQL answers at the
			// PROTOCOL layer (a StartupMessage handshake) rather than trusting a
			// bare TCP accept + a fixed sleep. The host port is bound by the
			// runtime's forwarder at container START (before PostgreSQL serves —
			// the image's first-init temp server does not listen on TCP), so a
			// TCP accept alone is NOT evidence the server can answer. Delegate to
			// the protocol-probe fallback loop, which re-probes until the server
			// responds or ctx expires — never a fixed-sleep unconditional success.
			return waitPostgresProtocolReady(ctx, hostPort)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// pgIsReady runs `<binary> exec <name> pg_isready -U postgres -h 127.0.0.1`
// inside the container. It returns ready=true only when pg_isready exits 0
// (server accepting connections). unsupported=true signals the runtime could not
// exec at all (e.g. the binary lacks an exec subcommand), so the caller can fall
// back to a TCP probe. A non-zero pg_isready exit (server starting / refusing)
// is returned as ready=false, unsupported=false with the captured output.
func pgIsReady(ctx context.Context, binary, name string) (ready, unsupported bool, err error) {
	// Bound each probe so a hung exec cannot starve the outer readiness loop.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, runErr := exec.CommandContext(probeCtx, binary,
		"exec", name, "pg_isready", "-U", postgresUser, "-h", "127.0.0.1").CombinedOutput()
	if runErr == nil {
		// pg_isready exited 0 => accepting connections.
		return true, false, nil
	}

	trimmed := strings.TrimSpace(string(out))
	// pg_isready exit codes: 1 = rejecting (server starting), 2 = no response,
	// 3 = no attempt (bad params). pg_isready being present and reporting a
	// status (even non-ready) means exec works; we keep probing. We only treat
	// it as "unsupported" when the runtime itself could not locate pg_isready
	// or the exec subcommand — surfaced as an exec/runtime error, never a clean
	// pg_isready non-zero status line.
	if isExecUnsupported(trimmed) {
		return false, true, fmt.Errorf("exec unsupported: %s", trimmed)
	}
	return false, false, fmt.Errorf("pg_isready not ready: %v: %s", runErr, trimmed)
}

// isExecUnsupported heuristically detects that the runtime could not exec the
// command at all (missing binary / unsupported subcommand), as opposed to
// pg_isready running and reporting a not-ready status.
func isExecUnsupported(out string) bool {
	lc := strings.ToLower(out)
	switch {
	case strings.Contains(lc, "executable file not found"),
		strings.Contains(lc, "pg_isready") && strings.Contains(lc, "not found"),
		strings.Contains(lc, "no such file or directory") && !strings.Contains(lc, "accepting"),
		strings.Contains(lc, "unknown command"),
		strings.Contains(lc, "unknown shorthand"),
		strings.Contains(lc, "exec failed"),
		strings.Contains(lc, "oci runtime exec failed"):
		return true
	}
	return false
}

// tcpReachable reports whether a TCP connection to the published host port
// succeeds. It is a bare reachability probe (retained for the real-container
// integration test's sink-side signal); readiness itself uses pgStartupProbe.
func tcpReachable(ctx context.Context, hostPort string) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", hostPort)
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

// waitPostgresProtocolReady loops a PostgreSQL StartupMessage protocol probe
// against the published host port until the server answers at the protocol layer
// or ctx expires. It is the exec-unsupported readiness fallback (BROK-3): it
// replaces the previous "TCP accept + fixed 2s sleep + return nil", which
// declared readiness the moment the runtime forwarder bound the port — before
// PostgreSQL could actually serve. Here every acceptance is a real protocol
// response, and a not-yet-serving endpoint is re-probed rather than slept past.
func waitPostgresProtocolReady(ctx context.Context, hostPort string) error {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		if ok, err := pgStartupProbe(ctx, hostPort); ok {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// pgProtocolVersion is the PostgreSQL v3.0 frontend/backend protocol version
// number (major 3 << 16 | minor 0) carried in a StartupMessage.
const pgProtocolVersion = 196608

// pgStartupProbe opens a TCP connection to the published host port, sends a
// PostgreSQL v3 StartupMessage, and reads the FIRST response byte. A serving
// PostgreSQL answers with 'R' (an Authentication* message) or 'E' (an
// ErrorResponse) — either proves the server is answering at the PROTOCOL layer,
// not merely that the runtime's port-forwarder accepted a TCP connection (which
// it does from container start, before PostgreSQL can serve). It is the BROK-3
// re-verification that replaces a bare TCP dial + fixed sleep in the
// exec-unsupported readiness fallback. No third-party driver is used — the
// handshake is framed with the standard library, mirroring the raw-protocol
// readiness probes for NATS (INFO) and Redis (PING/PONG).
func pgStartupProbe(ctx context.Context, hostPort string) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", hostPort)
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
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(pgStartupMessage(postgresUser, postgresDB)); err != nil {
		return false, err
	}

	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return false, err
	}
	switch buf[0] {
	case 'R', 'E':
		return true, nil
	default:
		return false, fmt.Errorf("unexpected first response byte %q from %s", buf[0], addr)
	}
}

// pgStartupMessage builds a PostgreSQL v3 StartupMessage for the given user and
// database. Layout: Int32 total-length (including itself), Int32 protocol
// version, then NUL-terminated key/value parameter pairs, terminated by a final
// NUL byte.
func pgStartupMessage(user, db string) []byte {
	params := []byte("user\x00" + user + "\x00database\x00" + db + "\x00\x00")
	total := 4 + 4 + len(params) // length field + version field + params
	buf := make([]byte, 0, total)
	buf = binary.BigEndian.AppendUint32(buf, uint32(total))
	buf = binary.BigEndian.AppendUint32(buf, uint32(pgProtocolVersion))
	return append(buf, params...)
}
