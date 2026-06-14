package health

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestHelixServiceHealthChecker_Check_HonorsContextCancellation proves the
// retry loop in Check ignores context cancellation: it uses time.Sleep
// between attempts instead of selecting on ctx.Done(). With an already-
// cancelled context the loop should bail out promptly; instead it sleeps
// Retries*1s. A closed port (Port:1) makes each checkOnce fail fast so the
// dominant time is the inter-retry sleep, isolating the bug.
func TestHelixServiceHealthChecker_Check_HonorsContextCancellation(t *testing.T) {
	h := &HelixServiceHealthChecker{
		ServiceName: "ctx-cancel",
		CheckType:   "tcp",
		Host:        "127.0.0.1",
		Port:        1, // closed -> DialTimeout fails fast
		Timeout:     200 * time.Millisecond,
		Retries:     3, // buggy loop sleeps ~3s ignoring ctx
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	start := time.Now()
	status, _ := h.Check(ctx)
	elapsed := time.Since(start)

	assert.False(t, status.Healthy)
	// A context-honoring retry loop returns well under 1s when ctx is
	// already cancelled. The buggy time.Sleep loop takes ~3s.
	assert.Less(t, elapsed, 1*time.Second,
		"Check must honor context cancellation and not sleep through retries")
}
