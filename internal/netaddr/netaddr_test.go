package netaddr

import (
	"net"
	"testing"
)

// TestBracketHost is the unit-level truth table for the rule. Every row states
// why it exists, because the failure mode this package guards against is a
// plausible-looking rule that is subtly too eager (net.JoinHostPort brackets on
// any colon) or too timid (a net.IP.To4 test skips v4-mapped literals).
func TestBracketHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
		why  string
	}{
		{
			name: "empty",
			host: "",
			want: "",
			why:  "an empty host has nothing to bracket",
		},
		{
			name: "ipv4",
			host: "127.0.0.1",
			want: "127.0.0.1",
			why:  "an IPv4 literal has no colon and must be untouched",
		},
		{
			name: "hostname",
			host: "sonar.internal",
			want: "sonar.internal",
			why:  "a hostname must never be bracketed",
		},
		{
			name: "localhost",
			host: "localhost",
			want: "localhost",
			why:  "the common default must be untouched",
		},
		{
			name: "ipv6_loopback",
			host: "::1",
			want: "[::1]",
			why:  "the whole point of the package",
		},
		{
			name: "ipv6_routable",
			host: "2001:db8::1",
			want: "[2001:db8::1]",
			why:  "a full-form literal is broken exactly like the loopback",
		},
		{
			name: "ipv6_v4mapped",
			host: "::ffff:127.0.0.1",
			want: "[::ffff:127.0.0.1]",
			why: "net.IP.To4 reports this as v4, so a To4-based rule would skip " +
				"it — but its colons break an authority all the same",
		},
		{
			name: "ipv6_with_zone",
			host: "fe80::1%eth0",
			want: "[fe80::1%eth0]",
			why:  "the zone must be stripped before ParseIP and preserved after",
		},
		{
			name: "ipv6_already_bracketed",
			host: "[::1]",
			want: "[::1]",
			why:  "double bracketing is rejected as hard as no bracketing",
		},
		{
			name: "ipv6_already_bracketed_with_zone",
			host: "[fe80::1%eth0]",
			want: "[fe80::1%eth0]",
			why:  "the bracketed-input short circuit must cover the zone form too",
		},
		{
			name: "full_http_url",
			host: "http://localhost:8080",
			want: "http://localhost:8080",
			why: "the trap this package exists to avoid: bracketing a URL yields " +
				"[http://localhost:8080] and breaks a working caller",
		},
		{
			name: "full_https_url_with_path",
			host: "https://sonar.internal:9000/healthz",
			want: "https://sonar.internal:9000/healthz",
			why:  "same trap, scheme and path variant",
		},
		{
			name: "host_port_text",
			host: "localhost:8080",
			want: "localhost:8080",
			why: "an already-composed authority contains a colon but is not an " +
				"IP literal, so it must be left alone",
		},
		{
			name: "not_an_ip_but_colonful",
			host: "::not:an:ip",
			want: "::not:an:ip",
			why:  "ParseIP rejects it, so it is not treated as an IPv6 literal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BracketHost(tt.host); got != tt.want {
				t.Fatalf("BracketHost(%q) = %q, want %q — %s",
					tt.host, got, tt.want, tt.why)
			}
		})
	}
}

// TestBracketHost_Idempotent proves a second application is a no-op. Two call
// sites in this module compose in sequence, so a non-idempotent rule would
// produce "[[::1]]" only on the composed path — the hardest variant to spot.
func TestBracketHost_Idempotent(t *testing.T) {
	for _, host := range []string{
		"::1", "2001:db8::1", "::ffff:127.0.0.1", "fe80::1%eth0",
		"127.0.0.1", "localhost", "http://localhost:8080", "",
	} {
		once := BracketHost(host)
		twice := BracketHost(once)
		if once != twice {
			t.Fatalf("BracketHost is not idempotent for %q: once=%q twice=%q",
				host, once, twice)
		}
	}
}

// TestJoinHostPort_ProducesResolvableAuthority is the sink-side assertion: for
// every host the resolver can handle, the composed authority must resolve.
// Asserting only against expected strings would pass even if the expectation
// and the implementation were wrong in the same way.
func TestJoinHostPort_ProducesResolvableAuthority(t *testing.T) {
	for _, host := range []string{
		"::1", "2001:db8::1", "::ffff:127.0.0.1", "[::1]", "127.0.0.1", "localhost",
	} {
		addr := JoinHostPort(host, "9000")
		if _, err := net.ResolveTCPAddr("tcp", addr); err != nil {
			t.Fatalf("JoinHostPort(%q, \"9000\") = %q, which the resolver "+
				"rejects: %v", host, addr, err)
		}
	}
}

// TestJoinHostPort_PreservesLegacyShape pins the two shape details that the
// replaced fmt.Sprintf produced, so the repair cannot quietly alter output that
// existing callers and tests depend on.
func TestJoinHostPort_PreservesLegacyShape(t *testing.T) {
	if got, want := JoinHostPort("localhost", ""), "localhost:"; got != want {
		t.Fatalf("JoinHostPort with an empty port = %q, want %q — the separator "+
			"was emitted unconditionally before the repair", got, want)
	}
	if got, want := JoinHostPort("", "9000"), ":9000"; got != want {
		t.Fatalf("JoinHostPort with an empty host = %q, want %q", got, want)
	}
}
