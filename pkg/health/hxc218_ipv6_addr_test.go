// HXC-218 — the containers half of the IPv6 authority-bracketing class.
//
// # What was actually measured (go1.26.4; probe output kept with the ticket)
//
// The two library layers this package leans on do NOT agree, and that
// disagreement is the whole shape of this defect:
//
//	net.ResolveTCPAddr("tcp", "::1:8080")   -> address ::1:8080: too many colons
//	net.ResolveTCPAddr("tcp", "[::1]:8080") -> resolves
//
//	url.Parse("http://::1:8080/x")   -> Host="::1:8080" Hostname="::1" Port="8080"
//	url.Parse("http://fe80::abcd/x") -> invalid port ":abcd" after host
//
// url.Parse is LENIENT: it splits the authority at the LAST colon, so an
// unbracketed IPv6 authority still yields the right Hostname/Port whenever a
// numeric port is present, and net/http then dials the bracketed form itself.
// The resolver is NOT lenient and rejects the same string outright.
//
// So the two sites in this package fail differently, and the assertions below
// are pinned to what each one actually does rather than assuming a shared
// failure mode:
//
//	types.go  ResolvedAddress() -> dialled by CheckTCP/CheckGRPC
//	                            -> HARD FAILURE: a reachable IPv6 socket is
//	                               reported unhealthy.
//	http.go   URL composition   -> parsed by net/http
//	                            -> the probe SUCCEEDS by leniency, but the URL
//	                               emitted into Details["url"] is malformed: its
//	                               authority is one the resolver rejects, and it
//	                               fails url.Parse outright once the port is
//	                               absent or non-numeric.
//
// Claiming the HTTP probe is broken would be a bluff — it is not. Claiming the
// URL it publishes is well-formed would equally be a bluff — it is not.
//
// # Polarity switch (§11.4.115)
//
//	RED_MODE=1  assert the defect is PRESENT (PASSes only on the pre-fix artifact)
//	RED_MODE=0  assert the defect is ABSENT  (standing regression guard) [default]
//
// The positive cases drive a REAL httptest server and a REAL net.Listener bound
// to the IPv6 loopback, through the REAL production check functions. Nothing
// here re-implements the join, so neither polarity is a replica of the code it
// guards.
package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// hxc218RedMode reports whether the guard runs in reproduce-the-defect polarity.
func hxc218RedMode() bool { return os.Getenv("RED_MODE") == "1" }

// hxc218IPv6Loopback returns a listener bound to the IPv6 loopback, or skips
// with a reason when the host genuinely has no IPv6 loopback (§11.4.3 — an
// absent capability is an honest SKIP, never a silent PASS).
func hxc218IPv6Loopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("SKIP-OK: hardware_not_present — no IPv6 loopback on this host: %v", err)
	}
	return ln
}

// hxc218SplitPort returns the port of a listener address. The host half is
// deliberately discarded: net.SplitHostPort strips the brackets, which is one
// of the two shapes a host legitimately arrives in.
func hxc218SplitPort(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) failed: %v", addr, err)
	}
	return port
}

