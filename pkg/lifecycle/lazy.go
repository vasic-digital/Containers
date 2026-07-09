package lifecycle

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// LazyBooter ensures a service is started exactly once, deferring
// the start until the first call to EnsureStarted.
type LazyBooter struct {
	once    sync.Once
	startFn func() error
	// err holds the result of the first startFn invocation. It is
	// stored through an atomic.Pointer so concurrent GetError /
	// EnsureStarted readers never race the once.Do writer that
	// assigns it (see TestLazyBooter_GetError_ConcurrentWithEnsureStarted).
	err atomic.Pointer[error]
	// Atomic flag for lock-free status checking
	started atomic.Int32 // 0 = not started, 1 = started, 2 = starting
}

const (
	stateNotStarted int32 = iota
	stateStarting
	stateStarted
)

// NewLazyBooter creates a LazyBooter that will invoke startFn on
// the first call to EnsureStarted.
func NewLazyBooter(startFn func() error) *LazyBooter {
	return &LazyBooter{startFn: startFn}
}

// loadErr returns the stored boot error, or nil if none has been set.
func (lb *LazyBooter) loadErr() error {
	if p := lb.err.Load(); p != nil {
		return *p
	}
	return nil
}

// EnsureStarted calls the start function at most once. Subsequent
// calls return the result of the first invocation.
func (lb *LazyBooter) EnsureStarted() error {
	// Fast path: check if already started without lock. The atomic
	// load of started establishes a happens-before edge with the
	// once.Do store below, so loadErr observes the final value.
	if lb.started.Load() == stateStarted {
		return lb.loadErr()
	}

	// Slow path: attempt to start.
	//
	// Panic recovery (constitution/Constitution.md §11.4.108
	// sibling-primitive audit of the idle.go stale-fire race — a
	// "TOCTOU on state flags" hazard applied to LazyBooter's own
	// stateNotStarted/stateStarting/stateStarted flags): sync.Once
	// marks itself done via a deferred store even when the wrapped
	// function panics, but without an equivalent defer here, a
	// panicking startFn would (1) propagate the panic uncontrolled to
	// whichever caller's goroutine happened to run once.Do's body,
	// crashing it unless that caller recovers, (2) leave `started`
	// stuck at stateStarting forever (Started()/IsStarting() report a
	// wrong, permanent status), and (3) because Once itself IS done,
	// every LATER caller's EnsureStarted() would silently return a nil
	// error — a fabricated success for a service that never actually
	// started (a PASS-bluff at the lifecycle layer). Recovering here
	// converts the panic into a normal, retrievable error and settles
	// the state flag consistently for every caller, matching the
	// contract every other EnsureStarted() failure already uses.
	lb.once.Do(func() {
		lb.started.Store(stateStarting)
		defer func() {
			if r := recover(); r != nil {
				e := fmt.Errorf("lifecycle: startFn panicked: %v", r)
				lb.err.Store(&e)
			}
			lb.started.Store(stateStarted)
		}()
		if lb.startFn != nil {
			e := lb.startFn()
			lb.err.Store(&e)
		}
	})
	return lb.loadErr()
}

// Started reports whether the start function has been executed.
// This method is now lock-free and has no side effects.
func (lb *LazyBooter) Started() bool {
	return lb.started.Load() == stateStarted
}

// IsStarting reports whether the start function is currently being executed.
func (lb *LazyBooter) IsStarting() bool {
	return lb.started.Load() == stateStarting
}

// GetError returns the error from the last start attempt, if any.
func (lb *LazyBooter) GetError() error {
	return lb.loadErr()
}
