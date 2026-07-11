// Package discovery Wave-20 DEEPER (§11.4.118 second-pass) hardening guards.
//
// These are §11.4.115 RED->GREEN regression guards for defects the FIRST
// hardening pass missed. The first pass normalised an UNSET (zero) probe
// timeout to defaultDiscoveryTimeout in both the DNS and TCP discoverers via
// `if timeout == 0`, but a NEGATIVE timeout (a caller misconfiguration —
// Timeout: -1) slips past that `== 0` guard unnormalised, and a negative
// duration is nonsensical:
//
//	DISC-1 (dns.go)  a negative Timeout reaches context.WithTimeout(ctx, neg),
//	                 producing an ALREADY-EXPIRED child context, so a perfectly
//	                 resolvable host is reported (false, err) — a §11.4.108
//	                 honesty defect (a reachable name reported unreachable).
//	DISC-2 (tcp.go)  a negative Timeout reaches net.Dialer{Timeout: neg}. Go's
//	                 Dialer.deadline() computes now.Add(Timeout) = a deadline in
//	                 the PAST, so DialContext fails IMMEDIATELY with an i/o
//	                 timeout — a reachable listener reported unreachable.
//
// The single-line fix in each file is `if timeout == 0` -> `if timeout <= 0`,
// so a negative (nonsensical) timeout falls back to the shared default exactly
// like the already-handled zero case, instead of poisoning the probe.
//
// NO real network/daemon dependency: DISC-1 uses a ctx-respecting in-process
// lookup double (faithful to net.Resolver, which returns ctx.Err() on an
// expired context); DISC-2 uses a loopback listener exactly like the existing
// TCP tests. Fakes are permitted in unit-test sources only; this IS one.
package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxAwareLookup is a hostLookup double that faithfully models the real
// net.Resolver contract: it fails with the context's error when the context is
// already expired/cancelled, and otherwise returns its preset addresses. This
// is what makes the DISC-1 negative-timeout defect observable in-process — a
// mock that ignored ctx (like the existing mockHostLookup) could never expose
// the already-expired child context the negative timeout produces.
type ctxAwareLookup struct {
	addrs []string
}

func (c *ctxAwareLookup) LookupHost(ctx context.Context, _ string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.addrs, nil
}

// TestWave20_DISC_DNSNegativeTimeoutReportsResolvableHost is the §11.4.115
// guard for DISC-1. A resolvable host probed with a NEGATIVE Timeout must be
// reported found — the negative (nonsensical) value must normalise to the
// shared default, not build an already-expired child context that fails the
// lookup of a genuinely-resolvable name.
//
// RED (fix reverted to `if timeout == 0`): -1s slips through unnormalised ->
// context.WithTimeout(bg, -1s) is already Done -> ctxAwareLookup returns
// context.DeadlineExceeded -> Discover returns (false, err) -> require.NoError
// FAILs. GREEN (`if timeout <= 0`): -1s normalises to defaultDiscoveryTimeout
// -> child context is live -> lookup returns the address -> (true, nil).
func TestWave20_DISC_DNSNegativeTimeoutReportsResolvableHost(t *testing.T) {
	d := &DNSDiscoverer{lookup: &ctxAwareLookup{addrs: []string{"192.0.2.10"}}}
	target := DiscoveryTarget{
		Name:    "neg-timeout-dns",
		Host:    "resolvable.example",
		Method:  "dns",
		Timeout: -1 * time.Second, // caller misconfiguration: nonsensical negative
	}

	found, err := d.Discover(context.Background(), target)
	require.NoError(t, err, "a negative Timeout must normalise to the default like the zero case "+
		"(DISC-1): instead it built an already-expired child context that failed the lookup of a "+
		"genuinely-resolvable host — a §11.4.108 honesty defect (reachable name reported unreachable)")
	assert.True(t, found, "a resolvable host probed with a negative Timeout must be reported found")
}

// TestWave20_DISC_TCPNegativeTimeoutReportsReachableListener is the §11.4.115
// guard for DISC-2. A live loopback listener probed with a NEGATIVE Timeout
// must be reported reachable — the negative value must normalise to the shared
// default, not reach net.Dialer{Timeout: neg} where Go computes a PAST deadline
// and DialContext fails immediately.
//
// RED (fix reverted to `if timeout == 0`): -1s slips through -> Dialer.Timeout
// = -1s -> deadline now.Add(-1s) is in the past -> DialContext returns an
// immediate i/o timeout -> Discover returns (false, err) -> require.NoError
// FAILs. GREEN (`if timeout <= 0`): -1s normalises to defaultDiscoveryTimeout
// -> the loopback dial completes -> (true, nil).
func TestWave20_DISC_TCPNegativeTimeoutReportsReachableListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	d := NewTCPDiscoverer()
	target := DiscoveryTarget{
		Name:    "neg-timeout-tcp",
		Host:    "127.0.0.1",
		Port:    port,
		Method:  "tcp",
		Timeout: -1 * time.Second, // caller misconfiguration: nonsensical negative
	}

	found, discErr := d.Discover(context.Background(), target)
	require.NoError(t, discErr, "a negative Timeout must normalise to the default like the zero case "+
		"(DISC-2): instead it reached net.Dialer{Timeout: -1s}, whose past deadline made DialContext "+
		"fail immediately against a genuinely-reachable listener")
	assert.True(t, found, "a reachable listener probed with a negative Timeout must be reported found")
}
