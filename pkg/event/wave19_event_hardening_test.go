package event

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestDefaultEventBus_SelfUnsubscribeFromHandler_NoDeadlock is the
// permanent regression guard for CT-HARDEN-EVHD-1: a subscriber handler
// that unsubscribes its OWN subscription (the one-shot /
// unsubscribe-on-condition pattern) must not deadlock.
//
// The handler runs on its subscription's own delivery goroutine
// (run -> dispatch -> handler). If Unsubscribe joined that goroutine
// (`<-s.done`) from inside the handler, it would wait for run() to
// return while run() is blocked in the very handler doing the join — a
// self-join deadlock that wedges the goroutine and never returns. The
// fix splits signal-from-join: Unsubscribe calls requestStop() (an
// idempotent close of s.ch, NO join), so run() winds down on its own.
//
// §11.4.115 polarity switch: the DEFAULT (RED_MODE unset or "0") is the
// shipped GUARD — it asserts the self-Unsubscribe returns within a
// bounded 3s budget and FAILs (rather than hanging the suite forever) on
// a regression. Set RED_MODE=1 to run the reproduce polarity: on the
// UNFIXED tree it observes the deadlock (PASS — defect reproduced); on
// the FIXED tree it inverts and FAILs, proving the guard is not blind.
func TestDefaultEventBus_SelfUnsubscribeFromHandler_NoDeadlock(t *testing.T) {
	redMode := os.Getenv("RED_MODE") == "1"

	bus := NewEventBus(4)

	var id SubscriptionID
	// idReady establishes a happens-before so the handler reads `id`
	// only after the main goroutine has assigned it (race-free under -race).
	idReady := make(chan struct{})
	unsubReturned := make(chan struct{})

	id = bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		<-idReady
		// Self-unsubscribe from inside the handler — executes on THIS
		// subscription's own delivery goroutine. Must not self-join.
		bus.Unsubscribe(id)
		close(unsubReturned)
	})
	close(idReady)

	bus.Publish(
		context.Background(),
		NewEvent(EventContainerStarted, "test", "trigger"),
	)

	const budget = 3 * time.Second
	select {
	case <-unsubReturned:
		if redMode {
			t.Fatal("RED_MODE=1: expected the self-unsubscribe deadlock " +
				"to reproduce, but Unsubscribe returned — the defect is " +
				"already fixed (reproduce polarity correctly inverted)")
		}
		// GUARD PASS: the handler's self-Unsubscribe returned in time.
	case <-time.After(budget):
		if redMode {
			t.Logf("RED_MODE=1: reproduced — a handler that unsubscribes "+
				"itself did not return within %s (self-join deadlock)", budget)
			return
		}
		t.Fatal("DEADLOCK: a handler that unsubscribes its own " +
			"subscription did not return within 3s — Unsubscribe joined " +
			"the subscription's own delivery goroutine on itself " +
			"(CT-HARDEN-EVHD-1 regression)")
	}
}
