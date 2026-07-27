package health

import (
	"context"
	"math"
	"time"
)

// maxRetryDelay caps the computed exponential-backoff delay (Wave-20 HE-5).
// Without a ceiling, a large Delay/BackoffFactor/MaxRetries combination can
// overflow the float64->time.Duration conversion; on amd64 this has been
// empirically confirmed to yield math.MinInt64 (a large NEGATIVE duration)
// via the CVTTSD2SI "integer indefinite" result, and time.After(negative)
// fires immediately — turning the intended backoff into a busy-loop.
const maxRetryDelay = 10 * time.Minute

// nextRetryDelay computes the next backoff delay for CheckWithRetry,
// clamped to maxRetryDelay whenever the raw product is non-finite or
// negative (the overflow signature above — delay and backoffFactor are
// both non-negative here, so a negative product can only be an overflow
// artifact, never a legitimate result) or simply larger than the ceiling.
func nextRetryDelay(delay time.Duration, backoffFactor float64) time.Duration {
	if backoffFactor <= 0 {
		return delay
	}
	next := float64(delay) * backoffFactor
	if math.IsNaN(next) || math.IsInf(next, 0) || next < 0 {
		return maxRetryDelay
	}
	if next > float64(maxRetryDelay) {
		return maxRetryDelay
	}
	return time.Duration(next)
}

// RetryPolicy configures how many times and how long to wait between
// successive health check attempts.
type RetryPolicy struct {
	// MaxRetries is the maximum number of additional attempts after the
	// first failure. A value of 0 means no retries.
	MaxRetries int
	// Delay is the base duration to wait between attempts.
	Delay time.Duration
	// BackoffFactor multiplies the delay after each failed attempt. A
	// value of 1.0 keeps the delay constant.
	BackoffFactor float64
}

// DefaultRetryPolicy returns a sensible default retry configuration.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:    3,
		Delay:         1 * time.Second,
		BackoffFactor: 2.0,
	}
}

// CheckWithRetry performs a health check using the given checker and
// target, retrying according to the policy until the check succeeds or
// all attempts are exhausted. The last HealthResult is always returned.
func CheckWithRetry(
	ctx context.Context,
	checker HealthChecker,
	target HealthTarget,
	policy RetryPolicy,
) *HealthResult {
	var result *HealthResult
	delay := policy.Delay

	// Clamp a negative retry count to 0 so at least one check always
	// runs and a non-nil result is always returned (honouring the
	// docstring contract "The last HealthResult is always returned").
	// Without this, MaxRetries < 0 makes the loop guard false on the
	// first iteration, the body never runs, and a nil *HealthResult is
	// returned — nil-panicking a normal caller's result.Healthy deref.
	maxRetries := policy.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		result = checker.Check(ctx, target)
		if result.Healthy {
			return result
		}

		// Don't sleep after the last attempt.
		if attempt < maxRetries {
			select {
			case <-ctx.Done():
				result.Error = "context cancelled during retry: " + result.Error
				return result
			case <-time.After(delay):
			}

			delay = nextRetryDelay(delay, policy.BackoffFactor)
		}
	}

	return result
}
