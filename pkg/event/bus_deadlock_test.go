package event

import (
	"context"
	"testing"
	"time"
)

// TestDefaultEventBus_Close_ReentrantHandlerPublish_NoDeadlock exercises
// a handler that, while being invoked from its subscription's delivery
// goroutine, calls back into the SAME bus (Publish) — a normal pattern
// for chained/derived events — at the exact moment Close() is tearing
// the bus down.
//
// Second-pass concurrency audit finding: DefaultEventBus.Close() held
// b.mu.Lock() for its entire body, including while blocked inside
// sub.stop()'s `<-s.done` wait. If the subscription's delivery goroutine
// was, at that moment, inside the handler call (not yet back at its
// select loop) and the handler tried to re-enter the bus (Publish,
// Subscribe, or Unsubscribe), that call blocked forever trying to
// acquire b.mu — and Close() could never release b.mu because it was
// waiting (via s.done) for that very same goroutine to return. A
// classic deadlock: a blocking call made while holding a lock that the
// thing being waited on needs in order to proceed.
//
// Unsubscribe already got this right (it releases b.mu before calling
// sub.stop()); Close() did not. This test proves Close() no longer
// deadlocks in this scenario. Detected via a bounded timeout (not an
// unbounded hang) so the test itself fails cleanly rather than wedging
// the test binary.
func TestDefaultEventBus_Close_ReentrantHandlerPublish_NoDeadlock(t *testing.T) {
	bus := NewEventBus(4)

	entered := make(chan struct{})
	proceed := make(chan struct{})
	handlerReturned := make(chan struct{})

	first := true
	bus.Subscribe(EventFilter{}, func(ctx context.Context, ev Event) {
		if ev.Type != EventContainerStarted || !first {
			return
		}
		first = false
		close(entered)
		<-proceed

		// Re-entrant call into the SAME bus from inside a handler — a
		// realistic "publish a derived event in response" pattern.
		bus.Publish(ctx, NewEvent(EventContainerStopped, "test", "derived"))
		close(handlerReturned)
	})

	bus.Publish(
		context.Background(),
		NewEvent(EventContainerStarted, "test", "trigger"),
	)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started executing")
	}

	closeDone := make(chan struct{})
	go func() {
		bus.Close()
		close(closeDone)
	}()

	// Give Close() a moment to acquire b.mu and reach sub.stop()'s
	// blocking wait before we let the handler try to re-enter the bus.
	time.Sleep(50 * time.Millisecond)
	close(proceed)

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("DEADLOCK: bus.Close() did not return within 2s — the " +
			"handler's re-entrant Publish() call is blocked on b.mu, " +
			"which Close() holds while waiting for that same handler " +
			"to finish")
	}

	select {
	case <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("handler's re-entrant Publish() call never returned")
	}
}
