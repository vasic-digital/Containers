package endpoint

// Wave-20 batch CT-HARDEN-EP-HARD guard suite.
//
// HONEST BOUNDARY (§11.4.107): these are PURE, in-process table tests
// (§11.4.27 — no container/network/infra). They prove the VALUE-BUILDER
// and URL/host:port assembly LOGIC:
//   * EP-1 net.JoinHostPort IPv6 bracketing so ResolvedURL is url.Parse-able
//   * EP-2 ResolveScheme agrees with the scheme of ResolvedURL
//   * EP-3 additive Validate()/BuildValidated() error path (Build() unchanged)
//   * EP-4 empty-required-host is surfaced by Validate(), not silently masked
// They do NOT assert live reachability of any endpoint.

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EP-1 — IPv6 host:port MUST be bracket-wrapped (net.JoinHostPort) so the
// assembled address and URL are valid; IPv4/hostnames MUST stay unchanged.
func TestWave20_EP1_ResolveHostPort_IPv6Bracketed(t *testing.T) {
	tests := []struct {
		name string
		host string
		port string
		want string
	}{
		{"ipv6 loopback", "::1", "8080", "[::1]:8080"},
		{"ipv6 full", "2001:db8::1", "443", "[2001:db8::1]:443"},
		// negative controls — unchanged assembly.
		{"ipv4 unchanged", "127.0.0.1", "8080", "127.0.0.1:8080"},
		{"hostname unchanged", "db.local", "5432", "db.local:5432"},
		{"host only unchanged", "db", "", "db"},
		{"empty host defaults", "", "5432", "localhost:5432"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := &ServiceEndpoint{Host: tc.host, Port: tc.port}
			assert.Equal(t, tc.want, ResolveHostPort(ep))
		})
	}
}

// EP-1 — an IPv6 endpoint (which IsLocalEndpoint accepts first-class) MUST
// resolve to a URL that url.Parse ACCEPTS. The pre-fix "http://::1:8080" is
// rejected by url.Parse ("invalid port ... after host").
func TestWave20_EP1_ResolvedURL_IPv6_Parseable(t *testing.T) {
	ep := &ServiceEndpoint{Host: "::1", Port: "8080"}
	got := ep.ResolvedURL()
	assert.Equal(t, "http://[::1]:8080", got)

	u, err := url.Parse(got)
	require.NoError(t, err, "ResolvedURL(%q) must be url.Parse-able", got)
	assert.Equal(t, "[::1]:8080", u.Host)
	assert.Equal(t, "8080", u.Port())
	assert.Equal(t, "http", u.Scheme)

	// negative control — IPv4 URL still parses and is unchanged.
	ep4 := &ServiceEndpoint{Host: "127.0.0.1", Port: "8080"}
	got4 := ep4.ResolvedURL()
	assert.Equal(t, "http://127.0.0.1:8080", got4)
	u4, err4 := url.Parse(got4)
	require.NoError(t, err4)
	assert.Equal(t, "127.0.0.1:8080", u4.Host)
}

// EP-2 — ResolveScheme MUST agree with the scheme actually produced by
// ResolvedURL, including when the scheme is embedded in Host.
func TestWave20_EP2_ResolveScheme_AgreesWithResolvedURL(t *testing.T) {
	tests := []struct {
		name string
		ep   ServiceEndpoint
	}{
		{"https in host", ServiceEndpoint{Host: "https://secure.local", Port: "443"}},
		{"http in host", ServiceEndpoint{Host: "http://plain.local", Port: "80"}},
		{"bare host defaults http", ServiceEndpoint{Host: "plain.local", Port: "80"}},
		{"https url", ServiceEndpoint{URL: "https://api.example.com"}},
		{"http url", ServiceEndpoint{URL: "http://api.example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := tc.ep
			u, err := url.Parse(ep.ResolvedURL())
			require.NoError(t, err)
			assert.Equal(t, u.Scheme, ResolveScheme(&ep),
				"ResolveScheme must equal the scheme of ResolvedURL")
		})
	}
	// direct assertion of the reported forensic case.
	assert.Equal(t, "https",
		ResolveScheme(&ServiceEndpoint{Host: "https://secure.local", Port: "443"}))
}

// EP-3 — additive Validate() catches silently-invalid endpoints. The plain
// Build() contract is UNCHANGED: it still returns the value with no error.
func TestWave20_EP3_Validate_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		ep      ServiceEndpoint
		wantErr bool
	}{
		{"non-numeric port", ServiceEndpoint{Host: "h", Port: "abc", Enabled: true}, true},
		{"port too high", ServiceEndpoint{Host: "h", Port: "99999", Enabled: true}, true},
		{"port negative", ServiceEndpoint{Host: "h", Port: "-1", Enabled: true}, true},
		{"port zero", ServiceEndpoint{Host: "h", Port: "0", Enabled: true}, true},
		{"host with whitespace", ServiceEndpoint{Host: " x\n", Port: "80", Enabled: true}, true},
		{"unknown health type", ServiceEndpoint{Host: "h", Port: "80", Enabled: true, HealthType: "gopher"}, true},
		// EP-4: enabled endpoint with empty host+url must be caught,
		// never silently masked to localhost.
		{"enabled empty host+url", ServiceEndpoint{Enabled: true}, true},
		// EP-4: required-but-disabled endpoint still needs a target.
		{"required empty host+url", ServiceEndpoint{Required: true}, true},
		// negative controls — well-formed endpoints validate clean.
		{"well-formed host:port", ServiceEndpoint{Host: "db.local", Port: "5432", Enabled: true, HealthType: "http"}, false},
		{"well-formed ipv6", ServiceEndpoint{Host: "::1", Port: "8080", Enabled: true, HealthType: "tcp"}, false},
		{"well-formed url only", ServiceEndpoint{URL: "https://api.example.com", Enabled: true, HealthType: "http"}, false},
		{"disabled empty is fine", ServiceEndpoint{Enabled: false}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ep.Validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// EP-3 — Build() stays lenient (back-compat, no error path); the NEW additive
// BuildValidated() surfaces the error. Proves the signature was NOT changed.
func TestWave20_EP3_BuildUnchanged_BuildValidatedGates(t *testing.T) {
	// Build() still returns a bare value (no error) even for a bad port.
	ep := NewEndpoint().WithHost("h").WithPort("abc").Build()
	assert.Equal(t, "abc", ep.Port, "Build() remains non-validating for back-compat")

	// BuildValidated() is the additive error path.
	_, err := NewEndpoint().WithHost("h").WithPort("abc").BuildValidated()
	assert.Error(t, err)

	okEp, okErr := NewEndpoint().WithHost("h").WithPort("8080").BuildValidated()
	assert.NoError(t, okErr)
	assert.Equal(t, "8080", okEp.Port)
}
