package egress

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic argument-construction test (runs everywhere).
// ---------------------------------------------------------------------------

func TestBuildDynamicForwardArgs_DynamicSocksAndHardening(t *testing.T) {
	o := Options{VPNHost: "nezha", User: "ops", LocalAddr: "127.0.0.1", LocalPort: 1080, SSHPort: 2222, KeyPath: "/k/id"}
	args := buildDynamicForwardArgs(o)
	joined := strings.Join(args, " ")

	for _, s := range []string{
		"-N",
		"-D 127.0.0.1:1080",
		"ExitOnForwardFailure=yes",
		"-p 2222",
		"-i /k/id",
		"ops@nezha",
	} {
		if !strings.Contains(joined, s) {
			t.Errorf("ssh args missing %q\ngot: %s", s, joined)
		}
	}
	// The destination MUST be the final argument (ssh positional).
	if args[len(args)-1] != "ops@nezha" {
		t.Errorf("destination not last arg: %v", args)
	}
	if strings.Contains(joined, "tail") {
		t.Errorf("ssh args unexpectedly contain tail: %s", joined)
	}
}

func TestOptions_UserNotDoubledWhenHostHasAt(t *testing.T) {
	o := Options{VPNHost: "root@nezha", User: "ignored", LocalPort: 1080}
	if got := o.destination(); got != "root@nezha" {
		t.Errorf("destination = %q, want root@nezha", got)
	}
}

// ---------------------------------------------------------------------------
// Real network test: Verify routes through a SOCKS5 proxy and the egress IP
// observed by the upstream differs from the direct egress IP. The in-test SOCKS
// server dials its targets from loopback alias 127.0.0.2, so the ipecho server
// sees 127.0.0.2 via the proxy vs 127.0.0.1 directly — a genuine, deterministic
// "egress identity changed" proof without touching the real internet.
// ---------------------------------------------------------------------------

func TestVerify_EgressIPDiffersViaProxy(t *testing.T) {
	// ipecho server: echoes the client IP it observed; /code/<n> returns status n.
	ipecho := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/code/") {
			code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/code/"))
			w.WriteHeader(code)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		_, _ = io.WriteString(w, host)
	}))
	defer ipecho.Close()

	// In-test SOCKS5 proxy, outbound source = 127.0.0.2.
	socksAddr, stopSocks := startSocks5(t, "127.0.0.2")
	defer stopSocks()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	directIP, err := DirectEgressIP(ctx, ipecho.URL)
	if err != nil {
		t.Fatalf("DirectEgressIP: %v", err)
	}

	res, err := Verify(ctx, socksAddr, ipecho.URL, []string{ipecho.URL + "/code/204"})
	if err != nil {
		t.Fatalf("Verify via proxy: %v", err)
	}

	t.Logf("direct egress IP = %s", directIP)
	t.Logf("via-proxy egress IP = %s", res.EgressIP)

	if res.EgressIP == "" {
		t.Fatal("via-proxy egress IP is empty")
	}
	// The load-bearing anti-bluff assertion: routing through the proxy changes
	// the egress identity the upstream sees. If Verify ignored the proxy (the
	// no-op/old approach), both would be 127.0.0.1 and this fails.
	if res.EgressIP == directIP {
		t.Errorf("via-proxy egress IP (%s) == direct egress IP (%s): traffic did NOT traverse the proxy",
			res.EgressIP, directIP)
	}
	if res.EgressIP != "127.0.0.2" {
		t.Errorf("via-proxy egress IP = %s, want 127.0.0.2 (the proxy's outbound source)", res.EgressIP)
	}
	// Per-target HTTP code is reported (user-observable reachability outcome).
	if got := res.Targets[ipecho.URL+"/code/204"]; got != 204 {
		t.Errorf("target status via proxy = %d, want 204", got)
	}
}

func TestVerify_DeadPortFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Port 1 is reserved/unused — connecting through it must fail, so Verify
	// MUST return an error (a dead proxy is never a successful verification).
	_, err := Verify(ctx, "127.0.0.1:1", "http://127.0.0.1:1/", []string{"http://example.invalid/"})
	if err == nil {
		t.Fatal("expected error when SOCKS proxy is dead, got nil")
	}
	t.Logf("dead-port Verify error (expected): %v", err)
}

// ---------------------------------------------------------------------------
// Minimal SOCKS5 (RFC 1928) test server: no-auth, CONNECT only. Outbound dials
// bind to fromIP so the upstream observes a distinct source address.
// ---------------------------------------------------------------------------

func startSocks5(t *testing.T, fromIP string) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks listen: %v", err)
	}
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(fromIP)},
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			go serveSocksConn(c, dialer)
		}
	}()
	return ln.Addr().String(), func() { close(done); _ = ln.Close() }
}

func serveSocksConn(c net.Conn, dialer *net.Dialer) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))

	// Greeting: VER, NMETHODS, METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil || head[0] != 0x05 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(head[1]))); err != nil {
		return
	}
	// No-auth.
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Request: VER, CMD, RSV, ATYP, ADDR, PORT.
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 0x05 || req[1] != 0x01 {
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // cmd not supported
		return
	}
	var host string
	switch req[3] {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(c, portBuf); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf)

	upstream, err := dialer.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // general failure
		return
	}
	defer upstream.Close()
	// Success reply with a zero BND.ADDR/BND.PORT.
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	_ = upstream.(*net.TCPConn).SetKeepAlive(true)

	// Pipe bidirectionally.
	go func() { _, _ = io.Copy(upstream, c) }()
	_, _ = io.Copy(c, upstream)
}
