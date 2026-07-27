package endpoint

import (
	"net"
	"strings"
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
		host = defaultHost
	}
	if ep.Port == "" {
		return host
	}
	// EP-1: net.JoinHostPort bracket-wraps an IPv6 literal ("[::1]:8080")
	// and leaves IPv4/hostnames unchanged, unlike a manual "%s:%s" concat.
	// EP2-1: unbracketHost first so an already-bracketed literal ("[::1]")
	// is not double-wrapped into an invalid "[[::1]]:port".
	return net.JoinHostPort(unbracketHost(host), ep.Port)
}

// ResolveScheme returns the URL scheme for the endpoint. It mirrors the
// scheme ResolvedURL would produce: an explicit URL's prefix wins, then a
// scheme prefix embedded in Host, otherwise the default scheme.
//
// EP-2: previously this ignored a scheme embedded in Host and always
// returned the default when URL was empty, so a caller composing
// ResolveScheme()+"://"+ResolveHostPort() disagreed with ResolvedURL()
// for e.g. Host="https://secure.local".
func ResolveScheme(ep *ServiceEndpoint) string {
	if ep.URL != "" {
		if strings.HasPrefix(ep.URL, "https://") {
			return "https"
		}
		return defaultScheme
	}
	if strings.HasPrefix(ep.Host, "https://") {
		return "https"
	}
	return defaultScheme
}

// IsLocalEndpoint returns true if the endpoint targets the local
// machine (localhost, 127.0.0.1, or empty host without Remote).
func IsLocalEndpoint(ep *ServiceEndpoint) bool {
	if ep.Remote {
		return false
	}
	// EP2-2: unbracketHost normalises an already-bracketed IPv6 literal
	// ("[::1]") so the loopback comparison recognises it exactly like the
	// bare "::1" form (otherwise "[::1]" is mis-classified as non-local).
	h := strings.ToLower(unbracketHost(ep.Host))
	return h == "" || h == "localhost" || h == "127.0.0.1" ||
		h == "::1"
}
