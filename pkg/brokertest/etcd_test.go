package brokertest

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// TestStartEtcd_RealContainer is the end-to-end, real-run integration test. It
// starts a real single-node etcd container, proves it is actually serving by
// reading {"health":"true"} from GET <endpoint>/health, then tears it down and
// asserts via pkg/runtime that the container is truly gone.
func TestStartEtcd_RealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	endpoint, stop, err := StartEtcd(ctx)
	if err != nil {
		t.Fatalf("StartEtcd: %v", err)
	}
	// Guarantee cleanup even if an assertion below fails/panics.
	defer stop()

	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("unexpected endpoint %q", endpoint)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "http://"))
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	if host != "127.0.0.1" || port == "" {
		t.Fatalf("unexpected host/port in endpoint %q", endpoint)
	}

	// Sink-side evidence: GET /health and verify etcd reports health true. This
	// proves end-user-visible operation, not just that a process started.
	body := getHealth(t, endpoint)
	if !strings.Contains(body, `"health":"true"`) {
		t.Fatalf("expected etcd /health to report health true, got %q", body)
	}
	t.Logf("verified etcd /health at %s: %s", endpoint, strings.TrimSpace(body))

	// Resolve the runtime + name so we can assert removal via pkg/runtime.
	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		t.Fatalf("AutoDetect: %v", err)
	}
	name := etcdContainerNameFor(t, rt, port)

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

// TestStartEtcd_StopIdempotent proves stop() is safe to call multiple times and
// that a second call after removal does not error or panic. (Mutation pairing:
// if makeStop dropped its sync.Once / force-remove, a double call would surface
// a runtime error path — this guards the contract.)
func TestStartEtcd_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if !runtimeAvailable(ctx) {
		t.Skip("SKIP-OK: no CLI container runtime detected (podman/docker/nerdctl)")
	}

	_, stop, err := StartEtcd(ctx)
	if err != nil {
		t.Fatalf("StartEtcd: %v", err)
	}
	stop()
	stop() // must not panic or hang
	stop()
}

// TestWaitEtcdReady_TimesOutWhenNothingListening is the mutation-style proof
// that readiness genuinely WAITS for etcd rather than returning success blindly.
// We point waitEtcdReady at a closed/unbound port with a short deadline; it MUST
// return a timeout error. If waitEtcdReady were a no-op (the mutation), this
// test would fail.
func TestWaitEtcdReady_TimesOutWhenNothingListening(t *testing.T) {
	port := freeClosedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitEtcdReady(ctx, port)
	if err == nil {
		t.Fatalf("waitEtcdReady returned nil for closed port %s — it did not wait/verify", port)
	}
	if time.Since(start) < 700*time.Millisecond {
		t.Fatalf("waitEtcdReady returned too early (%v); it is not honoring ctx", time.Since(start))
	}
}

// TestReadsEtcdHealth_RejectsUnhealthy proves the health check is real: a server
// that responds 200 but does NOT report health true must be rejected. This is
// the mutation pairing for the anti-bluff readiness evidence — a port being open
// (or even a 200 response) is NOT accepted as readiness.
func TestReadsEtcdHealth_RejectsUnhealthy(t *testing.T) {
	srv := &http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"health":"false","reason":"NOSPACE"}`)
	})
	srv.Handler = mux
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsEtcdHealth(ctx, port)
	if ok {
		t.Fatalf("readsEtcdHealth accepted an unhealthy etcd response")
	}
	if err == nil {
		t.Fatalf("expected an error describing the unhealthy response")
	}
}

// TestReadsEtcdHealth_AcceptsHealthy proves the positive path: a server that
// reports {"health":"true"} on /health is accepted. Paired with the rejection
// test above, this shows the check discriminates correctly.
func TestReadsEtcdHealth_AcceptsHealthy(t *testing.T) {
	srv := &http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"health":"true","reason":""}`)
	})
	srv.Handler = mux
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, err := readsEtcdHealth(ctx, port)
	if err != nil {
		t.Fatalf("readsEtcdHealth errored on a valid healthy response: %v", err)
	}
	if !ok {
		t.Fatalf("readsEtcdHealth rejected a valid healthy response")
	}
}

// TestApplyEtcdOptions verifies the shared Option surface maps onto an
// etcdConfig: defaults are etcd-specific, and overrides are adopted.
func TestApplyEtcdOptions(t *testing.T) {
	def := applyEtcdOptions(nil)
	if def.image != etcdImage {
		t.Fatalf("default image = %q, want %q", def.image, etcdImage)
	}
	if def.memoryLimit != etcdDefaultMemoryLimit {
		t.Fatalf("default memory = %q, want %q", def.memoryLimit, etcdDefaultMemoryLimit)
	}
	if def.hostPort != "" {
		t.Fatalf("default hostPort = %q, want empty", def.hostPort)
	}

	cfg := applyEtcdOptions([]Option{
		WithImage("quay.io/coreos/etcd:v3.5.16"),
		WithMemoryLimit("128m"),
		WithHostPort("12379"),
	})
	if cfg.image != "quay.io/coreos/etcd:v3.5.16" {
		t.Fatalf("override image = %q", cfg.image)
	}
	if cfg.memoryLimit != "128m" {
		t.Fatalf("override memory = %q", cfg.memoryLimit)
	}
	if cfg.hostPort != "12379" {
		t.Fatalf("override hostPort = %q", cfg.hostPort)
	}
}

// --- test helpers ---

func getHealth(t *testing.T, endpoint string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s/health: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		t.Fatalf("read /health body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status %d: %s", resp.StatusCode, string(body))
	}
	return string(body)
}

// etcdContainerNameFor finds the brokertest etcd container that published the
// given host port, via pkg/runtime.List. This lets the test assert removal
// without the package exposing its internal name.
func etcdContainerNameFor(t *testing.T, rt runtime.ContainerRuntime, hostPort string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := rt.List(ctx, runtime.ListFilter{All: true})
	if err != nil {
		t.Fatalf("runtime.List: %v", err)
	}
	for _, in := range infos {
		if !strings.Contains(in.Name, "brokertest-etcd-") {
			continue
		}
		st, err := rt.Status(ctx, in.Name)
		if err != nil {
			continue
		}
		if etcdClientHostPort(st) == hostPort {
			return in.Name
		}
	}
	t.Fatalf("could not locate brokertest etcd container publishing host port %s", hostPort)
	return ""
}
