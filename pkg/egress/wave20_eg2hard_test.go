package egress

// Wave-20 DEEPER (§11.4.118 loop-until-dry) 2nd-pass egress hardening. The first
// pass (EG-1..EG-4) proved launch-not-working false-positives, the IP-echo
// status gate, the DirectEgressIP keep-alive leak, and Down idempotency. This
// pass targets the ssh-command CONSTRUCTION + connect-bound + the package's core
// remote-DNS premise — defects EG-1..EG-4 did NOT touch.
//
// HONEST BOUNDARY (§11.4.107 / §11.4.27): every guard here is deterministic and
// hermetic — the ssh child is the injected execCommand seam (a local `sh -c`
// fake OR a spawn-counting spy), and the SOCKS/ip-echo peers are loopback
// httptest / in-test SOCKS servers. No real ssh, no VPN host, no internet.

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// ---------------------------------------------------------------------------
// EG2-1: ssh ARGUMENT injection via a destination beginning with '-'.
//
// The ssh child is a bare argv (execCommand("ssh", args...), no shell), so a
// hostile VPNHost like "-oProxyCommand=<cmd>" is NOT shell-injected — it is
// worse: ssh's own getopt parses the leading-'-' positional as an OPTION, so
// "-oProxyCommand=..." makes ssh run an arbitrary command. destination() is the
// final positional with no "--" guard. TunnelUp MUST refuse such a destination
// and MUST NOT even spawn ssh.
//
// Anti-tautology: the load-bearing assertion is that ssh was NEVER spawned
// (spawnCount==0). Revert the guard (source line
//   `if strings.HasPrefix(o.destination(), "-") {`  ->  `..., "\x00") {`)
// and TunnelUp falls through to spawn the child -> spawnCount==1 -> this FAILS.
// (`err != nil` alone does NOT distinguish: the fast-exiting fake child also
// yields a non-nil childError, so only the spawn count is discriminating.)
// ---------------------------------------------------------------------------

func TestWave20_EG2_ArgInjectionDestinationRefused(t *testing.T) {
	var spawned int32
	prev := execCommand
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		atomic.AddInt32(&spawned, 1)
		// Never launch a real ssh: a harmless fast-exiting child keeps the test
		// bounded on the reverted (guard-removed) path.
		return exec.Command("sh", "-c", "exit 0")
	}
	defer func() { execCommand = prev }()

	port := freeLoopbackPort(t) // nothing binds it — no accidental readiness
	for _, dest := range []string{
		"-oProxyCommand=touch /tmp/egress_eg2_pwned", // bare VPNHost injection
		"-lroot", // short-option injection
	} {
		atomic.StoreInt32(&spawned, 0)
		_, err := TunnelUp(context.Background(), Options{
			VPNHost:        dest,
			LocalPort:      port,
			ConnectTimeout: 2 * time.Second,
		}, logging.NopLogger{})
		if err == nil {
			t.Errorf("EG2-1: destination %q accepted (ssh argument injection), want error", dest)
		}
		if n := atomic.LoadInt32(&spawned); n != 0 {
			t.Errorf("EG2-1: ssh was SPAWNED for injection destination %q (spawned=%d) — guard bypassed", dest, n)
		}
	}

	// Also cover the User@VPNHost composition: a leading-'-' User is equally an
	// injection because destination() renders "-baduser@host".
	atomic.StoreInt32(&spawned, 0)
	_, err := TunnelUp(context.Background(), Options{
		VPNHost:        "vpnhost",
		User:           "-oProxyCommand=id",
		LocalPort:      port,
		ConnectTimeout: 2 * time.Second,
	}, logging.NopLogger{})
	if err == nil {
		t.Error("EG2-1: leading-'-' User accepted (ssh argument injection via User@host), want error")
	}
	if n := atomic.LoadInt32(&spawned); n != 0 {
		t.Errorf("EG2-1: ssh SPAWNED for leading-'-' User (spawned=%d) — guard bypassed", n)
	}

	// Positive control: a benign destination is NOT refused by the guard (it must
	// reach the spawn seam). The fake child exits fast, so err is non-nil for an
	// unrelated reason (child died) — what matters is that ssh WAS spawned.
	atomic.StoreInt32(&spawned, 0)
	_, _ = TunnelUp(context.Background(), Options{
		VPNHost:        "ops@nezha",
		LocalPort:      port,
		ConnectTimeout: 2 * time.Second,
	}, logging.NopLogger{})
	if n := atomic.LoadInt32(&spawned); n != 1 {
		t.Errorf("EG2-1 positive control: benign destination did NOT reach the spawn seam (spawned=%d, want 1)", n)
	}
}

// ---------------------------------------------------------------------------
// EG2-2: a sub-second ConnectTimeout must NOT truncate to "ConnectTimeout=0".
//
// ssh's ConnectTimeout is an integer number of seconds; ConnectTimeout=0 means
// "no explicit timeout — use the ~2-minute system TCP default", the OPPOSITE of
// a tight bound. int(d.Seconds()) truncates: 500ms -> 0. A caller asking for a
// tighter-than-1s ssh connect bound silently got an UNBOUNDED one.
//
// Pure/deterministic. Anti-tautology: revert `if connectSecs < 1 {` -> `< 0`
// (never true for the non-negative connectSecs), and 500ms renders
// "ConnectTimeout=0" -> the first assertion FAILS.
// ---------------------------------------------------------------------------

