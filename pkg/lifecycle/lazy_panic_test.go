package lifecycle_test

import (
	"testing"

	"digital.vasic.containers/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLazyBooter_EnsureStarted_PanicInStartFn_StateStaysConsistent
// reproduces a real state-consistency defect in LazyBooter: sync.Once
// marks itself "done" (via a deferred store) even when the wrapped
// function panics, but LazyBooter's OWN started/err bookkeeping is not
// deferred — it only runs on the normal-return path. When startFn
// panics: (1) the panic propagates out through EnsureStarted() to
// whichever caller's goroutine raced to be the one that ran once.Do's
// body — a caller that does not recover crashes its own goroutine; (2)
// `started` is left permanently at stateStarting (never reaches
// stateStarted), so Started()/IsStarting() report a stuck, wrong status
// forever; (3) because sync.Once itself IS marked done, every SUBSEQUENT
// caller's EnsureStarted() takes the fast/slow path through an
// already-fired Once, calls loadErr() (nil — err was never stored) and
// returns nil — silently reporting boot SUCCESS for a service that
// never actually started. This is the "TOCTOU on state flags" hazard
// class named in the audit brief applied to LazyBooter's own
// stateNotStarted/stateStarting/stateStarted flags (constitution/
// Constitution.md §11.4.108 sibling-primitive audit of the just-fixed
// idle.go stale-fire race).
//
// Deterministic (constitution/Constitution.md §11.4.115/§11.4.50): no
// timing race needed — this reproduces on every run given a panicking
// startFn.
func TestLazyBooter_EnsureStarted_PanicInStartFn_StateStaysConsistent(t *testing.T) {
	lb := lifecycle.NewLazyBooter(func() error {
		panic("boot exploded")
	})

	// First caller: EnsureStarted must not let an internal panic escape
	// as an uncontrolled panic — it must surface as an error return, the
	// same contract every other EnsureStarted() failure uses.
	require.NotPanics(t, func() {
		err := lb.EnsureStarted()
		assert.Error(t, err, "a panicking startFn must surface as an error, not an uncontrolled panic")
	})

	// The LazyBooter must have settled into a definitive, consistent
	// state — never permanently stuck "starting".
	assert.False(t, lb.IsStarting(),
		"LazyBooter must not be stuck reporting IsStarting()==true forever after startFn panicked")

	// A second, later caller must observe the SAME failure — not a
	// silently-fabricated success.
	err2 := lb.EnsureStarted()
	assert.Error(t, err2,
		"a later caller must see the recorded boot failure, not a fabricated nil (PASS-bluff)")
}
