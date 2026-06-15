package brokertest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// TestStartRedis_RealContainer is the end-to-end, real-run integration test. It
// starts a real single-node Redis container, proves the broker is actually
// serving by sending a RESP PING and reading "+PONG" off a raw TCP socket, then
// tears it down and asserts via pkg/runtime that the container is truly gone.
func TestStartRedis_RealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	addr, stop, err := StartRedis(ctx)
	if err != nil {
		t.Fatalf("StartRedis: %v", err)
	}
	// Guarantee cleanup even if an assertion below fails/panics.
	defer stop()

	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("unexpected addr %q", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("parse addr %q: %v", addr, err)
	}
	if host != "127.0.0.1" || port == "" {
		t.Fatalf("unexpected host/port in addr %q", addr)
	}

	// Sink-side evidence: open a raw TCP connection, PING, and verify the server
	// answers "+PONG". This proves end-user-visible operation, not just that a
	// process started.
	reply := pingRedis(t, addr)
	if !strings.HasPrefix(reply, "+PONG") {
		t.Fatalf("expected Redis PING reply to start with +PONG, got %q", reply)
	}
	t.Logf("verified Redis PONG at %s: %s", addr, firstLine(reply))

	// Resolve the runtime + name so we can assert removal via pkg/runtime.
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	name := redisContainerNameFor(t, rt, port)

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

// TestStartRedis_StopIdempotent proves stop() is safe to call multiple times and
// that a second call after removal does not error or panic. (Mutation pairing:
// if makeStop dropped its sync.Once / force-remove, a double call would surface
// a runtime error path — this guards the contract.)
func TestStartRedis_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	_, stop, err := StartRedis(ctx)
	if err != nil {
		t.Fatalf("StartRedis: %v", err)
	}
	stop()
	stop() // must not panic or hang
	stop()
}

// TestWaitRedisReady_TimesOutWhenNothingListening is the mutation-style proof
// that readiness genuinely WAITS for Redis rather than returning success
// blindly. We point waitRedisReady at a closed/unbound port with a short
// deadline; it MUST return a timeout error. If waitRedisReady were a no-op (the
// mutation), this test would fail.
func TestWaitRedisReady_TimesOutWhenNothingListening(t *testing.T) {
	port := freeClosedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitRedisReady(ctx, port)
	if err == nil {
		t.Fatalf("waitRedisReady returned nil for closed port %s — it did not wait/verify", port)
	}
	if time.Since(start) < 700*time.Millisecond {
		t.Fatalf("waitRedisReady returned too early (%v); it is not honoring ctx", time.Since(start))
	}
}

// TestReadsRedisPong_RejectsNonRedis proves the PING check is real: a server
// that replies with something other than "+PONG" must be rejected. This is the
// mutation pairing for the anti-bluff readiness evidence — a port being open is
// NOT accepted as readiness.
func TestReadsRedisPong_RejectsNonRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		// Read whatever the prober writes (the PING), then reply with a non-Redis
		// line so the probe must reject it.
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = conn.Read(make([]byte, 16))
		_, _ = conn.Write([]byte("-ERR not redis\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsRedisPong(ctx, ln.Addr().String())
	if ok {
		t.Fatalf("readsRedisPong accepted a non-Redis server")
	}
	if err == nil {
		t.Fatalf("expected an error describing the unexpected reply")
	}
}

// TestReadsRedisPong_AcceptsPong proves the positive path: a server that replies
// "+PONG" to PING is accepted. Paired with the rejection test above, this shows
// the check discriminates correctly.
func TestReadsRedisPong_AcceptsPong(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = conn.Read(make([]byte, 16))
		_, _ = conn.Write([]byte("+PONG\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsRedisPong(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("readsRedisPong errored on a valid +PONG reply: %v", err)
	}
	if !ok {
		t.Fatalf("readsRedisPong rejected a valid +PONG reply")
	}
}

// TestApplyRedisOptions verifies the shared Option surface maps onto a
// redisConfig: defaults are Redis-specific, and overrides are adopted.
func TestApplyRedisOptions(t *testing.T) {
	def := applyRedisOptions(nil)
	if def.image != redisImage {
		t.Fatalf("default image = %q, want %q", def.image, redisImage)
	}
	if def.memoryLimit != redisDefaultMemoryLimit {
		t.Fatalf("default memory = %q, want %q", def.memoryLimit, redisDefaultMemoryLimit)
	}
	if def.hostPort != "" {
		t.Fatalf("default hostPort = %q, want empty", def.hostPort)
	}

	cfg := applyRedisOptions([]Option{
		WithImage("docker.io/library/redis:7.2-alpine"),
		WithMemoryLimit("256m"),
		WithHostPort("16379"),
	})
	if cfg.image != "docker.io/library/redis:7.2-alpine" {
		t.Fatalf("override image = %q", cfg.image)
	}
	if cfg.memoryLimit != "256m" {
		t.Fatalf("override memory = %q", cfg.memoryLimit)
	}
	if cfg.hostPort != "16379" {
		t.Fatalf("override hostPort = %q", cfg.hostPort)
	}
}

// --- test helpers ---

// pingRedis dials addr, sends an inline RESP PING, and returns the raw reply.
func pingRedis(t *testing.T, addr string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("write PING to %s: %v", addr, err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("reading PING reply from %s: %v", addr, err)
	}
	return string(buf[:n])
}

// redisContainerNameFor finds the brokertest Redis container that published the
// given host port, via pkg/runtime.List. This lets the test assert removal
// without the package exposing its internal name.
func redisContainerNameFor(t *testing.T, rt runtime.ContainerRuntime, hostPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := rt.List(ctx, runtime.ListFilter{All: true})
	if err != nil {
		t.Fatalf("runtime.List: %v", err)
	}
	for _, in := range infos {
		if !strings.Contains(in.Name, "brokertest-redis-") {
			continue
		}
		st, err := rt.Status(ctx, in.Name)
		if err != nil {
			continue
		}
		if redisClientHostPort(st) == hostPort {
			return in.Name
		}
	}
	t.Fatalf("could not locate brokertest redis container publishing host port %s", hostPort)
	return ""
}
