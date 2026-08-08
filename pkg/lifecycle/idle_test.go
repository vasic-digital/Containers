// Package lifecycle (white-box test file). This file is intentionally
// `package lifecycle` rather than `lifecycle_test` (unlike its siblings
// in this directory) because TestIdleShutdown_StaleFireAfterTouch needs
// the unexported newIdleShutdownWithClock seam to drive the fire()/Touch()
// race deterministically (constitution/Constitution.md §11.4.115). Go
// permits both `lifecycle` and `lifecycle_test` test files in the same
// package directory; all other *_test.go files here remain `lifecycle_test`
// and are unaffected.
package lifecycle

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIdleShutdown_Fires(t *testing.T) {
	var fired atomic.Int32
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() { fired.Add(1) },
	)

	is.Start()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), fired.Load())
}

func TestIdleShutdown_TouchResets(t *testing.T) {
	var fired atomic.Int32
	is := NewIdleShutdown(
		150*time.Millisecond,
		func() { fired.Add(1) },
	)

	is.Start()
	time.Sleep(100 * time.Millisecond)
	is.Touch() // reset: should not fire yet
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), fired.Load())

	// Wait for the full timeout after touch.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), fired.Load())
}

func TestIdleShutdown_StopPrevents(t *testing.T) {
	var fired atomic.Int32
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() { fired.Add(1) },
	)

	is.Start()
	is.Stop()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), fired.Load())
}

func TestIdleShutdown_TouchAfterStop(t *testing.T) {
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() {},
	)
	is.Stop()
	// Should not panic.
	is.Touch()
}

func TestIdleShutdown_LastTouch(t *testing.T) {
	is := NewIdleShutdown(
		time.Hour, func() {},
	)

	is.Start()
	defer is.Stop()

	t1 := is.LastTouch()
	time.Sleep(10 * time.Millisecond)
	is.Touch()
	t2 := is.LastTouch()

	assert.True(t, t2.After(t1))
}

func TestIdleShutdown_MultipleStarts(t *testing.T) {
	var fired atomic.Int32
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() { fired.Add(1) },
	)

	is.Start()
	is.Start() // should reset
	time.Sleep(200 * time.Millisecond)
	// Should only fire once.
	assert.Equal(t, int32(1), fired.Load())
}

func TestIdleShutdown_NilCallback(t *testing.T) {
	// Test that nil onIdle callback does not panic.
	is := NewIdleShutdown(
		50*time.Millisecond,
		nil,
	)

	is.Start()
	time.Sleep(100 * time.Millisecond)
	// Should not panic; the fire() method checks for nil.
}

func TestIdleShutdown_TouchBeforeStart(t *testing.T) {
	var fired atomic.Int32
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() { fired.Add(1) },
	)

	// Touch before Start should be a no-op (timer is nil).
	is.Touch()

	is.Start()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), fired.Load())
}

func TestIdleShutdown_StopMultipleTimes(t *testing.T) {
	is := NewIdleShutdown(
		100*time.Millisecond,
		func() {},
	)

	is.Start()
	is.Stop()
	// Calling Stop again should not panic.
	is.Stop()
}

func TestIdleShutdown_LastTouchBeforeStart(t *testing.T) {
	is := NewIdleShutdown(
		time.Hour, func() {},
	)

	// LastTouch before Start returns zero time.
	t1 := is.LastTouch()
	assert.True(t, t1.IsZero())
}

func TestIdleShutdown_FireAfterStopRace(t *testing.T) {
	// This test covers the race condition between timer firing and Stop().
	// With a nanosecond timeout, the timer may fire before Stop() acquires
	// the lock. In this case, the callback legitimately runs because it
	// acquired the lock first and checked stopped=false.
	//
	// The key invariant is: callback fires AT MOST once per Start(), and
	// after Stop() returns, no additional callbacks occur.

	for i := 0; i < 100; i++ {
		var fired atomic.Int32
		is := NewIdleShutdown(
			time.Nanosecond, // Very short to increase race chance.
			func() { fired.Add(1) },
		)

		is.Start()
		// Immediately stop to race with the timer.
		is.Stop()

		// Wait for any pending timer to complete.
		time.Sleep(time.Millisecond)

		// The callback should fire at most once (0 or 1).
		// It may be 1 if the timer fired before Stop() acquired the lock.
		count := fired.Load()
		assert.True(t, count <= 1, "callback should fire at most once, got %d", count)

		// Verify no additional callbacks occur after Stop() + sleep.
		time.Sleep(time.Millisecond)
		assert.Equal(t, count, fired.Load(), "no additional callbacks after Stop()")
	}
}

func TestIdleShutdown_FireIdempotent(t *testing.T) {
	// Test that once idle shutdown fires, it sets stopped to true
	// and subsequent timer events (if any) are ignored.
	var fired atomic.Int32
	is := NewIdleShutdown(
		50*time.Millisecond,
		func() { fired.Add(1) },
	)

	is.Start()
	// Wait for first fire.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), fired.Load())

	// Start again without explicitly calling Stop (this should reset).
	is.Start()
	time.Sleep(100 * time.Millisecond)
	// Should fire again since we called Start.
	assert.Equal(t, int32(2), fired.Load())
}

