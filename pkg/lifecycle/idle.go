package lifecycle

import (
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// IdleShutdown monitors a service for inactivity and triggers a
// callback when the idle timeout elapses without any Touch calls.
type IdleShutdown struct {
	mu        sync.Mutex
	timeout   time.Duration
	onIdle    func()
	timer     timerHandle
	stopped   bool
	lastTouch time.Time
	clk       clock
	// busy, when non-nil and returning true, means the monitored resource
	// is genuinely in use (e.g. an Acquire lease is held) and MUST NOT be
	// idle-shut-down. fire() re-arms instead of firing onIdle while busy. A
	// nil busy preserves the original behavior for callers that do not set
	// it. Set once before Start() via setBusy (LIFE-(a) IDLE-vs-LEASE fix).
	busy func() bool
}

// setBusy installs the in-use predicate consulted by fire(). It must be
// called before Start() arms the timer (no concurrent fire is possible
// yet), so it takes no lock. The predicate MUST be non-blocking (an atomic
// load) because fire() calls it while holding is.mu.
func (is *IdleShutdown) setBusy(f func() bool) {
	is.busy = f
}

// NewIdleShutdown creates an IdleShutdown that fires onIdle after
// timeout elapses without a Touch.
func NewIdleShutdown(timeout time.Duration, onIdle func()) *IdleShutdown {
	return newIdleShutdownWithClock(timeout, onIdle, realClock{})
}

// newIdleShutdownWithClock is the clock-injectable constructor used
// only by tests (constitution/Constitution.md §11.4.108 — the seam
// stays unexported; NewIdleShutdown's public signature and behavior
// are unchanged for every production caller).
func newIdleShutdownWithClock(timeout time.Duration, onIdle func(), clk clock) *IdleShutdown {
	return &IdleShutdown{
		timeout: timeout,
		onIdle:  onIdle,
		clk:     clk,
	}
}

// Start begins the idle countdown. If Start has already been called
// it resets the timer.
func (is *IdleShutdown) Start() {
	is.mu.Lock()
	defer is.mu.Unlock()

	is.stopped = false
	is.lastTouch = is.clk.Now()

	if is.timer != nil {
		is.timer.Stop()
	}
	is.timer = is.clk.AfterFunc(is.timeout, is.fire)
}

// Touch resets the idle countdown to the full timeout duration.
func (is *IdleShutdown) Touch() {
	is.mu.Lock()
	defer is.mu.Unlock()

	if is.stopped || is.timer == nil {
		return
	}
	is.lastTouch = is.clk.Now()
	is.timer.Reset(is.timeout)
}

// Stop cancels the idle countdown. The onIdle callback will not be
// invoked after Stop returns.
func (is *IdleShutdown) Stop() {
	is.mu.Lock()
	defer is.mu.Unlock()

	is.stopped = true
	if is.timer != nil {
		is.timer.Stop()
		is.timer = nil
	}
}

// LastTouch returns the time of the most recent Touch (or Start).
func (is *IdleShutdown) LastTouch() time.Time {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.lastTouch
}

// fire is called by the timer when the idle period elapses.
//
// Real race (constitution/Constitution.md §11.4.108): the timer
// expires and the runtime launches fire() concurrently with a
// caller's Touch(). If Touch() wins the lock race and reschedules
// the timer for a fresh full timeout, fire() must NOT treat the
// stale expiration as genuine idleness — it must verify the idle
// period has actually elapsed relative to lastTouch before
// committing to shutdown. If a Touch beat this fire, reschedule for
// the remaining duration instead of firing onIdle.
func (is *IdleShutdown) fire() {
	is.mu.Lock()
	if is.stopped {
		is.mu.Unlock()
		return
	}

	sinceLastTouch := is.clk.Now().Sub(is.lastTouch)
	if sinceLastTouch < is.timeout {
		// A Touch happened after this timer was scheduled to fire.
		// The idle period has NOT genuinely elapsed — do not shut
		// down. Reschedule for the remaining duration and return.
		remaining := is.timeout - sinceLastTouch
		if is.timer != nil {
			is.timer.Reset(remaining)
		}
		is.mu.Unlock()
		return
	}

	// Timeout elapsed, but if the resource is still in use (a lease is
	// held) it is NOT genuinely idle — re-arm rather than reclaiming it out
	// from under an active holder (LIFE-(a) IDLE-vs-LEASE fix). is.busy()
	// must be a non-blocking check (an atomic load) so it is safe to call
	// while holding is.mu; it never takes another lock, so no lock ordering
	// is introduced.
	if is.busy != nil && is.busy() {
		is.lastTouch = is.clk.Now()
		if is.timer != nil {
			is.timer.Reset(is.timeout)
		}
		is.mu.Unlock()
		return
	}

	is.stopped = true
	is.mu.Unlock()

	if is.onIdle != nil {
		// SW6-1: the timer's runtime goroutine (clock.go realClock.AfterFunc
		// → time.AfterFunc) runs fire() on a stack with NO recover(), so a
		// panic inside the consumer-supplied onIdle() would crash the whole
		// daemon. Contain it here, mirroring the module's own established
		// convention in pkg/event/subscriber.go dispatch(): a recovered panic
		// is surfaced honestly (logged with the recovered value and the stack)
		// — never silently swallowed — while the process survives. fire() has
		// no logger in scope, so this mirrors dispatch()'s package-level
		// log.Printf form exactly. The non-panic path is unchanged: onIdle()
		// runs, no recover triggers, and fire() has already reset/rescheduled
		// (or committed to shutdown) above.
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf(
						"lifecycle: idle-shutdown onIdle callback "+
							"panicked: %v\n%s",
						r, debug.Stack(),
					)
				}
			}()
			is.onIdle()
		}()
	}
}
