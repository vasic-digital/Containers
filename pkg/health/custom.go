package health

import (
	"context"
	"fmt"
	"time"
)

// CustomCheckFunc is a user-supplied function that returns an error when
// the target is unhealthy.
type CustomCheckFunc func(ctx context.Context, target HealthTarget) error

// NewCustomCheckFunc wraps a CustomCheckFunc into a CheckFunc suitable
// for registration on a DefaultChecker.
func NewCustomCheckFunc(fn CustomCheckFunc) CheckFunc {
	return func(ctx context.Context, target HealthTarget) *HealthResult {
		start := time.Now()
		err := fn(ctx, target)
		duration := time.Since(start)
		// A near-instant custom check (e.g. a no-op) can complete faster
		// than the monotonic clock's smallest representable tick, yielding
		// a literal 0 from time.Since. The check did take a non-negative
		// amount of real time, so report the smallest representable positive
		// duration rather than 0 — this keeps Duration a truthful "elapsed
		// time was measured" signal for consumers without fabricating delay.
		if duration <= 0 {
			duration = time.Nanosecond
		}

		result := &HealthResult{
			Target:    target.Name,
			Duration:  duration,
			Timestamp: start,
			Details:   map[string]string{"type": "custom"},
		}

		if err != nil {
			result.Healthy = false
			result.Error = fmt.Sprintf("custom check failed: %v", err)
		} else {
			result.Healthy = true
		}

		return result
	}
}
