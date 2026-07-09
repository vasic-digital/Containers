package lifecycle

import (
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

	is.stopped = true
	is.mu.Unlock()

	if is.onIdle != nil {
		is.onIdle()
	}
}
