package brokertest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// runtimeAvailable reports whether any CLI-backed container runtime that we can
// drive is present. Used to skip the real integration test gracefully on hosts
// with no runtime (and ONLY then).
func runtimeAvailable(ctx context.Context) bool {
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return false
	}
	_, ok := runtimeBinary(rt.Name())
	return ok
}

// TestStartNATS_RealContainer is the end-to-end, real-run integration test. It
// starts a real NATS+JetStream container, proves the broker is actually serving
// by reading the "INFO" greeting off a raw TCP socket, then tears it down and
// asserts via pkg/runtime that the container is truly gone.
func TestStartNATS_RealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	endpoint, stop, err := StartNATS(ctx)
	if err != nil {
		t.Fatalf("StartNATS: %v", err)
	}
	// Guarantee cleanup even if an assertion below fails/panics.
	defer stop()

	if !strings.HasPrefix(endpoint, "nats://127.0.0.1:") {
		t.Fatalf("unexpected endpoint %q", endpoint)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "nats://"))
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	if host != "127.0.0.1" || port == "" {
		t.Fatalf("unexpected host/port in endpoint %q", endpoint)
	}

	// Sink-side evidence: open a raw TCP connection and verify the server
	// greeting begins with "INFO". This proves end-user-visible operation, not
	// just that a process started.
	greeting := readGreeting(t, endpoint)
	if !strings.HasPrefix(greeting, "INFO") {
		t.Fatalf("expected NATS greeting to start with INFO, got %q", greeting)
	}
	t.Logf("verified NATS greeting at %s: %s", endpoint, firstLine(greeting))

	// Resolve the runtime + name so we can assert removal via pkg/runtime.
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	name := containerNameFor(t, rt, port)

	// Sanity: the container exists and is running BEFORE stop().
	if st, err := rt.Status(ctx, name); err != nil {
		t.Fatalf("expected container %s to exist before stop: %v", name, err)
	} else if st.State != runtime.StateRunning {
		t.Fatalf("expected container %s running before stop, got %s", name, st.State)
	}

	// Now stop and assert it is gone via pkg/runtime.List.
	stop()

	gone := waitGone(ctx, rt, name)
	if !gone {
		t.Fatalf("container %s still present after stop()", name)
	}
	t.Logf("confirmed container %s removed after stop()", name)
}

// TestStartNATS_StopIdempotent proves stop() is safe to call multiple times and
// that a second call after removal does not error or panic. (Mutation pairing:
// if makeStop dropped its sync.Once / force-remove, a double call would surface
// a runtime error path — this guards the contract.)
func TestStartNATS_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	_, stop, err := StartNATS(ctx)
	if err != nil {
		t.Fatalf("StartNATS: %v", err)
	}
	stop()
	stop() // must not panic or hang
	stop()
}

// TestWaitReady_TimesOutWhenNothingListening is the mutation-style proof that
// readiness genuinely WAITS for the broker rather than returning success
// blindly. We point waitReady at a closed/unbound port with a short deadline;
// it MUST return a timeout error. If waitReady were a no-op (the mutation), this
// test would fail.
func TestWaitReady_TimesOutWhenNothingListening(t *testing.T) {
	port := freeClosedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitReady(ctx, port)
	if err == nil {
		t.Fatalf("waitReady returned nil for closed port %s — it did not wait/verify", port)
	}
	if time.Since(start) < 700*time.Millisecond {
		t.Fatalf("waitReady returned too early (%v); it is not honoring ctx", time.Since(start))
	}
}

// TestReadsNATSGreeting_RejectsNonNATS proves the greeting check is real: a
// server that sends something other than "INFO" must be rejected. This is the
// mutation pairing for the anti-bluff readiness evidence — a port being open is
// NOT accepted as readiness.
func TestReadsNATSGreeting_RejectsNonNATS(t *testing.T) {
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
		_, _ = conn.Write([]byte("HELLO not-nats\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsNATSGreeting(ctx, ln.Addr().String())
	if ok {
		t.Fatalf("readsNATSGreeting accepted a non-NATS server")
	}
	if err == nil {
		t.Fatalf("expected an error describing the unexpected greeting")
	}
}

// TestReadsNATSGreeting_AcceptsINFO proves the positive path: a server that
// sends an INFO line is accepted. Paired with the rejection test above, this
// shows the check discriminates correctly.
func TestReadsNATSGreeting_AcceptsINFO(t *testing.T) {
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
		_, _ = conn.Write([]byte("INFO {\"server_id\":\"test\"}\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsNATSGreeting(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("readsNATSGreeting errored on a valid INFO greeting: %v", err)
	}
	if !ok {
		t.Fatalf("readsNATSGreeting rejected a valid INFO greeting")
	}
}

// TestHostPortSpec verifies the -p host-binding prefix for both pinned and
// auto-assigned ports.
func TestHostPortSpec(t *testing.T) {
	if got := hostPortSpec(""); got != "127.0.0.1::" {
		t.Fatalf("hostPortSpec(\"\") = %q, want 127.0.0.1::", got)
	}
	if got := hostPortSpec("5555"); got != "127.0.0.1:5555:" {
		t.Fatalf("hostPortSpec(5555) = %q, want 127.0.0.1:5555:", got)
	}
}

// TestRuntimeBinary verifies only docker-compatible CLI runtimes are accepted.
func TestRuntimeBinary(t *testing.T) {
	for _, name := range []string{"podman", "docker", "nerdctl"} {
		if _, ok := runtimeBinary(name); !ok {
			t.Fatalf("runtimeBinary(%q) should be supported", name)
		}
	}
	for _, name := range []string{"kubernetes", "cri-o", "lxd", ""} {
		if _, ok := runtimeBinary(name); ok {
			t.Fatalf("runtimeBinary(%q) should be unsupported", name)
		}
	}
}

// --- test helpers ---

func readGreeting(t *testing.T, endpoint string) string {
	t.Helper()
	addr := strings.TrimPrefix(endpoint, "nats://")
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("reading greeting from %s: %v", addr, err)
	}
	return string(buf[:n])
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// containerNameFor finds the brokertest container that published the given host
// port, via pkg/runtime.List. This lets the test assert removal without the
// package exposing its internal name.
func containerNameFor(t *testing.T, rt runtime.ContainerRuntime, hostPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := rt.List(ctx, runtime.ListFilter{All: true})
	if err != nil {
		t.Fatalf("runtime.List: %v", err)
	}
	for _, in := range infos {
		if !strings.Contains(in.Name, "brokertest-nats-") {
			continue
		}
		st, err := rt.Status(ctx, in.Name)
		if err != nil {
			continue
		}
		if clientHostPort(st) == hostPort {
			return in.Name
		}
	}
	t.Fatalf("could not locate brokertest container publishing host port %s", hostPort)
	return ""
}

func waitGone(ctx context.Context, rt runtime.ContainerRuntime, name string) bool {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		infos, err := rt.List(ctx, runtime.ListFilter{All: true, Names: []string{name}})
		if err == nil {
			found := false
			for _, in := range infos {
				if in.Name == name {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// freeClosedPort grabs a port, then closes it so nothing is listening — used to
// prove waitReady actually fails when the broker is absent.
func freeClosedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	_ = ln.Close()
	return port
}
