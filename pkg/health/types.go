package health

import (
	"time"

	"digital.vasic.containers/internal/netaddr"
)

// HealthType identifies the mechanism used to verify a target's health.
type HealthType string

const (
	// HealthTCP performs a raw TCP dial to verify connectivity.
	HealthTCP HealthType = "tcp"
	// HealthHTTP sends an HTTP GET and checks the response status.
	HealthHTTP HealthType = "http"
	// HealthGRPC performs a gRPC health check (or TCP fallback).
	HealthGRPC HealthType = "grpc"
	// HealthCustom delegates to a user-supplied check function.
	HealthCustom HealthType = "custom"
)

// HealthTarget describes a single endpoint that should be health-checked.
type HealthTarget struct {
	// Name is a human-readable identifier for this target.
	Name string
	// Host is the hostname or IP address.
	Host string
	// Port is the port number as a string.
	Port string
	// URL is the full URL for HTTP-based checks. When set, it takes
	// precedence over Host+Port for HTTP checks.
	URL string
	// Type selects the check mechanism.
	Type HealthType
	// Path is the HTTP path appended to Host:Port when URL is empty.
	Path string
	// Timeout is the maximum duration for a single check attempt.
	Timeout time.Duration
	// Required indicates whether a failure for this target should be
	// treated as fatal.
	Required bool
}

// ResolvedAddress returns the address to connect to. For HTTP targets with
// a URL set, the full URL is returned. Otherwise host:port is returned.
func (t *HealthTarget) ResolvedAddress() string {
	if t.URL != "" {
		return t.URL
	}
	if t.Host == "" && t.Port == "" {
		return ""
	}
	// An IPv6 literal must be bracketed or the resolver rejects the
	// authority outright ("too many colons in address"). netaddr.JoinHostPort
	// brackets only a bare IPv6 literal, so IPv4 hosts, hostnames and
	// already-bracketed literals pass through byte-for-byte unchanged.
	return netaddr.JoinHostPort(t.Host, t.Port)
}

// HealthResult captures the outcome of a single health check.
type HealthResult struct {
	// Target is the name of the checked target.
	Target string
	// Healthy indicates whether the target passed the check.
	Healthy bool
	// Duration is how long the check took.
	Duration time.Duration
	// Error contains the error message when the check failed.
	Error string
	// Timestamp records when the check was performed.
	Timestamp time.Time
	// Details holds optional key-value metadata about the check.
	Details map[string]string
}
