package event

import (
	"context"
	"sync"
	"time"
)

// closeSubscriberJoinTimeout bounds how long Close() waits, IN TOTAL,
// for all subscribers' delivery goroutines to finish once every one of
// them has been signalled to stop. A single handler that never returns
// (blocked on I/O with no deadline, or waiting on a channel that never
// fires) must not be able to wedge Close() — and therefore the whole
// process's graceful shutdown — forever (CT-HARDEN-EV-1). Close()
// still requests every subscriber to stop FIRST (regardless of a stuck
// sibling) so a healthy sibling is never starved of its own stop
// signal by one that hangs; this timeout only bounds how long Close()
// is willing to wait for the JOIN of the whole batch.
const closeSubscriberJoinTimeout = 2 * time.Second

// EventHandler is a callback that processes a single event.
type EventHandler func(ctx context.Context, event Event)

// EventFilter specifies which events a subscriber wants to
// receive. An empty filter matches all events.
type EventFilter struct {
	// Types restricts delivery to the listed event types. An
	// empty slice matches all types.
	Types []EventType
	// Sources restricts delivery to events from the listed
	// sources. An empty slice matches all sources.
	Sources []string
}

// SubscriptionID uniquely identifies a subscription.
type SubscriptionID string

// EventBus defines the publish/subscribe interface for system
// events.
type EventBus interface {
	// Publish sends an event to all matching subscribers.
	Publish(ctx context.Context, event Event)
	// Subscribe registers a handler for events matching the
	// filter and returns a subscription identifier.
	Subscribe(
		filter EventFilter,
		handler EventHandler,
	) SubscriptionID
	// Unsubscribe removes the subscription identified by id.
	Unsubscribe(id SubscriptionID)
}

// DefaultEventBus is a thread-safe, channel-based EventBus
// implementation.
type DefaultEventBus struct {
	mu         sync.RWMutex
	subs       map[SubscriptionID]*subscription
	nextID     uint64
	bufferSize int
	closed     bool
	closeCh    chan struct{}
}

// NewEventBus creates a DefaultEventBus. bufferSize controls the
// per-subscriber channel buffer.
//
// Honesty note (§11.4.108): bufferSize == 0 does NOT provide synchronous, guaranteed delivery.
// An earlier version of this comment claimed otherwise, which was
// false. Publish's per-subscriber
// send is ALWAYS a non-blocking best-effort send (`select ...
// default:` — see Publish's doc comment): with bufferSize == 0 (an
// unbuffered channel), that send's `default:` branch is taken, and the
// event is DROPPED, whenever the subscriber's delivery goroutine is
// not, at that exact instant, parked in its channel receive — which in
// practice is most of the time its handler is doing any real work.
//
// A genuinely blocking, guaranteed-delivery send for bufferSize == 0
// was assessed and rejected: holding Publish's read lock across a
// blocking channel send would let one slow/stuck subscriber wedge
// Close/Unsubscribe/Subscribe (which need the write lock) for the
// ENTIRE bus; releasing the lock before sending would let a concurrent
// Close()/Unsubscribe() close that subscriber's channel first, making
// the send panic. Both alternatives are worse than today's best-effort
// semantics, so bufferSize == 0 keeps the SAME best-effort,
// non-blocking, drop-when-full contract as every other buffer size —
// it is simply the smallest possible drop window, not a different
// (synchronous) delivery mode.
//
// There is no bufferSize value that guarantees delivery. Callers that
// need a subscriber to miss as few events as possible under load
// should size bufferSize generously for their workload; a larger
// buffer only shrinks the drop window, it does not eliminate it.
func NewEventBus(bufferSize int) *DefaultEventBus {
	if bufferSize < 0 {
		bufferSize = 0
	}
	return &DefaultEventBus{
		subs:       make(map[SubscriptionID]*subscription),
		bufferSize: bufferSize,
		closeCh:    make(chan struct{}),
	}
}

// Publish delivers the event to every matching subscriber. Each
// subscriber's handler is invoked in its own goroutine via the
// subscriber's buffered channel, so Publish never blocks on slow
// handlers.
func (b *DefaultEventBus) Publish(
	ctx context.Context,
	event Event,
) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}
	for _, sub := range b.subs {
		if sub.matches(event) {
			select {
			case sub.ch <- deliverableEvent{
				ctx: ctx, event: event,
			}:
			default:
				// Drop event when subscriber buffer is full.
			}
		}
	}
}

