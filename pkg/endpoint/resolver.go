package endpoint

import (
	"strings"

	"digital.vasic.containers/internal/netaddr"
)

// ResolveHealthURL returns the full health check URL for the
// given endpoint. It combines the base URL with the health path.
func ResolveHealthURL(ep *ServiceEndpoint) string {
	base := ep.ResolvedURL()
	if ep.HealthPath == "" {
		return base
	}
	path := "/" + strings.TrimLeft(ep.HealthPath, "/")
	return strings.TrimRight(base, "/") + path
}

// ResolveHostPort returns a "host:port" string for the endpoint.
// If the host is empty, "localhost" is used.
func ResolveHostPort(ep *ServiceEndpoint) string {
	host := ep.Host
	if host == "" {
		host = "localhost"
	}
	if ep.Port == "" {
		return host
	}
	// Callers dial this string, so an IPv6 literal must arrive bracketed.
	return netaddr.JoinHostPort(host, ep.Port)
}

// ResolveScheme returns the URL scheme inferred from the
// endpoint's explicit URL or defaults to "http".
func ResolveScheme(ep *ServiceEndpoint) string {
	if ep.URL != "" {
		if strings.HasPrefix(ep.URL, "https://") {
			return "https"
		}
		return "http"
	}
	return "http"
}

// IsLocalEndpoint returns true if the endpoint targets the local
// machine (localhost, 127.0.0.1, or empty host without Remote).
func IsLocalEndpoint(ep *ServiceEndpoint) bool {
	if ep.Remote {
		return false
	}
	h := strings.ToLower(ep.Host)
	return h == "" || h == "localhost" || h == "127.0.0.1" ||
		h == "::1"
}
