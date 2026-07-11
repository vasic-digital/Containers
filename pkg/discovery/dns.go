package discovery

import (
	"context"
	"fmt"
	"net"
)

// hostLookup defines the interface for DNS host lookups.
type hostLookup interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// defaultHostLookup uses net.Resolver for DNS lookups.
type defaultHostLookup struct {
	resolver *net.Resolver
}

func (d *defaultHostLookup) LookupHost(
	ctx context.Context, host string,
) ([]string, error) {
	return d.resolver.LookupHost(ctx, host)
}

// DNSDiscoverer implements Discoverer by performing a DNS lookup
// for the target host.
type DNSDiscoverer struct {
	lookup hostLookup
}

// NewDNSDiscoverer returns a new DNSDiscoverer.
func NewDNSDiscoverer() *DNSDiscoverer {
	return &DNSDiscoverer{
		lookup: &defaultHostLookup{resolver: &net.Resolver{}},
	}
}

// Discover performs a DNS host lookup for target.Host and returns
// (true, nil) when at least one address resolves.
//
// A true result proves only that the NAME RESOLVES to >= 1 A/AAAA address.
// It says NOTHING about whether a service actually listens at that address,
// whether it is the intended service, or whether it is healthy — a wildcard
// DNS entry or a stale record resolves successfully while nothing serves the
// name. This package deliberately splits discovery from health; compose with
// pkg/health for a liveness signal.
//
// On failure it returns (false, err). found==false does NOT prove the name is
// absent: an NXDOMAIN (definitive absent) and a context cancel/timeout or
// network-unreachable (indeterminate) both yield (false, err). The wrapped
// cause is preserved via %w, so callers needing to tell these apart MUST
// inspect the error class (e.g. errors.Is(err, context.DeadlineExceeded)).
func (d *DNSDiscoverer) Discover(
	ctx context.Context,
	target DiscoveryTarget,
) (bool, error) {
	if target.Host == "" {
		return false, fmt.Errorf(
			"discovery %s: host is required for DNS lookup",
			target.Name,
		)
	}

	timeout := target.Timeout
	if timeout == 0 {
		timeout = defaultDiscoveryTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lookup := d.lookup
	if lookup == nil {
		lookup = &defaultHostLookup{resolver: &net.Resolver{}}
	}

	addrs, err := lookup.LookupHost(ctx, target.Host)
	if err != nil {
		return false, fmt.Errorf(
			"discovery %s: dns lookup %s: %w",
			target.Name, target.Host, err,
		)
	}
	if len(addrs) == 0 {
		return false, fmt.Errorf(
			"discovery %s: dns lookup %s: no addresses found",
			target.Name, target.Host,
		)
	}
	return true, nil
}
