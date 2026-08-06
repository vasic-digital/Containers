// HXC-218 — the pkg/endpoint half of the IPv6 authority-bracketing class.
//
// ResolveHostPort and ResolvedURL are exported with no in-module callers: they
// exist for consuming projects, which dial or fetch whatever they return. An
// IPv6 endpoint therefore produced an authority the resolver rejects, at the
// module's public surface.
//
// The load-bearing negative case here is IsLocalEndpoint. It compares the RAW
// host against "::1", so the repair had to be applied at the JOIN sites and
// never to ServiceEndpoint.Host itself — bracketing the stored host would
// silently stop the loopback being recognised as local, trading one defect for
// a quieter one.
//
//	RED_MODE=1  assert the defect is PRESENT (PASSes only on the pre-fix artifact)
//	RED_MODE=0  assert the defect is ABSENT  (standing regression guard) [default]
package endpoint

import (
	"net"
	"net/url"
	"os"
	"testing"
)

func hxc218RedMode() bool { return os.Getenv("RED_MODE") == "1" }

// TestHXC218_ResolveHostPort pins the dial string the module hands to consumers.
func TestHXC218_ResolveHostPort(t *testing.T) {
	tests := []struct {
		name     string
		ep       ServiceEndpoint
		want     string
		repaired bool
		why      string
	}{
		{
			name: "ipv4_unchanged",
			ep:   ServiceEndpoint{Host: "127.0.0.1", Port: "9000"},
			want: "127.0.0.1:9000",
			why:  "an IPv4 literal must pass through untouched",
		},
		{
			name: "hostname_unchanged",
			ep:   ServiceEndpoint{Host: "sonar.internal", Port: "9000"},
			want: "sonar.internal:9000",
			why:  "a hostname must never be bracketed",
		},
		{
			name: "empty_host_defaults_to_localhost",
			ep:   ServiceEndpoint{Port: "9000"},
			want: "localhost:9000",
			why:  "the documented empty-host default must be preserved",
		},
		{
			name: "empty_port_returns_bare_host",
			ep:   ServiceEndpoint{Host: "localhost"},
			want: "localhost",
			why:  "the documented empty-port contract must be preserved",
		},
		{
			name:     "ipv6_bracketed",
			ep:       ServiceEndpoint{Host: "::1", Port: "9000"},
			want:     "[::1]:9000",
			repaired: true,
			why:      "consumers dial this string; unbracketed it cannot resolve",
		},
		{
			name:     "ipv6_already_bracketed_not_doubled",
			ep:       ServiceEndpoint{Host: "[::1]", Port: "9000"},
			want:     "[::1]:9000",
			repaired: true,
			why:      "a host supplied already-bracketed must not become [[::1]]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hxc218RedMode() && tt.repaired {
				t.Skipf("SKIP-OK: RED_MODE=1 — %q is a repaired case, asserted "+
					"in the GREEN polarity", tt.name)
			}
			ep := tt.ep
			if got := ResolveHostPort(&ep); got != tt.want {
				t.Fatalf("ResolveHostPort(Host=%q, Port=%q) = %q, want %q — %s",
					tt.ep.Host, tt.ep.Port, got, tt.want, tt.why)
			}
		})
	}
}

// TestHXC218_ResolveHostPort_IPv6IsResolvable is the sink-side assertion: the
// returned string must be one the resolver actually accepts. Comparing to a
// literal alone would pass even if both sides were wrong in the same way.
func TestHXC218_ResolveHostPort_IPv6IsResolvable(t *testing.T) {
	ep := ServiceEndpoint{Host: "::1", Port: "9000"}
	got := ResolveHostPort(&ep)

	_, err := net.ResolveTCPAddr("tcp", got)

	if hxc218RedMode() {
		if err == nil {
			t.Fatalf("RED_MODE=1 expected the pre-fix artifact to emit an "+
				"unresolvable authority, but %q resolved", got)
		}
		t.Logf("RED reproduced on the pre-fix artifact: ResolveHostPort emitted "+
			"%q, which the resolver rejects: %v", got, err)
		return
	}

	if err != nil {
		t.Fatalf("ResolveHostPort emitted %q, which the resolver rejects: %v", got, err)
	}
}

