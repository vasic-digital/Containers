// Package netaddr provides the single, conservative rule this module uses to
// compose a network authority ("host:port") from a host and a port.
//
// The rule exists because an IPv6 literal contains colons, so the obvious
// fmt.Sprintf("%s:%s", host, port) yields an authority that the Go resolver
// rejects outright:
//
//	net.ResolveTCPAddr("tcp", "::1:8080")   -> address ::1:8080: too many colons
//	net.ResolveTCPAddr("tcp", "[::1]:8080") -> resolves
//
// net.JoinHostPort already solves that, but it brackets on seeing ANY colon,
// which makes it unsafe for the call sites in this module: several of them
// accept a host field that legitimately holds an already-bracketed literal
// ("[::1]") or a full URL ("http://localhost:8080"). Feeding either to
// net.JoinHostPort produces "[[::1]]:8080" or "[http://localhost:8080]:9000"
// and breaks a working caller.
//
// BracketHost therefore brackets only what provably needs it: a bare IPv6
// literal. Everything else — IPv4, hostnames, already-bracketed literals, full
// URLs, the empty string — is returned byte-for-byte unchanged.
package netaddr

import (
	"net"
	"strings"
)

// BracketHost returns host wrapped in square brackets when, and only when, it
// is an unbracketed IPv6 literal. Every other input is returned unchanged.
//
// The IPv6 determination is made by net.ParseIP rather than by looking for a
// colon, because a colon alone does not imply an IPv6 literal: "http://x" and
// "host:1234" both contain one and neither may be bracketed. A zone suffix
// ("fe80::1%eth0") is stripped before the parse — net.ParseIP does not accept
// zones — and preserved in the returned value.
//
// Examples:
//
//	BracketHost("::1")                      == "[::1]"
//	BracketHost("2001:db8::1")              == "[2001:db8::1]"
//	BracketHost("::ffff:127.0.0.1")         == "[::ffff:127.0.0.1]"
//	BracketHost("fe80::1%eth0")             == "[fe80::1%eth0]"
//	BracketHost("[::1]")                    == "[::1]"       // never doubled
//	BracketHost("127.0.0.1")                == "127.0.0.1"   // IPv4 untouched
//	BracketHost("sonar.internal")           == "sonar.internal"
//	BracketHost("http://localhost:8080")    == "http://localhost:8080"
//	BracketHost("")                         == ""
func BracketHost(host string) string {
	if host == "" || strings.HasPrefix(host, "[") {
		return host
	}

	// A host with no colon can never be an IPv6 literal, and is the
	// overwhelmingly common case — settle it before parsing.
	if !strings.Contains(host, ":") {
		return host
	}

	bare := host
	if i := strings.IndexByte(bare, '%'); i >= 0 {
		bare = bare[:i]
	}

	// Anything that is not a parseable IP literal is a hostname, a URL, or
	// already-composed text. None of those may be bracketed.
	if ip := net.ParseIP(bare); ip == nil {
		return host
	}

	// A parseable IP that still contains a colon is an IPv6 literal. This
	// deliberately includes the v4-mapped form "::ffff:127.0.0.1", whose
	// colons break an authority exactly like any other IPv6 literal even
	// though net.IP.To4 reports it as v4.
	return "[" + host + "]"
}

// JoinHostPort composes an authority from host and port, bracketing the host
// only when BracketHost says it must be.
//
// It intentionally differs from net.JoinHostPort in two ways that the call
// sites in this module depend on: it never brackets a non-IPv6 host (see
// BracketHost), and it preserves the historical shape of the string it
// replaces — the separator is emitted unconditionally, so an empty port still
// yields "host:", matching what fmt.Sprintf("%s:%s", host, port) produced.
// Callers that want the port omitted must check for the empty port themselves.
func JoinHostPort(host, port string) string {
	return BracketHost(host) + ":" + port
}
