package discovery

import (
	"context"
	"fmt"
	"net"
)

// TCPDiscoverer implements Discoverer by performing a TCP dial to
// the target host and port.
type TCPDiscoverer struct{}

// NewTCPDiscoverer returns a new TCPDiscoverer.
func NewTCPDiscoverer() *TCPDiscoverer {
	return &TCPDiscoverer{}
}

// Discover attempts a TCP connection to target.Host:target.Port and
// returns (true, nil) when the TCP handshake completes.
//
// "Reachable" here means only that the port ACCEPTS A CONNECTION — the
// handshake is completed and the connection is immediately closed without
// exchanging any application bytes. It does NOT prove the intended service is
// live or correct: a port-squatter, or a listener that never calls Accept
// (the kernel completes the handshake from the listen backlog), both report
// reachable. Compose with pkg/health for an application-level liveness/health
// check.
//
// On failure it returns (false, err). found==false does NOT prove the service
// is absent: a connection refused (definitive absent) and a context
// cancel/timeout or network-unreachable (indeterminate) both yield
// (false, err). The wrapped cause is preserved via %w, so callers needing to
// tell these apart MUST inspect the error class (e.g.
// errors.Is(err, context.Canceled) or a net.Error.Timeout() assertion).
func (d *TCPDiscoverer) Discover(
	ctx context.Context,
	target DiscoveryTarget,
) (bool, error) {
	if target.Host == "" || target.Port == "" {
		return false, fmt.Errorf(
			"discovery %s: host and port are required",
			target.Name,
		)
	}

	timeout := target.Timeout
	if timeout == 0 {
		timeout = defaultDiscoveryTimeout
	}

	addr := net.JoinHostPort(target.Host, target.Port)

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, fmt.Errorf(
			"discovery %s: tcp dial %s: %w",
			target.Name, addr, err,
		)
	}
	_ = conn.Close()
	return true, nil
}