// TestHXC218_CheckHTTP_IPv6EmitsResolvableURL probes a REAL HTTP server on the
// IPv6 loopback exactly the way BootManager.HealthCheckAll probes one — Host
// and Port set, URL empty — and asserts on the URL the checker publishes.
//
// Reachability is asserted identically in BOTH polarities on purpose: net/http
// reaches the server either way, so making reachability the RED signal would
// produce a test that cannot fail on the broken artifact. The signal that
// genuinely differs between artifacts is whether the published authority is one
// the resolver accepts.
func TestHXC218_CheckHTTP_IPv6EmitsResolvableURL(t *testing.T) {
	ln := hxc218IPv6Loopback(t)

	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/health" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		)},
	}
	srv.Start()
	defer srv.Close()

	port := hxc218SplitPort(t, ln.Addr().String())

	// The start-path shape: bare IPv6 host, no URL override.
	target := HealthTarget{
		Name:    "hxc218-http",
		Host:    "::1",
		Port:    port,
		Path:    "/api/health",
		Type:    HealthHTTP,
		Timeout: 5 * time.Second,
	}

	res := CheckHTTP(context.Background(), target)

	// Invariant in both polarities: net/http's leniency means the probe itself
	// succeeds. If this ever fails, the defect is worse than measured.
	if !res.Healthy {
		t.Fatalf("CheckHTTP could not reach a REACHABLE IPv6 server at %q: %s",
			ln.Addr().String(), res.Error)
	}

	got := res.Details["url"]

	if hxc218RedMode() {
		unbracketed := "http://::1:" + port + "/api/health"
		if got != unbracketed {
			t.Fatalf("RED_MODE=1 expected the pre-fix artifact to publish the "+
				"unbracketed URL %q, got %q — the defect is not present, so this "+
				"is not a valid RED baseline", unbracketed, got)
		}
		// Prove the published authority is genuinely malformed, not merely
		// cosmetically odd: the resolver rejects it.
		u, err := url.Parse(got)
		if err != nil {
			t.Logf("RED reproduced on the pre-fix artifact: published URL %q "+
				"does not parse: %v", got, err)
			return
		}
		if _, err := net.ResolveTCPAddr("tcp", u.Host); err == nil {
			t.Fatalf("RED_MODE=1 expected the published authority %q to be "+
				"rejected by the resolver, but it resolved", u.Host)
		} else {
			t.Logf("RED reproduced on the pre-fix artifact: published URL %q "+
				"carries authority %q, which the resolver rejects: %v",
				got, u.Host, err)
		}
		return
	}

	want := "http://[::1]:" + port + "/api/health"
	if got != want {
		t.Fatalf("published URL = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q) failed: %v — the published URL is unusable", got, err)
	}
	if u.Hostname() != "::1" {
		t.Fatalf("url.Parse(%q).Hostname() = %q, want %q", got, u.Hostname(), "::1")
	}
	if u.Port() != port {
		t.Fatalf("url.Parse(%q).Port() = %q, want %q", got, u.Port(), port)
	}
	// The repaired authority must be one the resolver accepts — precisely the
	// property the pre-fix artifact lacked.
	if _, err := net.ResolveTCPAddr("tcp", u.Host); err != nil {
		t.Fatalf("published authority %q is not resolvable: %v", u.Host, err)
	}
}

// TestHXC218_CheckTCP_IPv6HostWithoutURL covers the hard-failure site:
// ResolvedAddress is what CheckTCP and CheckGRPC dial, so an IPv6 endpoint
// declared with HealthType "tcp" — the shape a database endpoint uses — cannot
// be probed at all before the fix.
func TestHXC218_CheckTCP_IPv6HostWithoutURL(t *testing.T) {
	ln := hxc218IPv6Loopback(t)
	defer func() { _ = ln.Close() }()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	port := hxc218SplitPort(t, ln.Addr().String())

	target := HealthTarget{
		Name:    "hxc218-tcp",
		Host:    "::1",
		Port:    port,
		Type:    HealthTCP,
		Timeout: 5 * time.Second,
	}

	res := CheckTCP(context.Background(), target)

	if hxc218RedMode() {
		if res.Healthy {
			t.Fatalf("RED_MODE=1 expected the pre-fix artifact to FAIL the dial "+
				"of a LISTENING IPv6 socket, but it reported healthy (%+v)", res)
		}
		t.Logf("RED reproduced on the pre-fix artifact: a LISTENING IPv6 socket "+
			"at %q is reported unhealthy — %s", ln.Addr().String(), res.Error)
		return
	}

	if !res.Healthy {
		t.Fatalf("CheckTCP could not dial a LISTENING IPv6 socket at %q: %s",
			ln.Addr().String(), res.Error)
	}
	if got, want := res.Details["address"], net.JoinHostPort("::1", port); got != want {
		t.Fatalf("dialled address = %q, want %q", got, want)
	}
}

