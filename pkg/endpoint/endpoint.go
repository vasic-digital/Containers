package endpoint

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Package-wide defaults. Centralised as named constants (§6.R) so the
// same value is not spelled as a literal across builder.go, config.go,
// endpoint.go, and resolver.go. These are the WithHost/WithHealthType/
// scheme-overridable defaults, not hardcoded connection targets.
const (
	// defaultHost is the fallback host when an endpoint specifies none.
	defaultHost = "localhost"
	// defaultScheme is the URL scheme applied when the host/URL carries
	// no explicit http://|https:// prefix.
	defaultScheme = "http"
	// defaultHealthType is the health-check type applied when unset.
	defaultHealthType = "http"
	// defaultRetryCount is the health-check retry count applied when unset.
	defaultRetryCount = 3
)

// defaultTimeout is the health-check timeout applied when unset.
const defaultTimeout = 10 * time.Second

// knownHealthTypes is the closed set of health-check mechanisms the
// health package recognises (see pkg/health/types.go). Validate rejects
// any other non-empty value. Empty is permitted (defaulted downstream).
var knownHealthTypes = map[string]struct{}{
	"http":   {},
	"tcp":    {},
	"grpc":   {},
	"custom": {},
}

// ServiceEndpoint holds the configuration for a single service
// endpoint, including connectivity, health checking, and discovery
// settings.
type ServiceEndpoint struct {
	// Host is the hostname or IP address of the service.
	Host string
	// Port is the port number the service listens on.
	Port string
	// URL is an explicit URL override. When set, it takes
	// precedence over Host and Port for URL resolution.
	URL string

	// Enabled indicates whether this endpoint is active.
	Enabled bool
	// Required indicates whether the service must be reachable
	// for the system to start successfully.
	Required bool
	// Remote indicates the service runs on a remote host and
	// should not be managed by the local container runtime.
	Remote bool

	// HealthPath is the HTTP path used for health checks
	// (e.g., "/healthz").
	HealthPath string
	// HealthType is the type of health check to perform
	// (e.g., "http", "tcp", "grpc").
	HealthType string

	// Timeout is the maximum duration to wait for a response
	// from this endpoint.
	Timeout time.Duration
	// RetryCount is the number of retry attempts for failed
	// health checks.
	RetryCount int

	// ComposeFile is the path to the Docker Compose file that
	// defines this service.
	ComposeFile string
	// ServiceName is the name of the service within the compose
	// file.
	ServiceName string
	// Profile is the compose profile this service belongs to.
	Profile string

	// DiscoveryEnabled indicates whether service discovery is
	// active for this endpoint.
	DiscoveryEnabled bool
	// DiscoveryMethod is the discovery mechanism (e.g., "dns",
	// "consul", "static").
	DiscoveryMethod string
	// DiscoveryTimeout is the maximum duration for a discovery
	// lookup.
	DiscoveryTimeout time.Duration

	// Discovered indicates that this endpoint was resolved via
	// service discovery rather than static configuration.
	Discovered bool
}

// ResolvedURL returns the base URL for this endpoint. If URL is
// set explicitly it is returned directly. Otherwise the URL is
// constructed from Host and Port without the HealthPath.
func (e *ServiceEndpoint) ResolvedURL() string {
	if e.URL != "" {
		return e.URL
	}
	return resolveURL(e.Host, e.Port, "")
}

// resolveURL builds a URL from host, port, and an optional path.
//
// EP-1: host:port assembly uses net.JoinHostPort so an IPv6 literal is
// bracket-wrapped ("[::1]:8080") and the result is url.Parse-able, while
// IPv4/hostnames are left unchanged. Any scheme prefix embedded in host
// is preserved and split off first so JoinHostPort only ever sees the
// bare host authority (a scheme's "://" colon must not trigger bracketing).
func resolveURL(host, port, path string) string {
	if host == "" {
		host = defaultHost
	}
	scheme := defaultScheme + "://"
	bareHost := host
	switch {
	case strings.HasPrefix(host, "https://"):
		scheme = "https://"
		bareHost = strings.TrimPrefix(host, "https://")
	case strings.HasPrefix(host, "http://"):
		scheme = "http://"
		bareHost = strings.TrimPrefix(host, "http://")
	}
	authority := bareHost
	if port != "" {
		// EP2-1: strip an already-bracketed IPv6 literal ("[::1]") first so
		// JoinHostPort re-adds exactly one bracket layer, not an invalid
		// double-wrapped "[[::1]]:port".
		authority = net.JoinHostPort(unbracketHost(bareHost), port)
	}
	base := scheme + authority
	if path != "" {
		path = "/" + strings.TrimLeft(path, "/")
		return base + path
	}
	return base
}

// Validate reports whether the endpoint is internally consistent.
//
// EP-3/EP-4: Validate is ADDITIVE — it does NOT change the Build()
// contract. A plain Build() still returns the value unchecked for
// backward compatibility; callers opt into checking via Validate() (or
// the BuildValidated helper). It catches: a non-numeric or out-of-range
// port; a host containing whitespace/control characters; an unknown
// health type; and an enabled-or-required endpoint that specifies no
// host and no url (the empty-host case that resolveURL would otherwise
// silently mask to localhost).
func (e *ServiceEndpoint) Validate() error {
	if e.Port != "" {
		n, err := strconv.Atoi(e.Port)
		if err != nil {
			return fmt.Errorf(
				"endpoint: invalid port %q: not numeric", e.Port,
			)
		}
		if n < 1 || n > 65535 {
			return fmt.Errorf(
				"endpoint: port %d out of range 1-65535", n,
			)
		}
	}
	if e.Host != "" && hasSpaceOrControl(e.Host) {
		return fmt.Errorf(
			"endpoint: host %q contains whitespace or control characters",
			e.Host,
		)
	}
	if e.HealthType != "" {
		if _, ok := knownHealthTypes[strings.ToLower(e.HealthType)]; !ok {
			return fmt.Errorf(
				"endpoint: unknown health type %q", e.HealthType,
			)
		}
	}
	if (e.Enabled || e.Required) && e.Host == "" && e.URL == "" {
		return fmt.Errorf(
			"endpoint: enabled/required endpoint needs a host or url",
		)
	}
	return nil
}

// hasSpaceOrControl reports whether s contains any Unicode whitespace or
// control character — an invalid host/authority component.
func hasSpaceOrControl(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// unbracketHost removes a single surrounding "[" "]" pair from an
// already-bracketed IPv6 host literal (e.g. "[::1]" -> "::1"). A downstream
// net.JoinHostPort re-adds exactly one bracket layer for a colon-bearing
// host, so without this an already-bracketed input double-wraps into an
// invalid "[[::1]]:port" (rejected by url.Parse). Only a bracketed literal
// whose inner text contains a colon (a genuine IPv6 address) is unwrapped;
// IPv4 / hostname / bare-IPv6 / other bracketed text is returned unchanged.
func unbracketHost(h string) string {
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' &&
		strings.Contains(h[1:len(h)-1], ":") {
		return h[1 : len(h)-1]
	}
	return h
}
