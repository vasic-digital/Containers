package lifecycle

import (
	"context"
	"fmt"
	"sync/atomic"
)

// ConcurrencySemaphore limits the number of concurrent users of a
// service. Callers must Acquire before using the service and
// Release when done.
type ConcurrencySemaphore struct {
	ch     chan struct{}
	max    int
	active atomic.Int32
}

// NewConcurrencySemaphore creates a semaphore allowing up to max
// concurrent acquisitions.
func NewConcurrencySemaphore(max int) *ConcurrencySemaphore {
	if max <= 0 {
		max = 1
	}
	return &ConcurrencySemaphore{
		ch:  make(chan struct{}, max),
		max: max,
	}
}

// Acquire blocks until a slot is available or ctx is cancelled.
func (s *ConcurrencySemaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		s.active.Add(1)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("semaphore: %w", ctx.Err())
	}
}

// Release returns a slot to the semaphore. It must be called EXACTLY
// once for each successful Acquire — this is a caller-discipline
// requirement inherent to a counting-channel semaphore with no
// per-acquisition token (the same contract golang.org/x/sync/semaphore's
// Weighted.Release documents: "it is a run-time error to call Release
// with a value larger than what is available"). Because Release() takes
// no argument identifying which acquisition it corresponds to, it
// cannot distinguish "I am releasing MY OWN slot" from "I am releasing
// SOME other still-active holder's slot" — a caller that double-calls
// Release() for one Acquire() while a different holder is genuinely
// still active will incorrectly free that OTHER holder's slot early,
// letting one extra Acquire() through. Closing that fully would require
// a per-acquisition token/handle (changing the exported Acquire/Release
// signatures), which is out of scope for a same-signature fix
// (constitution/Constitution.md §11.4.108/§11.4.112 sibling-primitive
// audit of the idle.go stale-fire race — investigated and confirmed
// structurally bounded by the current API, not a closable same-
// signature bug). What Release() DOES guarantee, and is covered by
// TestConcurrencySemaphore_ReleaseWithoutAcquire /
// TestConcurrencySemaphore_ExtraReleaseBeyondTotalCapacity_IsSafeNoOp:
// releasing more times than the TOTAL number of currently active
// acquisitions (i.e. when nobody at all is holding a slot) is always a
// safe no-op — it never panics, never blocks, and never drives
// ActiveCount() negative. The only in-repo consumer
// (DefaultManager.Acquire()'s release closure in manager.go) already
// guards against the double-release class via a `released` bool, so
// this caller-discipline requirement is not reachable through this
// module's own call sites.
func (s *ConcurrencySemaphore) Release() {
	select {
	case <-s.ch:
		s.active.Add(-1)
	default:
		// No slot to release; this is a programming error but we
		// avoid panicking.
	}
}

// ActiveCount returns the number of currently held slots.
func (s *ConcurrencySemaphore) ActiveCount() int {
	return int(s.active.Load())
}

// Max returns the maximum number of concurrent slots.
func (s *ConcurrencySemaphore) Max() int {
	return s.max
}