// Subscribe registers a handler and starts a goroutine that
// delivers matching events to it.
//
// Subscribe is a no-op once the bus has been Close()d: it returns the
// empty SubscriptionID sentinel without inserting a map entry or
// spawning a delivery goroutine (CT-HARDEN-EV-3). Mirrors the guard
// Publish already has (`if b.closed { return }`) — without it, a
// Subscribe() call arriving after Close() (e.g. a retry loop racing
// shutdown) would insert a map entry that Close() has already run and
// will never come back to reclaim: an unbounded leak, plus a normal-
// looking SubscriptionID with no signal the bus is actually shut down.
func (b *DefaultEventBus) Subscribe(
	filter EventFilter,
	handler EventHandler,
) SubscriptionID {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return SubscriptionID("")
	}

	b.nextID++
	id := newSubscriptionID(b.nextID)
	sub := newSubscription(id, filter, handler, b.bufferSize)
	b.subs[id] = sub
	go sub.run(b.closeCh)
	return id
}

// Unsubscribe removes a subscription and stops its delivery
// goroutine.
//
// It signals the goroutine to stop via sub.requestStop() rather than
// sub.stop(): a subscriber handler may unsubscribe its OWN subscription
// (the one-shot / unsubscribe-on-condition pattern), in which case
// Unsubscribe runs INSIDE that subscription's delivery goroutine. A
// joining stop() (`<-s.done`) there would wait for run() to return while
// run() is blocked in the very handler doing the unsubscribe — a self-
// join deadlock that wedges the goroutine and never returns.
// requestStop() only closes the channel (idempotently), which is enough
// to terminate run(); the goroutine winds down on its own without a
// join. The delete under b.mu.Lock() has already removed the sub from
// the map (waiting out any in-flight Publish holding b.mu.RLock()), so
// closing s.ch is strictly after any send that could have seen it.
func (b *DefaultEventBus) Unsubscribe(id SubscriptionID) {
	b.mu.Lock()
	sub, ok := b.subs[id]
	if ok {
		delete(b.subs, id)
	}
	b.mu.Unlock()

	if ok {
		sub.requestStop()
	}
}

// Close shuts down the event bus and all active subscriptions.
//
// A subscription's delivery goroutine may, at the moment Close is
// called, be in the middle of invoking the subscriber's handler — and a
// handler that re-enters the bus (Publish/Subscribe/Unsubscribe), a
// normal pattern for chained or derived events, needs b.mu to proceed.
// Previously b.mu was held for Close's entire body (deferred Unlock),
// so that re-entrant call blocked forever waiting for a lock Close
// would never release until the very goroutine it's blocking was done
// — a deadlock. Unsubscribe already gets this right (it releases b.mu
// before touching any subscription); Close does the same: collect+
// remove the subscriptions under the lock, release it, then signal and
// join each subscription outside the lock.
//
// CT-HARDEN-EV-1: signal-all-before-join-any. A single handler that
// never returns (blocked on I/O with no deadline, or a channel that
// never fires) must not be able to wedge Close() forever, nor starve
// any OTHER subscriber of its own stop signal. So Close() first calls
// requestStop() on EVERY subscriber (non-blocking, idempotent — safe
// regardless of order), and only THEN joins them, all together, under
// one shared closeSubscriberJoinTimeout deadline rather than an
// unbounded per-subscriber wait. A subscriber whose handler never
// returns leaves its own delivery goroutine running past the deadline
// — a known, isolated, single-goroutine leak this accepts (there is no
// way to force a running goroutine to stop from the outside) — but
// Close() itself, and every OTHER subscriber's teardown, is never held
// hostage by it.
func (b *DefaultEventBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.closeCh)

	subs := make([]*subscription, 0, len(b.subs))
	for id, sub := range b.subs {
		subs = append(subs, sub)
		delete(b.subs, id)
	}
	b.mu.Unlock()

	for _, sub := range subs {
		sub.requestStop()
	}

	deadline := time.Now().Add(closeSubscriberJoinTimeout)
	for _, sub := range subs {
		remaining := time.Until(deadline)
		sub.waitDone(remaining)
	}
}
