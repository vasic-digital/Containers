package discovery

import (
	"context"
	"time"
)

// defaultDiscoveryTimeout is the fallback probe timeout applied when a
// DiscoveryTarget leaves Timeout non-positive — either unset (zero) or a
// nonsensical negative value. Both DNS and TCP normalise `Timeout <= 0` to
// this default: a negative duration otherwise poisons the probe (an
// already-expired context / a past dial deadline) and falsely reports a
// reachable target unreachable. Hoisted here so the DNS and TCP discoverers
// share one authoritative value instead of each hardcoding a duplicate
// literal (§6.R no-hardcoding / de-duplication).
const defaultDiscoveryTimeout = 5 * time.Second

// DiscoveryTarget describes a service endpoint to discover.
type DiscoveryTarget struct {
	// Name is a human-readable identifier for the target.
	Name string
	// Host is the hostname or IP address to probe.
	Host string
	// Port is the port number to probe.
	Port string
	// Method is the discovery mechanism ("tcp", "dns").
	Method string
	// Timeout is the maximum duration for a discovery attempt.
	Timeout time.Duration
}

// Discoverer probes a service endpoint using the configured discovery
// mechanism (DNS name resolution or TCP connect).
//
// The precise meaning of a true result is mechanism-specific and
// deliberately weak: this package proves REACHABILITY only, never liveness,
// health, or service identity. A true result means only that the
// mechanism's own check succeeded — the name resolved (DNS) or the port
// accepted a connection (TCP). It does NOT prove that the intended service
// is running, is the correct service, or is healthy; compose this package
// with pkg/health for a liveness/health signal.
//
// A false result is always returned together with a non-nil error, and
// found==false does NOT mean the service is definitively absent: it
// conflates a definitive-absent outcome (e.g. NXDOMAIN, connection refused)
// with an indeterminate one (context cancel/timeout, network unreachable).
// The boolean alone cannot distinguish these; callers that need the
// distinction MUST inspect the returned error's class. The wrapped cause is
// preserved via %w, so errors.Is(err, context.Canceled),
// errors.Is(err, context.DeadlineExceeded), and a net.Error.Timeout()
// type-assertion all work.
type Discoverer interface {
	// Discover probes the target. It returns (true, nil) when the
	// mechanism-specific reachability check succeeds, or (false, err)
	// otherwise. See the Discoverer doc for the deliberately weak meaning
	// of "reachable" and the caveat that found==false is not proof of
	// absence.
	Discover(ctx context.Context, target DiscoveryTarget) (bool, error)
}
