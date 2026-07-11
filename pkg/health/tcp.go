package health

import (
	"context"
	"fmt"
	"net"
	"time"
)

const defaultTCPTimeout = 5 * time.Second

// CheckTCP performs a health check by attempting a TCP dial to the
// target's resolved address (host:port).
func CheckTCP(ctx context.Context, target HealthTarget) *HealthResult {
	start := time.Now()
	addr := target.ResolvedAddress()

	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTCPTimeout
	}

	// Respect context deadline if it is shorter.
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining < timeout {
			timeout = remaining
		}
	}

	// Use a Dialer bound to ctx (not net.DialTimeout, which takes no
	// context and cannot observe cancellation) so an explicit cancel()
	// on a deadline-less ctx aborts the dial promptly instead of
	// blocking for the full timeout — matching grpc.go's DialContext
	// use (Wave-20 HE-1).
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	duration := time.Since(start)

	if err != nil {
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Duration:  duration,
			Error:     fmt.Sprintf("tcp dial failed: %v", err),
			Timestamp: start,
		}
	}
	_ = conn.Close()

	return &HealthResult{
		Target:    target.Name,
		Healthy:   true,
		Duration:  duration,
		Timestamp: start,
		Details:   map[string]string{"address": addr},
	}
}