// TestHXC218_ResolvedURL pins the URL built for an endpoint without an explicit
// URL, including the both-branches case where the port is absent.
func TestHXC218_ResolvedURL(t *testing.T) {
	tests := []struct {
		name     string
		ep       ServiceEndpoint
		want     string
		repaired bool
		why      string
	}{
		{
			name: "explicit_url_verbatim",
			ep:   ServiceEndpoint{URL: "http://localhost:8080/base", Host: "::1", Port: "1"},
			want: "http://localhost:8080/base",
			why:  "an explicit URL wins and must never be recomposed or bracketed",
		},
		{
			name: "ipv4_unchanged",
			ep:   ServiceEndpoint{Host: "127.0.0.1", Port: "9000"},
			want: "http://127.0.0.1:9000",
			why:  "an IPv4 literal must pass through untouched",
		},
		{
			name: "hostname_unchanged",
			ep:   ServiceEndpoint{Host: "sonar.internal", Port: "9000"},
			want: "http://sonar.internal:9000",
			why:  "a hostname must never be bracketed",
		},
		{
			name:     "ipv6_with_port_bracketed",
			ep:       ServiceEndpoint{Host: "::1", Port: "9000"},
			want:     "http://[::1]:9000",
			repaired: true,
			why:      "the authority must be bracketed for url.Parse to be reliable",
		},
		{
			name:     "ipv6_without_port_bracketed",
			ep:       ServiceEndpoint{Host: "2001:db8::1"},
			want:     "http://[2001:db8::1]",
			repaired: true,
			why: "the no-port branch is equally malformed: url.Parse reads the " +
				"final group of an unbracketed literal as a port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hxc218RedMode() && tt.repaired {
				t.Skipf("SKIP-OK: RED_MODE=1 — %q is a repaired case, asserted "+
					"in the GREEN polarity", tt.name)
			}
			ep := tt.ep
			got := ep.ResolvedURL()
			if got != tt.want {
				t.Fatalf("ResolvedURL(Host=%q, Port=%q, URL=%q) = %q, want %q — %s",
					tt.ep.Host, tt.ep.Port, tt.ep.URL, got, tt.want, tt.why)
			}
			if !tt.repaired {
				return
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q) failed: %v", got, err)
			}
			if u.Hostname() != tt.ep.Host {
				t.Fatalf("url.Parse(%q).Hostname() = %q, want %q",
					got, u.Hostname(), tt.ep.Host)
			}
		})
	}
}

// TestHXC218_ResolveHealthURL_IPv6 covers the composed path: the health URL is
// the base URL plus the health path, so a malformed base propagates.
func TestHXC218_ResolveHealthURL_IPv6(t *testing.T) {
	if hxc218RedMode() {
		t.Skip("SKIP-OK: RED_MODE=1 — repaired case, asserted in the GREEN polarity")
	}
	ep := ServiceEndpoint{Host: "::1", Port: "9000", HealthPath: "api/health"}
	got := ResolveHealthURL(&ep)
	want := "http://[::1]:9000/api/health"
	if got != want {
		t.Fatalf("ResolveHealthURL = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q) failed: %v", got, err)
	}
	if _, err := net.ResolveTCPAddr("tcp", u.Host); err != nil {
		t.Fatalf("health URL authority %q is not resolvable: %v", u.Host, err)
	}
}

// TestHXC218_IsLocalEndpoint_UnaffectedByBracketing is the guard against the
// tempting-but-wrong repair. IsLocalEndpoint compares the raw host against
// "::1"; had the fix bracketed ServiceEndpoint.Host at the source, the loopback
// would have stopped being recognised as local and every local-vs-remote
// decision downstream would have silently flipped.
func TestHXC218_IsLocalEndpoint_UnaffectedByBracketing(t *testing.T) {
	tests := []struct {
		name string
		ep   ServiceEndpoint
		want bool
		why  string
	}{
		{
			name: "ipv6_loopback_is_local",
			ep:   ServiceEndpoint{Host: "::1", Port: "9000"},
			want: true,
			why: "resolver.go compares h == \"::1\" — the repair must not have " +
				"bracketed the stored host",
		},
		{
			name: "ipv4_loopback_is_local",
			ep:   ServiceEndpoint{Host: "127.0.0.1"},
			want: true,
			why:  "the IPv4 loopback must keep being recognised",
		},
		{
			name: "localhost_is_local",
			ep:   ServiceEndpoint{Host: "localhost"},
			want: true,
			why:  "the hostname form must keep being recognised",
		},
		{
			name: "empty_host_is_local",
			ep:   ServiceEndpoint{},
			want: true,
			why:  "an unset host defaults to local",
		},
		{
			name: "remote_flag_wins",
			ep:   ServiceEndpoint{Host: "::1", Remote: true},
			want: false,
			why:  "an explicit Remote flag must still override the host check",
		},
		{
			name: "routable_ipv6_is_not_local",
			ep:   ServiceEndpoint{Host: "2001:db8::1"},
			want: false,
			why:  "only the loopback is local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := tt.ep
			if got := IsLocalEndpoint(&ep); got != tt.want {
				t.Fatalf("IsLocalEndpoint(Host=%q, Remote=%v) = %v, want %v — %s",
					tt.ep.Host, tt.ep.Remote, got, tt.want, tt.why)
			}
		})
	}
}

// TestHXC218_ResolveHostPort_DoesNotMutateHost proves the repair is confined to
// the returned string. If ResolveHostPort ever wrote a bracketed value back
// into the endpoint, IsLocalEndpoint would break on the NEXT call rather than
// the first — a failure mode a single-call test cannot see.
func TestHXC218_ResolveHostPort_DoesNotMutateHost(t *testing.T) {
	ep := ServiceEndpoint{Host: "::1", Port: "9000"}

	_ = ResolveHostPort(&ep)
	_ = ep.ResolvedURL()
	_ = ResolveHealthURL(&ep)

	if ep.Host != "::1" {
		t.Fatalf("ServiceEndpoint.Host was mutated to %q; it must stay %q so the "+
			"IsLocalEndpoint comparison keeps working", ep.Host, "::1")
	}
	if !IsLocalEndpoint(&ep) {
		t.Fatalf("IsLocalEndpoint went false after resolving — the stored host " +
			"was perturbed by a resolve call")
	}
}
