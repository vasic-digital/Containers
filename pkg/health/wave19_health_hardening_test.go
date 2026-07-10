package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckWithRetry_NegativeMaxRetries_ReturnsNonNilAndRunsOnce is the
// permanent CT-HARDEN-EVHD-2 guard (§11.4.115 GREEN polarity — asserts the
// FIXED behavior; a regression is reproduced by surgically reverting the
// negative-clamp in retry.go). Pre-fix, `for attempt := 0; attempt <=
// policy.MaxRetries` with MaxRetries<0 made the loop guard false on the first
// iteration: the body never ran, CheckWithRetry returned a nil *HealthResult,
// and a normal caller's `result.Healthy` deref nil-panicked. The clamp makes a
// negative retry count behave like 0 — at least one check runs and a non-nil
// result is always returned (the docstring contract).
func TestCheckWithRetry_NegativeMaxRetries_ReturnsNonNilAndRunsOnce(t *testing.T) {
	c := NewDefaultChecker()
	var calls int32
	c.Register(HealthCustom, func(
		_ context.Context, target HealthTarget,
	) *HealthResult {
		atomic.AddInt32(&calls, 1)
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Error:     "always unhealthy",
			Timestamp: time.Now(),
		}
	})

	policy := RetryPolicy{MaxRetries: -1, Delay: 10 * time.Millisecond}
	target := HealthTarget{Name: "neg-retries", Type: HealthCustom}

	result := CheckWithRetry(context.Background(), c, target, policy)

	// The load-bearing assertion: a non-nil result is always returned, so the
	// caller's result.Healthy deref cannot nil-panic.
	require.NotNil(t, result, "CheckWithRetry(MaxRetries<0) must return a non-nil result, not nil")
	_ = result.Healthy // proves no nil-deref panic
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"a negative MaxRetries must clamp to 0 and still run exactly one check")
}

// TestCheckWithRetry_ZeroMaxRetries_ExactlyOneCheck is the negative control:
// MaxRetries==0 (the documented "no retries") must still run exactly one
// check — the clamp must not change the well-defined zero path.
func TestCheckWithRetry_ZeroMaxRetries_ExactlyOneCheck(t *testing.T) {
	c := NewDefaultChecker()
	var calls int32
	c.Register(HealthCustom, func(
		_ context.Context, target HealthTarget,
	) *HealthResult {
		atomic.AddInt32(&calls, 1)
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Error:     "always unhealthy",
			Timestamp: time.Now(),
		}
	})

	policy := RetryPolicy{MaxRetries: 0, Delay: 10 * time.Millisecond}
	target := HealthTarget{Name: "zero-retries", Type: HealthCustom}

	result := CheckWithRetry(context.Background(), c, target, policy)

	require.NotNil(t, result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"MaxRetries==0 must run exactly one check")
}
