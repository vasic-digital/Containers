package egress

// Wave-20 CT-HARDEN-EG-HARD anti-bluff guards (§11.4.115 GREEN-polarity, committed
// default = GUARD). Each guard proves a real hardening via the injected process
// seam (execCommand) + httptest — deterministic, no network.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the STATUS-gate, the
// child-death-surfacing, and the readiness LOGIC through the injected runner
// seam + loopback httptest servers. They do NOT exercise a live ssh tunnel or a
// real VPN host (§11.4.27 — no real ssh in tests). The ssh child is replaced by
// a local `sh -c` fake whose exit/stderr/liveness the test controls.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// fakeSSHChild swaps the process seam so TunnelUp starts `sh -c <script>` instead
// of a real ssh. Returns a restore func.
func fakeSSHChild(script string) (restore func()) {
	prev := execCommand
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", script)
	}
	return func() { execCommand = prev }
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// heldLoopbackListener binds a port and keeps it dialable for the whole test (a
// FOREIGN listener the ssh child never established). Drains accepts so connects
// complete.
func heldLoopbackListener(t *testing.T) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("held listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-done; _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { close(done); _ = ln.Close() }
}

// ---------------------------------------------------------------------------
// EG-1 (a): a fast-failing ssh child (VPN host down / auth / bind) surfaces the
// child's REAL exit cause, NOT the generic readiness timeout, and does NOT wait
// out the whole readyTimeout.
// ---------------------------------------------------------------------------

func TestWave20_EG1_ChildExitSurfacedNotTimeout(t *testing.T) {
	restore := fakeSSHChild(`echo 'ssh: connect to host vpnhost port 22: Connection refused' >&2; exit 255`)
	defer restore()

	port := freeLoopbackPort(t) // nothing listens here -> the port never dials
	ctx := context.Background()
	start := time.Now()
	tun, err := TunnelUp(ctx, Options{VPNHost: "vpnhost", LocalPort: port, ConnectTimeout: 2 * time.Second}, logging.NopLogger{})
	elapsed := time.Since(start)
	if tun != nil {
		_ = tun.Down()
	}
	if err == nil {
		t.Fatal("EG-1(a): a dead ssh child returned a ready tunnel, want error")
	}
	if strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("EG-1(a): got the generic readiness-timeout error, real ssh cause discarded: %v", err)
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("EG-1(a): error does not carry the ssh child's stderr cause: %v", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("EG-1(a): TunnelUp burned %v (~the full timeout) instead of breaking on child death", elapsed)
	}
	t.Logf("EG-1(a) child-exit error in %v (expected): %v", elapsed, err)
}

// ---------------------------------------------------------------------------
// EG-1 (b): a FOREIGN pre-bound listener makes the SOCKS port dial successfully,
// but our ssh child already died -> TunnelUp MUST return an error (never route
// traffic through a proxy it never established).
// ---------------------------------------------------------------------------

func TestWave20_EG1_DeadChildBehindForeignListener(t *testing.T) {
	restore := fakeSSHChild(`echo 'ssh: bind: Address already in use' >&2; exit 255`)
	defer restore()

	port, stop := heldLoopbackListener(t)
	defer stop()

	ctx := context.Background()
	tun, err := TunnelUp(ctx, Options{VPNHost: "vpnhost", LocalPort: port, ConnectTimeout: 2 * time.Second}, logging.NopLogger{})
	if tun != nil {
		_ = tun.Down()
	}
	if err == nil {
		t.Fatal("EG-1(b): foreign-listener + dead child reported ready, want error (false positive)")
	}
	t.Logf("EG-1(b) foreign-listener+dead-child error (expected): %v", err)
}

// ---------------------------------------------------------------------------
// EG-4: Down is concurrency-safe + clean-idempotent — many concurrent Down()
// calls all return nil, the ssh child is reaped exactly once, and t.cmd is niled
// post-teardown. Uses a held listener + a live child so TunnelUp genuinely
// succeeds first.
// ---------------------------------------------------------------------------

