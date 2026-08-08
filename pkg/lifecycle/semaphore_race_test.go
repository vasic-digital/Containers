package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"digital.vasic.containers/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrencySemaphore_ExtraReleaseBeyondTotalCapacity_IsSafeNoOp
// proves the invariant ConcurrencySemaphore.Release() actually
// guarantees: calling Release() more times than the TOTAL number of
// currently active acquisitions (i.e. when nobody at all holds a slot)
// is always a safe no-op — ActiveCount() never goes negative, the
// internal channel is never over-drained, and a subsequent Acquire()
// still behaves correctly. This generalises the existing
// TestConcurrencySemaphore_ReleaseWithoutAcquire (zero acquisitions
// ever made) to the "was acquired, was correctly released, then
// released again with nobody active" case (constitution/Constitution.md
// §11.4.108 sibling-primitive audit of the idle.go stale-fire race).
func TestConcurrencySemaphore_ExtraReleaseBeyondTotalCapacity_IsSafeNoOp(t *testing.T) {
	sem := lifecycle.NewConcurrencySemaphore(1)
	ctx := context.Background()

	require.NoError(t, sem.Acquire(ctx))
	sem.Release()
	assert.Equal(t, 0, sem.ActiveCount())

	// Extra releases beyond total capacity: must stay a safe no-op.
	sem.Release()
	sem.Release()
	assert.Equal(t, 0, sem.ActiveCount(),
		"releasing beyond the total active count must never go negative")

	// The semaphore must still work correctly afterwards.
	require.NoError(t, sem.Acquire(ctx))
	assert.Equal(t, 1, sem.ActiveCount())
	sem.Release()
	assert.Equal(t, 0, sem.ActiveCount())
}

// TestConcurrencySemaphore_DoubleReleaseWhileOtherHolderActive_IsAKnownCallerDisciplineLimit
// documents a REAL, investigated hazard that is NOT closable with a
// same-signature fix: ConcurrencySemaphore.Release() takes no argument
// identifying which acquisition it corresponds to, so it cannot tell
// "I am releasing my own slot" apart from "I am releasing a different,
// still-active holder's slot". A caller that double-releases one
// Acquire() while another holder is genuinely still active will
// prematurely free that OTHER holder's slot, letting one extra
// Acquire() through beyond the configured max.
//
// This is the same class of limitation golang.org/x/sync/semaphore's
// Weighted.Release documents as caller-discipline ("it is a run-time
// error to call Release with n greater than what is available") rather
// than something the primitive defends against — closing it fully
// would require a per-acquisition token/handle, which changes the
// exported Acquire/Release signatures and is out of scope for this
// audit (constitution/Constitution.md §11.4.108/§11.4.112: investigated
// and confirmed structurally bounded by the current no-token API, not a
// closable bug within it).
//
// This test is NOT a regression guard against a fix (there is none to
// guard) — it is a recorded, deterministic demonstration of the
// boundary, kept green by asserting the ACTUAL (structurally expected)
// outcome, so a future accidental "fix" that changes this behavior
// without also changing the API is caught and re-evaluated rather than
// silently drifting.
func TestConcurrencySemaphore_DoubleReleaseWhileOtherHolderActive_IsAKnownCallerDisciplineLimit(t *testing.T) {
	sem := lifecycle.NewConcurrencySemaphore(2)
	ctx := context.Background()

	require.NoError(t, sem.Acquire(ctx)) // holder A
	require.NoError(t, sem.Acquire(ctx)) // holder B
	require.Equal(t, 2, sem.ActiveCount())

	sem.Release() // holder A's real release
	sem.Release() // holder A's caller bug: double-release

	// Structurally expected (documented limitation, not a bug this
	// no-token API can prevent): the double-release drains holder B's
	// slot too, so ActiveCount() reads 0 even though B never released.
	assert.Equal(t, 0, sem.ActiveCount())

	// Observable consequence: a third Acquire() succeeds immediately
	// instead of blocking, because the semaphore now (incorrectly)
	// believes it is fully free. Any caller relying on Release() to be
	// self-correcting against this class of misuse MUST instead adopt
	// the same release-once idempotency guard DefaultManager.Acquire()
	// already uses in manager.go (a `released` bool captured per lease).
	timeoutCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := sem.Acquire(timeoutCtx)
	assert.NoError(t, err,
		"documents the known limitation: the stolen slot lets an extra Acquire() through")

	sem.Release() // cleanup holder B's slot (the one this test proved gets stolen)
}