func TestIdleShutdown_ConcurrentStartStop(t *testing.T) {
	// Stress test: concurrent Start and Stop calls to cover race paths.
	var wg sync.WaitGroup
	var fired atomic.Int32

	for i := 0; i < 50; i++ {
		is := NewIdleShutdown(
			time.Microsecond,
			func() { fired.Add(1) },
		)

		wg.Add(2)
		go func() {
			defer wg.Done()
			is.Start()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond)
			is.Stop()
		}()
	}

	wg.Wait()
	// Wait for any pending timers.
	time.Sleep(10 * time.Millisecond)
	// We don't assert specific count - just ensure no panic/race.
}

// ---------------------------------------------------------------------
// §11.4.115 clock-injection seam: deterministic reproduction of the
// stale-fire-after-touch race.
// ---------------------------------------------------------------------

// fakeTimerHandle is the fakeClock's timerHandle. It records the
// currently-scheduled duration and whether Stop() was called; it never
// fires on its own — the test invokes the captured callback manually.
type fakeTimerHandle struct {
	mu      sync.Mutex
	dur     time.Duration
	stopped bool
}

func (h *fakeTimerHandle) Stop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	wasRunning := !h.stopped
	h.stopped = true
	return wasRunning
}

func (h *fakeTimerHandle) Reset(d time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	wasRunning := !h.stopped
	h.dur = d
	h.stopped = false
	return wasRunning
}

// fakeClock is a deterministic clock test double. Now() is fully
// controlled by the test via SetNow/Advance; AfterFunc captures the
// scheduled callback (LastFunc) without ever invoking it automatically
// — the test decides exactly when a "stale" fire callback runs,
// reproducing the real runtime interleaving where the timer goroutine
// is already past the AfterFunc dispatch but blocked on is.mu.Lock()
// while a concurrent Touch() proceeds first.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	lastFunc func()
	lastH    *fakeTimerHandle
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) SetNow(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) timerHandle {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := &fakeTimerHandle{dur: d}
	c.lastFunc = f
	c.lastH = h
	return h
}

// LastFunc returns the most recently registered AfterFunc callback,
// captured but never auto-invoked.
func (c *fakeClock) LastFunc() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastFunc
}

// TestIdleShutdown_StaleFireAfterTouch reproduces, deterministically,
// the real race described in constitution/Constitution.md §11.4.108:
// the idle timer expires and the runtime launches fire(), which blocks
// on is.mu.Lock(); concurrently Touch() wins the lock race, updates
// lastTouch, and reschedules the timer; fire() then proceeds. The FIX
// under test requires fire() to notice, via lastTouch, that a Touch
// genuinely landed after the timer was scheduled, and to NOT shut the
// service down in that case (§11.4.115 RED->GREEN polarity: see
// red_run.log / green_run.log captured under qa-results/idle_race_fix/,
// produced by temporarily removing the sinceLastTouch guard below and
// re-running this exact test — never left in the committed tree per
// §11.4.84).
func TestIdleShutdown_StaleFireAfterTouch(t *testing.T) {
	start := time.Now()
	clk := newFakeClock(start)
	timeout := 100 * time.Millisecond

	var onIdleCalls atomic.Int32
	is := newIdleShutdownWithClock(timeout, func() { onIdleCalls.Add(1) }, clk)

	// Start the idle countdown at t=0. This schedules a fire callback
	// for t=100ms and sets lastTouch=t0.
	is.Start()
	fireCallback := clk.LastFunc()
	if fireCallback == nil {
		t.Fatal("Start() did not register an AfterFunc callback")
	}

	// Simulate activity happening just before the stale timer expires:
	// advance the fake clock to t=95ms (still within the timeout
	// window relative to lastTouch=t0) and Touch(). Touch() updates
	// lastTouch to t=95ms and reschedules the timer for a fresh 100ms
	// (i.e. to fire at t=195ms).
	clk.SetNow(start.Add(95 * time.Millisecond))
	is.Touch()

	// Now invoke the STALE captured fire callback manually — this is
	// exactly the runtime interleaving where the original timer's
	// goroutine had already been dispatched by the runtime scheduler
	// (from the original Start()-registered AfterFunc, "due" at
	// t=100ms) but only now acquires is.mu.Lock(), AFTER Touch()
	// already ran and moved lastTouch forward. Advance the clock to
	// t=100ms (the original stale fire's nominal expiry) so sinceLast-
	// Touch = 100ms-95ms = 5ms, genuinely less than the 100ms timeout.
	clk.SetNow(start.Add(100 * time.Millisecond))
	fireCallback()

	// The guarded fix MUST NOT treat this stale fire as genuine
	// idleness — recent activity (the Touch at t=95ms) is still within
	// the timeout window. onIdle must NOT have been called.
	assert.Equal(t, int32(0), onIdleCalls.Load(),
		"stale fire() must not invoke onIdle when a Touch landed after the timer was scheduled")

	// The service must still be alive: a genuine idle period measured
	// from the LATEST Touch (t=95ms) must still fire onIdle once it
	// truly elapses. Advance to t=95ms+100ms=195ms and invoke whatever
	// callback is now current (the fix reschedules for the remaining
	// duration; drive it the same way the real runtime would: capture
	// and invoke the latest registered callback).
	clk.SetNow(start.Add(195 * time.Millisecond))
	latest := clk.LastFunc()
	if latest == nil {
		t.Fatal("no fire callback registered after the stale-fire reschedule")
	}
	latest()

	assert.Equal(t, int32(1), onIdleCalls.Load(),
		"genuine idleness measured from the latest Touch must still fire onIdle exactly once")
}