func TestWave20_EG4_DownConcurrentIdempotent(t *testing.T) {
	restore := fakeSSHChild(`sleep 5`) // live child; Down kills it
	defer restore()

	port, stop := heldLoopbackListener(t)
	defer stop()

	ctx := context.Background()
	tun, err := TunnelUp(ctx, Options{VPNHost: "vpnhost", LocalPort: port, ConnectTimeout: 3 * time.Second}, logging.NopLogger{})
	if err != nil {
		t.Fatalf("EG-4 setup: TunnelUp should succeed (held listener + live child): %v", err)
	}

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if e := tun.Down(); e != nil {
				t.Errorf("EG-4: concurrent Down returned %v, want nil", e)
			}
		}()
	}
	wg.Wait()

	tun.mu.Lock()
	c := tun.cmd
	tun.mu.Unlock()
	if c != nil {
		t.Errorf("EG-4: t.cmd not niled after teardown (leaked child handle)")
	}
}

// ---------------------------------------------------------------------------
// EG-2: a proxied IP-echo that is BLOCKED (403 "Access Denied", or a non-2xx
// whose body is coincidentally IP-shaped, or a 200 HTML soft-block) MUST become
// an ERROR — never a bogus "successful" egress IP. Positive control: a bare IP
// still succeeds.
// ---------------------------------------------------------------------------

func TestWave20_EG2_BlockedIPEchoIsError(t *testing.T) {
	ctx := context.Background()

	serve := func(code int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if code != 200 {
				w.WriteHeader(code)
			}
			_, _ = io.WriteString(w, body)
		}))
	}

	// 403 "Access Denied" — the DPI/MITM block this package exists to diagnose.
	blocked := serve(http.StatusForbidden, "Access Denied")
	defer blocked.Close()
	ip, err := DirectEgressIP(ctx, blocked.URL)
	if err == nil {
		t.Fatalf("EG-2: 403-blocked IP-echo returned success ip=%q, want error", ip)
	}
	if strings.Contains(ip, "Access Denied") {
		t.Fatalf("EG-2: returned the block body as an egress IP: %q", ip)
	}
	t.Logf("EG-2 403 error (expected): %v", err)

	// Non-2xx whose body is coincidentally a valid IP (captive portal / MITM
	// echoing its own address). ONLY the status gate catches this one.
	weird := serve(http.StatusForbidden, "10.0.0.1")
	defer weird.Close()
	if _, err := DirectEgressIP(ctx, weird.URL); err == nil {
		t.Fatal("EG-2: 403 with IP-shaped body returned success — status gate missing")
	}

	// 200 with an HTML soft-block body — not an IP. ONLY the IP-validation
	// catches this one.
	html := serve(200, "<html>blocked</html>")
	defer html.Close()
	if _, err := DirectEgressIP(ctx, html.URL); err == nil {
		t.Fatal("EG-2: 200 non-IP body returned success — IP validation missing")
	}

	// Positive control: a real bare IP echo still succeeds.
	ok := serve(200, "198.51.100.9\n")
	defer ok.Close()
	got, err := DirectEgressIP(ctx, ok.URL)
	if err != nil {
		t.Fatalf("EG-2 positive control errored: %v", err)
	}
	if got != "198.51.100.9" {
		t.Errorf("EG-2 positive control ip=%q, want 198.51.100.9", got)
	}
}

// ---------------------------------------------------------------------------
// EG-3: DirectEgressIP's one-shot transport must NOT keep the connection alive
// (it is never reused) — a kept-alive idle conn + its readLoop goroutine leak.
// Proven by observing the server never sees the connection enter StateIdle.
// ---------------------------------------------------------------------------

func TestWave20_EG3_DirectEgressIPNoKeepAliveLeak(t *testing.T) {
	var mu sync.Mutex
	idleSeen := false

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.7")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateIdle {
			mu.Lock()
			idleSeen = true
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	ctx := context.Background()
	ip, err := DirectEgressIP(ctx, srv.URL)
	if err != nil {
		t.Fatalf("EG-3: DirectEgressIP: %v", err)
	}
	if ip != "203.0.113.7" {
		t.Fatalf("EG-3: ip=%q", ip)
	}
	// Let the server's post-response ConnState callback fire.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	seen := idleSeen
	mu.Unlock()
	if seen {
		t.Errorf("EG-3: server observed a kept-alive idle connection after DirectEgressIP — one-shot transport leaked a conn (DisableKeepAlives missing)")
	}
}