func TestWave20_EG2_ConnectTimeoutNotTruncatedToZero(t *testing.T) {
	joined := strings.Join(buildDynamicForwardArgs(Options{
		VPNHost:        "nezha",
		LocalPort:      1080,
		ConnectTimeout: 500 * time.Millisecond,
	}), " ")

	if strings.Contains(joined, "ConnectTimeout=0") {
		t.Errorf("EG2-2: sub-second ConnectTimeout truncated to ConnectTimeout=0 "+
			"(disables the ssh connect bound — reverts to the ~2min system default): %s", joined)
	}
	if !strings.Contains(joined, "ConnectTimeout=1") {
		t.Errorf("EG2-2: want a >=1s ssh ConnectTimeout for a 500ms request, got: %s", joined)
	}

	// Regression floor: the normal (multi-second + default) paths are unchanged.
	def := strings.Join(buildDynamicForwardArgs(Options{VPNHost: "h", LocalPort: 1080}), " ")
	if !strings.Contains(def, "ConnectTimeout=10") {
		t.Errorf("EG2-2: default ConnectTimeout must stay 10s: %s", def)
	}
	sec := strings.Join(buildDynamicForwardArgs(Options{VPNHost: "h", LocalPort: 1080, ConnectTimeout: 7 * time.Second}), " ")
	if !strings.Contains(sec, "ConnectTimeout=7") {
		t.Errorf("EG2-2: 7s ConnectTimeout must stay 7: %s", sec)
	}
}

// ---------------------------------------------------------------------------
// EG2-COV (enumerated coverage, §11.4.118 — NOT a defect fix): the package's
// central premise is REMOTE (proxy-side) DNS — "the local host's DNS is often
// part of the block, so this matters". The existing suite only routes Verify to
// an IP target, so the remote-DNS path (SOCKS ATYP=0x03 domainname) is never
// asserted. This guard proves Verify sends the DOMAIN to the SOCKS proxy rather
// than pre-resolving it locally: a fake, locally-UNRESOLVABLE domain still
// reaches the proxy as ATYP=0x03. If socksClient ever regressed to local DNS,
// the fake domain would fail to resolve and never reach the recorder.
// (Empirically confirmed against the Go stdlib socks5 dialer.)
// ---------------------------------------------------------------------------

func TestWave20_EG2_RemoteDNSSentToProxyNotLocalResolution(t *testing.T) {
	// Backend the recording SOCKS server forwards every "resolved" host to.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "198.51.100.7")
	}))
	defer backend.Close()
	backendAddr := strings.TrimPrefix(backend.URL, "http://")

	var mu sync.Mutex
	var gotAtyp byte
	var gotHost string

	socksAddr, stop := recordingSocks5(t, backendAddr, func(atyp byte, host string) {
		mu.Lock()
		gotAtyp, gotHost = atyp, host
		mu.Unlock()
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// A domain that does NOT resolve locally. Local-DNS would fail here first.
	const fakeDomain = "ipecho.egress-eg2.invalid"
	res, err := Verify(ctx, socksAddr, "http://"+fakeDomain+"/", nil)
	if err != nil {
		t.Fatalf("EG2-COV: Verify to a domain URL errored (remote DNS not used?): %v", err)
	}
	if res.EgressIP != "198.51.100.7" {
		t.Errorf("EG2-COV: egress IP = %q, want 198.51.100.7", res.EgressIP)
	}

	mu.Lock()
	atyp, host := gotAtyp, gotHost
	mu.Unlock()
	if atyp != 0x03 {
		t.Errorf("EG2-COV: SOCKS proxy received ATYP=0x%02x — Verify pre-resolved DNS locally instead of remote (proxy-side) resolution", atyp)
	}
	if host != fakeDomain {
		t.Errorf("EG2-COV: SOCKS proxy received host=%q, want the raw domain %q (remote DNS)", host, fakeDomain)
	}
}

// recordingSocks5 is a minimal no-auth SOCKS5 (RFC 1928) CONNECT server that
// reports the (ATYP, host) of the first request to onReq, then forwards EVERY
// request to forwardTo (so a fake/unresolvable domain still completes). Distinct
// from egress_test.go's startSocks5, which dials the requested host directly
// (a fake domain would fail there).
func recordingSocks5(t *testing.T, forwardTo string, onReq func(atyp byte, host string)) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("recordingSocks5 listen: %v", err)
	}
	var once sync.Once
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(15 * time.Second))
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil || head[0] != 0x05 {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, int(head[1]))); err != nil {
					return
				}
				if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil || req[0] != 0x05 || req[1] != 0x01 {
					return
				}
				atyp := req[3]
				var host string
				switch atyp {
				case 0x01:
					b := make([]byte, 4)
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = net.IP(b).String()
				case 0x03:
					l := make([]byte, 1)
					if _, err := io.ReadFull(c, l); err != nil {
						return
					}
					b := make([]byte, int(l[0]))
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = string(b)
				case 0x04:
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
				_ = binary.BigEndian.Uint16(portBuf)
				once.Do(func() { onReq(atyp, host) })

				upstream, err := net.Dial("tcp", forwardTo)
				if err != nil {
					_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer upstream.Close()
				if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				_ = c.SetDeadline(time.Time{})
				go func() { _, _ = io.Copy(upstream, c) }()
				_, _ = io.Copy(c, upstream)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}