// TestHXC218_ResolvedAddress_NegativeCases pins what MUST NOT change.
//
// A guard that only asserted the IPv6 happy path would not have caught either
// trap this ticket family has already hit: (a) applying the join to a value that
// is ALREADY a full URL corrupts working code, and (b) applying it to a host
// that is ALREADY bracketed produces "[[::1]]", which the resolver rejects just
// as hard as the unbracketed form.
func TestHXC218_ResolvedAddress_NegativeCases(t *testing.T) {
	tests := []struct {
		name     string
		target   HealthTarget
		want     string
		repaired bool // differs between artifacts; assert in GREEN polarity only
		why      string
	}{
		{
			name:   "ipv4_literal_unchanged",
			target: HealthTarget{Host: "127.0.0.1", Port: "9000"},
			want:   "127.0.0.1:9000",
			why:    "an IPv4 literal carries no colon and must pass through untouched",
		},
		{
			name:   "hostname_unchanged",
			target: HealthTarget{Host: "sonar.internal", Port: "9000"},
			want:   "sonar.internal:9000",
			why:    "a hostname must never be bracketed",
		},
		{
			name:   "localhost_unchanged",
			target: HealthTarget{Host: "localhost", Port: "5432"},
			want:   "localhost:5432",
			why:    "the common default must not be perturbed",
		},
		{
			name:   "empty_host_and_port_unchanged",
			target: HealthTarget{},
			want:   "",
			why:    "the documented empty-target contract must be preserved",
		},
		{
			name:   "empty_port_keeps_trailing_separator",
			target: HealthTarget{Host: "localhost"},
			want:   "localhost:",
			why: "the pre-fix Sprintf emitted a trailing colon for an empty port; " +
				"the repair must not silently change that shape",
		},
		{
			name:   "full_url_returned_verbatim",
			target: HealthTarget{URL: "http://localhost:8080/healthz", Host: "ignored", Port: "1"},
			want:   "http://localhost:8080/healthz",
			why: "URL takes precedence and is NOT a host — joining it would yield " +
				"[http://localhost:8080/healthz]:1 and BREAK a working call site",
		},
		{
			name:     "ipv6_literal_bracketed",
			target:   HealthTarget{Host: "::1", Port: "9000"},
			want:     "[::1]:9000",
			repaired: true,
			why:      "an unbracketed IPv6 authority is rejected by the resolver",
		},
		{
			name:     "ipv6_full_form_bracketed",
			target:   HealthTarget{Host: "2001:db8::1", Port: "9000"},
			want:     "[2001:db8::1]:9000",
			repaired: true,
			why:      "a routable IPv6 literal is broken exactly like the loopback",
		},
		{
			name:     "ipv6_v4mapped_bracketed",
			target:   HealthTarget{Host: "::ffff:127.0.0.1", Port: "9000"},
			want:     "[::ffff:127.0.0.1]:9000",
			repaired: true,
			why: "the v4-mapped form reports as IPv4 via net.IP.To4 but still " +
				"carries colons, so a To4-based test would wrongly skip it",
		},
		{
			name:     "ipv6_with_zone_bracketed",
			target:   HealthTarget{Host: "fe80::1%eth0", Port: "9000"},
			want:     "[fe80::1%eth0]:9000",
			repaired: true,
			why: "net.ParseIP rejects a zone suffix, so the zone must be stripped " +
				"before the parse and preserved in the output",
		},
		{
			name:     "already_bracketed_not_double_bracketed",
			target:   HealthTarget{Host: "[::1]", Port: "9000"},
			want:     "[::1]:9000",
			repaired: true,
			why: "net.JoinHostPort brackets unconditionally on seeing a colon, so " +
				"an already-bracketed host would become [[::1]] — rejected just as " +
				"hard as the unbracketed form",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hxc218RedMode() && tt.repaired {
				t.Skipf("SKIP-OK: RED_MODE=1 — %q is a repaired case, asserted in "+
					"the GREEN polarity", tt.name)
			}
			target := tt.target
			if got := target.ResolvedAddress(); got != tt.want {
				t.Fatalf("ResolvedAddress() with Host=%q Port=%q URL=%q = %q, want %q — %s",
					tt.target.Host, tt.target.Port, tt.target.URL, got, tt.want, tt.why)
			}
		})
	}
}

// TestHXC218_CheckHTTP_ExplicitURLIsUsedVerbatim is the full-URL negative case
// at the sink: a caller that already supplies a complete URL must keep working
// byte-for-byte. This is the assertion that would fail if the fix had been
// applied indiscriminately to every host-shaped field.
func TestHXC218_CheckHTTP_ExplicitURLIsUsedVerbatim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
	))
	defer srv.Close()

	full := srv.URL + "/healthz"
	target := HealthTarget{
		Name: "hxc218-explicit-url",
		// Host and Port are deliberately populated and deliberately wrong:
		// URL must win, and must not be recomposed from them.
		Host:    "::1",
		Port:    "1",
		URL:     full,
		Type:    HealthHTTP,
		Timeout: 5 * time.Second,
	}

	res := CheckHTTP(context.Background(), target)
	if !res.Healthy {
		t.Fatalf("CheckHTTP with an explicit URL failed: %s", res.Error)
	}
	if got := res.Details["url"]; got != full {
		t.Fatalf("probed URL = %q, want the caller's URL verbatim %q — a full URL "+
			"must never be treated as a host", got, full)
	}
	if strings.Contains(res.Details["url"], "[http") {
		t.Fatalf("the caller's full URL was bracketed as if it were a host: %q",
			res.Details["url"])
	}
}
