// Package lifecycle (white-box test file — `package lifecycle`, like its
// sibling idle_test.go and wave20_life*hard_test.go, NOT lifecycle_test)
// because it drives the fire() callback through the unexported
// newIdleShutdownWithClock + fakeClock seam and asserts on fire()'s panic
// containment directly. It REUSES the fakeClock / newFakeClock helpers
// declared in idle_test.go (same package) — it does not redeclare them.
package lifecycle

import (
	"bytes"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestIdleShutdown_OnIdlePanicContained is the SW6-1 regression guard
// (constitution/Constitution.md §11.4.115 RED->GREEN, §11.4.6). It proves
// that a panic inside the consumer-supplied onIdle() callback is CONTAINED
// inside fire() rather than propagating out of it.
//
// Why propagation matters (the real defect): production wires fire() via
// clock.go realClock.AfterFunc -> time.AfterFunc, so fire() runs on the
// timer's runtime goroutine, which has NO recover() on its stack. A panic
// that escapes fire() there is fatal to the WHOLE daemon. This mirrors the
// module's own established convention in pkg/event/subscriber.go dispatch(),
// which already wraps its untrusted handler(...) in recover() "containing any
// panic so one misbehaving subscriber cannot crash the process".
//
// The subtlety that makes this a GENUINE fixed-vs-broken discriminator
// (§11.4.115, not a tautology): a fakeClock inline fire() call runs on THIS
// test goroutine, whereas the real realClock runs fire() on the runtime
// goroutine where an escaping panic is fatal. So the test does NOT rely on
// "the daemon crashes" (unobservable in-process); instead it captures, with
// its own recover, whether the panic ESCAPED the fire() call:
//
//	WITHOUT the SW6-1 wrap -> panic propagates out of fire() -> escaped=true
//	                          -> assert FAILs (RED).
//	WITH the SW6-1 wrap     -> panic contained inside fire()  -> escaped=false
//	                          -> fire() returns normally, assert PASSes (GREEN).
//
// It also asserts the panic is surfaced HONESTLY (logged, never silently
// swallowed) exactly as dispatch() does — a second independent RED->GREEN
// signal (no log call exists without the wrap, so the buffer is empty in RED).
func TestIdleShutdown_OnIdlePanicContained(t *testing.T) {
	start := time.Now()
	clk := newFakeClock(start)
	timeout := 100 * time.Millisecond

	// Capture the module's package-level log output (dispatch() and the
	// SW6-1 wrap both use log.Printf) to prove the recovered panic is
	// surfaced, not silently swallowed. Restored on exit. Only this
	// (synchronous) test goroutine drives fire(), so nothing else writes
	// the buffer concurrently.
	var logBuf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	var onIdleCalls atomic.Int32
	is := newIdleShutdownWithClock(timeout, func() {
		onIdleCalls.Add(1)
		panic("SW6-1 boom: consumer onIdle blew up")
	}, clk)

	// Arm the timer at t=0 and capture the fire() callback the runtime
	// would otherwise invoke on its own goroutine.
	is.Start()
	fireCallback := clk.LastFunc()
	if fireCallback == nil {
		t.Fatal("Start() did not register an AfterFunc callback")
	}

	// Advance past the timeout so fire() genuinely reaches the onIdle()
	// call site: sinceLastTouch == timeout (>= timeout, so no reschedule),
	// busy is nil (so no lease re-arm) -> onIdle() is invoked.
	clk.SetNow(start.Add(timeout))

	// Drive fire() exactly as the runtime goroutine would, capturing
	// whether the panic ESCAPED it. This inner recover is the RED-baseline
	// instrument, NOT the fix under test — the fix is fire()'s OWN recover;
	// this test's recover only observes whether one was needed here.
	escaped := func() (didEscape bool) {
		defer func() {
			if r := recover(); r != nil {
				didEscape = true
			}
		}()
		fireCallback()
		return false
	}()

	// GREEN with the SW6-1 wrap; RED without it (panic propagates out of
	// fire(), which on the realClock runtime goroutine crashes the daemon).
	assert.False(t, escaped,
		"SW6-1: a panicking onIdle() must be CONTAINED inside fire() — it "+
			"escaped, and on the realClock timer goroutine that panic would "+
			"crash the whole daemon (no recover() on that stack)")

	// The callback was actually reached (guards against a vacuous pass where
	// fire() never called onIdle at all).
	assert.Equal(t, int32(1), onIdleCalls.Load(),
		"fire() must have invoked onIdle() exactly once (else the containment "+
			"assertion is vacuous)")

	// Honest surfacing (mirrors dispatch()): the recovered panic is logged,
	// never silently swallowed. Empty in the RED build (no log call exists
	// without the wrap) -> a second independent RED->GREEN signal.
	logged := logBuf.String()
	assert.Contains(t, logged, "panicked",
		"SW6-1: the recovered panic must be surfaced honestly via log (as "+
			"pkg/event/subscriber.go dispatch() does), not silently swallowed")
	assert.Contains(t, logged, "SW6-1 boom",
		"the log must carry the recovered panic value")
}
