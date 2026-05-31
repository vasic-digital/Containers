package brokertest

import (
	"context"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// TestStartPostgres_RealContainer is the end-to-end, real-run integration test.
// It starts a real PostgreSQL container, proves it is actually serving queries
// by waiting on `pg_isready` (the StartPostgres readiness gate) and asserting a
// well-formed DSN whose port hosts a live server, then tears it down and asserts
// via pkg/runtime that the container is truly gone.
func TestStartPostgres_RealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	dsn, stop, err := StartPostgres(ctx)
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	// Guarantee cleanup even if an assertion below fails/panics.
	defer stop()

	// DSN shape: postgres://postgres:helix@127.0.0.1:<port>/postgres?sslmode=disable
	if !strings.HasPrefix(dsn, "postgres://postgres:helix@127.0.0.1:") {
		t.Fatalf("unexpected DSN prefix %q", dsn)
	}
	if !strings.HasSuffix(dsn, "/postgres?sslmode=disable") {
		t.Fatalf("unexpected DSN suffix %q", dsn)
	}
	port := postgresPortFromDSN(t, dsn)
	if port == "" {
		t.Fatalf("could not extract port from DSN %q", dsn)
	}

	// Sink-side evidence: StartPostgres only returns once pg_isready reported the
	// server accepting connections. Re-confirm independently that pg_isready
	// succeeds inside the container right now — this proves end-user-visible
	// query-serving readiness, not just that a TCP port is bound.
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		t.Fatalf("runtime %q unexpectedly unsupported", rt.Name())
	}
	name := postgresContainerNameFor(t, rt, port)

	ready, unsupported, perr := pgIsReady(ctx, binary, name)
	if unsupported {
		// Exec path not available — the TCP fallback governed readiness instead;
		// confirm the port is at least reachable as the sink-side signal.
		if _, derr := tcpReachable(ctx, port); derr != nil {
			t.Fatalf("postgres port %s not reachable and exec unsupported: %v", port, derr)
		}
		t.Logf("pg_isready exec unsupported on %s; verified TCP reachability of port %s", rt.Name(), port)
	} else if !ready {
		t.Fatalf("pg_isready reported not-ready for a started container: %v", perr)
	} else {
		t.Logf("verified pg_isready accepting connections for container %s (DSN %s)", name, dsn)
	}

	// Sanity: the container exists and is running BEFORE stop().
	if st, err := rt.Status(ctx, name); err != nil {
		t.Fatalf("expected container %s to exist before stop: %v", name, err)
	} else if st.State != runtime.StateRunning {
		t.Fatalf("expected container %s running before stop, got %s", name, st.State)
	}

	// Now stop and assert it is gone via pkg/runtime.List.
	stop()

	if !waitGone(ctx, rt, name) {
		t.Fatalf("container %s still present after stop()", name)
	}
	t.Logf("confirmed container %s removed after stop()", name)
}

// TestStartPostgres_StopIdempotent proves stop() is safe to call multiple times
// and that a second call after removal does not error or panic. (Mutation
// pairing: if makeStop dropped its sync.Once / force-remove, a double call would
// surface a runtime error path — this guards the contract.)
func TestStartPostgres_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	_, stop, err := StartPostgres(ctx)
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	stop()
	stop() // must not panic or hang
	stop()
}

// TestWaitPostgresReady_TimesOutWhenNothingListening is the mutation-style proof
// that readiness genuinely WAITS for Postgres rather than returning success
// blindly. We point waitPostgresReady at a container name that does not exist
// AND a closed host port with a short deadline; both the exec probe and the TCP
// fallback MUST fail, so it MUST return a timeout error. If waitPostgresReady
// were a no-op (the mutation), this test would fail.
func TestWaitPostgresReady_TimesOutWhenNothingListening(t *testing.T) {
	port := freeClosedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	start := time.Now()
	// A bogus binary that does not exist makes exec fail immediately; combined
	// with the closed port the TCP fallback also fails. The function must keep
	// waiting until the deadline rather than declaring success.
	err := waitPostgresReady(ctx, "definitely-not-a-real-runtime-binary", "brokertest-nonexistent", port)
	if err == nil {
		t.Fatalf("waitPostgresReady returned nil for a dead container/closed port — it did not wait/verify")
	}
	if time.Since(start) < 1000*time.Millisecond {
		t.Fatalf("waitPostgresReady returned too early (%v); it is not honoring ctx", time.Since(start))
	}
}

