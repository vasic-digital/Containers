package serviceregistry

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiscover_ExistingReturnsCopy_NotInternalAlias is a deterministic
// (race-detector-independent) proof that Discover's already-registered
// fast path returns a COPY of the stored *Service, matching the
// copy-on-return contract every other accessor honors (Get copies via
// `copy := *svc`). On the pre-fix code Discover returned the internal
// pointer directly, so a caller mutating the returned value silently
// corrupted the registry's stored record.
func TestDiscover_ExistingReturnsCopy_NotInternalAlias(t *testing.T) {
	r := newTestRegistry(t)
	require.NoError(t, r.Register("web", 8080))

	// Hits the already-registered fast path in Discover.
	svc, err := r.Discover(context.Background(), "web", 9000, 9000, 9010)
	require.NoError(t, err)
	require.Equal(t, 8080, svc.Port)

	// A caller reasonably treats a returned value as its own to mutate.
	svc.Port = 1
	svc.Healthy = false

	// The registry's stored record MUST be unaffected by the caller's
	// mutation. On the aliasing bug, Get sees the corrupted values.
	stored, ok := r.Get("web")
	require.True(t, ok)
	assert.Equal(t, 8080, stored.Port,
		"registry corrupted: Discover leaked the internal *Service pointer")
	assert.True(t, stored.Healthy,
		"registry corrupted: Discover leaked the internal *Service pointer")
}

// TestDiscover_ExistingConcurrentWithUpdateHealth exercises the data
// race directly: Discover's fast path hands the caller the internal
// *Service while UpdateHealth mutates that same struct under the lock.
// The caller reads the returned struct without the lock, so the two
// overlap on the same memory. Under `go test -race` this aborts the
// binary on the pre-fix code; with the copy fix the caller reads its
// own copy and the test passes cleanly.
func TestDiscover_ExistingConcurrentWithUpdateHealth(t *testing.T) {
	r := newTestRegistry(t)
	require.NoError(t, r.Register("web", 8080))

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: mutates the stored *Service (svc.Healthy, svc.LastChecked).
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			r.UpdateHealth("web", i%2 == 0)
		}
	}()

	// Reader: obtains the service via Discover's fast path and reads the
	// fields the writer mutates — without holding the registry lock.
	go func() {
		defer wg.Done()
		var sink bool
		for i := 0; i < iterations; i++ {
			svc, err := r.Discover(context.Background(), "web", 9000, 9000, 9010)
			if err != nil {
				continue
			}
			// Read the concurrently-mutated fields.
			sink = svc.Healthy && !svc.LastChecked.IsZero()
		}
		_ = sink
	}()

	wg.Wait()
}
