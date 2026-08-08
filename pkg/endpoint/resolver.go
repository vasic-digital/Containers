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
		host = defaultHost
	}
	if ep.Port == "" {
		return host
	}
	// Callers dial this string, so an IPv6 literal must arrive bracketed.
	//
	// Both sides of this merge fix the same defect; netaddr.JoinHostPort is
	// kept because it is the strictly safer rule at THIS call site. There is
	// no scheme-splitting here, so Host can legitimately arrive holding a
	// full URL — a shape the sibling ResolveScheme explicitly supports
	// (Host="https://secure.local"). net.JoinHostPort brackets on seeing ANY
	// colon, so it would emit "[https://secure.local]:8080" and break that
	// caller; netaddr brackets only what net.ParseIP confirms is an IPv6
	// literal. It subsumes EP2-1 as well — an already-bracketed "[::1]" is
	// passed through untouched instead of being unwrapped and re-wrapped —
	// and additionally covers the v4-mapped ("::ffff:127.0.0.1", which
	// net.IP.To4 would wrongly exempt) and zoned ("fe80::1%eth0") forms.
	return netaddr.JoinHostPort(host, ep.Port)
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
