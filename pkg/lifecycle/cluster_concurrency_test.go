package lifecycle_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultManager_ReleaseFunc_ConcurrentDoubleRelease_NoRace guards
// LIFE-(b) RELEASED-RACE. The ReleaseFunc idempotency guard must be
// concurrency-safe: two goroutines calling the SAME ReleaseFunc concurrently
// data-race on a plain non-atomic `released` bool and double-Release the
// semaphore. RED (pre-fix): `go test -race` reports a data race in
// DefaultManager.Acquire.func2. GREEN (sync.Once guard): -race clean.
func TestDefaultManager_ReleaseFunc_ConcurrentDoubleRelease_NoRace(t *testing.T) {
	orch := &stubOrchestrator{}
	hc := &stubHealthChecker{healthy: true}
	m := lifecycle.NewDefaultManager(orch, hc)
	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name: "svc", ComposeFile: "c.yml", MaxConcurrent: 1,
	}))
	ctx := context.Background()
	require.NoError(t, m.Start(ctx, "svc"))

	release, err := m.Acquire(ctx, "svc")
	require.NoError(t, err)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; release() }()
	}
	close(start)
	wg.Wait()
	// The -race detector is the oracle; a clean run once the fix lands is the proof.
}

// TestDefaultManager_IdleShutdown_HeldLeaseNotReclaimed guards LIFE-(a)
// IDLE-vs-LEASE. A service must NOT be idle-reclaimed while a lease is held
// (types.go: "Failure to call [ReleaseFunc] will prevent idle shutdown").
// RED (pre-fix): after holding a lease past IdleTimeout, State=="stopped"
// while ActiveUsers==1. GREEN (busy-gate): stays "running" while held.
func TestDefaultManager_IdleShutdown_HeldLeaseNotReclaimed(t *testing.T) {
	orch := &stubOrchestrator{}
	hc := &stubHealthChecker{healthy: true}
	m := lifecycle.NewDefaultManager(orch, hc)
	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name:          "svc",
		ComposeFile:   "c.yml",
		IdleTimeout:   40 * time.Millisecond,
		MaxConcurrent: 1,
	}))
	ctx := context.Background()
	require.NoError(t, m.Start(ctx, "svc"))

	release, err := m.Acquire(ctx, "svc") // HOLD — never released
	require.NoError(t, err)
	defer release()

	time.Sleep(300 * time.Millisecond) // >> IdleTimeout

	st, err := m.Status("svc") // read under m.mu — harness stays -race clean
	require.NoError(t, err)
	assert.Equal(t, "running", st.State,
		"idle shutdown must not reclaim a service while a lease is held "+
			"(got State=%q ActiveUsers=%d)", st.State, st.ActiveUsers)
	assert.Equal(t, 1, st.ActiveUsers)
}