// TestPgIsReady_DetectsRunningServer proves the readiness probe is real against
// a live container: after StartPostgres returns, pg_isready inside the container
// MUST report ready. Paired with the timeout test above, this shows the probe
// discriminates a serving server from an absent one. Skips only when no runtime.
func TestPgIsReady_DetectsRunningServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	dsn, stop, err := StartPostgres(ctx)
	if err != nil {
		t.Fatalf("StartPostgres: %v", err)
	}
	defer stop()

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		t.Fatalf("runtime %q unexpectedly unsupported", rt.Name())
	}
	port := postgresPortFromDSN(t, dsn)
	name := postgresContainerNameFor(t, rt, port)

	ready, unsupported, perr := pgIsReady(ctx, binary, name)
	if unsupported {
		t.Skipf("SKIP-OK: pg_isready exec unsupported on runtime %s", rt.Name())
	}
	if !ready {
		t.Fatalf("pg_isready reported not-ready for a running server: %v", perr)
	}
}

// TestIsExecUnsupported verifies the exec-vs-not-ready discrimination used by
// pgIsReady: runtime/exec failures map to unsupported; a plain pg_isready
// not-ready status line does not.
func TestIsExecUnsupported(t *testing.T) {
	unsupported := []string{
		"Error: executable file not found in $PATH",
		"OCI runtime exec failed: exec failed: unable to start container process",
		"unknown command \"exec\" for \"podman\"",
		"pg_isready: command not found",
	}
	for _, s := range unsupported {
		if !isExecUnsupported(s) {
			t.Fatalf("isExecUnsupported(%q) = false, want true", s)
		}
	}
	notUnsupported := []string{
		"127.0.0.1:5432 - accepting connections",
		"127.0.0.1:5432 - rejecting connections",
		"127.0.0.1:5432 - no response",
		"",
	}
	for _, s := range notUnsupported {
		if isExecUnsupported(s) {
			t.Fatalf("isExecUnsupported(%q) = true, want false", s)
		}
	}
}

// TestApplyPostgresOptions verifies the shared Option surface maps onto a
// postgresConfig: defaults are postgres-specific, and overrides are adopted.
func TestApplyPostgresOptions(t *testing.T) {
	def := applyPostgresOptions(nil)
	if def.image != postgresImage {
		t.Fatalf("default image = %q, want %q", def.image, postgresImage)
	}
	if def.memoryLimit != postgresDefaultMemoryLimit {
		t.Fatalf("default memory = %q, want %q", def.memoryLimit, postgresDefaultMemoryLimit)
	}
	if def.hostPort != "" {
		t.Fatalf("default hostPort = %q, want empty", def.hostPort)
	}

	cfg := applyPostgresOptions([]Option{
		WithImage("docker.io/library/postgres:15-alpine"),
		WithMemoryLimit("512m"),
		WithHostPort("15432"),
	})
	if cfg.image != "docker.io/library/postgres:15-alpine" {
		t.Fatalf("override image = %q", cfg.image)
	}
	if cfg.memoryLimit != "512m" {
		t.Fatalf("override memory = %q", cfg.memoryLimit)
	}
	if cfg.hostPort != "15432" {
		t.Fatalf("override hostPort = %q", cfg.hostPort)
	}
}

// --- test helpers ---

// postgresPortFromDSN extracts the host port from a StartPostgres DSN.
func postgresPortFromDSN(t *testing.T, dsn string) string {
	t.Helper()
	// postgres://postgres:helix@127.0.0.1:<port>/postgres?sslmode=disable
	const marker = "@127.0.0.1:"
	i := strings.Index(dsn, marker)
	if i < 0 {
		return ""
	}
	rest := dsn[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// postgresContainerNameFor finds the brokertest postgres container that
// published the given host port, via pkg/runtime.List. This lets the test
// assert removal without the package exposing its internal name.
func postgresContainerNameFor(t *testing.T, rt runtime.ContainerRuntime, hostPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := rt.List(ctx, runtime.ListFilter{All: true})
	if err != nil {
		t.Fatalf("runtime.List: %v", err)
	}
	for _, in := range infos {
		if !strings.Contains(in.Name, "brokertest-postgres-") {
			continue
		}
		st, err := rt.Status(ctx, in.Name)
		if err != nil {
			continue
		}
		if postgresClientHostPort(st) == hostPort {
			return in.Name
		}
	}
	t.Fatalf("could not locate brokertest postgres container publishing host port %s", hostPort)
	return ""
}
