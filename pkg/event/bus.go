package event

import (
	"context"
	"sync"
)

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
// per-subscriber channel buffer; use 0 for synchronous delivery.
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
func (b *DefaultEventBus) Subscribe(
	filter EventFilter,
	handler EventHandler,
) SubscriptionID {
	b.mu.Lock()
	defer b.mu.Unlock()

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
// sub.stop() closes the subscription's channel and then blocks
// (`<-s.done`) until the subscription's delivery goroutine returns. That
// goroutine may, at the moment Close is called, be in the middle of
// invoking the subscriber's handler — and a handler that re-enters the
// bus (Publish/Subscribe/Unsubscribe), a normal pattern for chained or
// derived events, needs b.mu to proceed. Previously b.mu was held for
// Close's entire body (deferred Unlock), so that re-entrant call blocked
// forever waiting for a lock Close would never release until the very
// goroutine it's blocking was done — a deadlock. Unsubscribe already
// gets this right (it releases b.mu before calling sub.stop()); Close
// must do the same: collect+remove the subscriptions under the lock,
// release it, then stop each subscription outside the lock.
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
		sub.stop()
	}
}
