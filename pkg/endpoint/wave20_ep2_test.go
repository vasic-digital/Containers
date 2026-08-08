package endpoint

// Wave-20 DEEPER (§11.4.118) second-pass CT-HARDEN-EP guard suite.
//
// HONEST BOUNDARY (§11.4.107): these are PURE, in-process table tests
// (§11.4.27 — no container/network/infra). They prove host/port/URL
// ASSEMBLY logic the FIRST pass (EP-1..EP-4) missed:
//   * EP2-1 an ALREADY-bracketed IPv6 literal ("[::1]") — the URL-canonical
//     form — must NOT be double-wrapped by net.JoinHostPort into an invalid
//     "[[::1]]:port"; it must assemble identically to the bare "::1" form and
//     stay url.Parse-able. EP-1 only handled the bare form.
//   * EP2-2 IsLocalEndpoint must recognise the bracketed loopback "[::1]" as
//     local, exactly like the bare "::1" (a classification consistency bug).
// They do NOT assert live reachability of any endpoint.

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EP2-1 — an already-bracketed IPv6 host ("[::1]") MUST assemble to exactly
// one bracket layer and stay url.Parse-able, both via ResolveHostPort and via
// ResolvedURL. The pre-fix double-wrap "[[::1]]:8080" / "http://[[::1]]:8080"
// is rejected by url.Parse ("invalid IP-literal").
func TestWave20_EP2_BracketedIPv6NotDoubleWrapped(t *testing.T) {
	hostPortTests := []struct {
		name string
		host string
		port string
		want string
	}{
		// bracketed IPv6 — the URL-canonical form EP-1 missed.
		{"bracketed loopback", "[::1]", "8080", "[::1]:8080"},
		{"bracketed full", "[2001:db8::1]", "443", "[2001:db8::1]:443"},
		// negative controls — bare/IPv4/hostname assembly is unchanged.
		{"bare loopback unchanged", "::1", "8080", "[::1]:8080"},
		{"ipv4 unchanged", "127.0.0.1", "8080", "127.0.0.1:8080"},
		{"hostname unchanged", "db.local", "5432", "db.local:5432"},
	}
	for _, tc := range hostPortTests {
		t.Run("hostport/"+tc.name, func(t *testing.T) {
			ep := &ServiceEndpoint{Host: tc.host, Port: tc.port}
			assert.Equal(t, tc.want, ResolveHostPort(ep))
		})
	}

	urlTests := []struct {
		name     string
		host     string
		port     string
		wantURL  string
		wantHost string
	}{
		{"bracketed loopback url", "[::1]", "8080", "http://[::1]:8080", "[::1]:8080"},
		{"bracketed full url", "[2001:db8::1]", "443", "http://[2001:db8::1]:443", "[2001:db8::1]:443"},
		{"bracketed https-in-host", "https://[fe80::1]", "9090", "https://[fe80::1]:9090", "[fe80::1]:9090"},
		// negative control — bare form still parses and matches.
		{"bare loopback url", "::1", "8080", "http://[::1]:8080", "[::1]:8080"},
	}
	for _, tc := range urlTests {
		t.Run("url/"+tc.name, func(t *testing.T) {
			ep := &ServiceEndpoint{Host: tc.host, Port: tc.port}
			got := ep.ResolvedURL()
			assert.Equal(t, tc.wantURL, got)

			u, err := url.Parse(got)
			require.NoError(t, err,
				"ResolvedURL(%q) must be url.Parse-able (double-bracket is not)", got)
			assert.Equal(t, tc.wantHost, u.Host)
			assert.Equal(t, tc.port, u.Port())
		})
	}
}

// EP2-2 — IsLocalEndpoint MUST treat the bracketed loopback "[::1]" as local,
// exactly like the bare "::1" form. Pre-fix the bracketed form was compared
// against the bare literal and mis-classified as non-local.
func TestWave20_EP2_IsLocalEndpoint_BracketedLoopback(t *testing.T) {
	tests := []struct {
		name   string
		ep     ServiceEndpoint
		expect bool
	}{
		{"bracketed loopback is local", ServiceEndpoint{Host: "[::1]"}, true},
		// negative controls.
		{"bare loopback is local", ServiceEndpoint{Host: "::1"}, true},
		{"bracketed non-loopback is not local", ServiceEndpoint{Host: "[fe80::1]"}, false},
		{"bracketed loopback + remote overrides", ServiceEndpoint{Host: "[::1]", Remote: true}, false},
		{"ipv4 loopback is local", ServiceEndpoint{Host: "127.0.0.1"}, true},
		{"external host not local", ServiceEndpoint{Host: "db.prod.internal"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, IsLocalEndpoint(&tc.ep))
		})
	}
}
