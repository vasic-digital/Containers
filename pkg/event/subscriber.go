package event

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
)

// deliverableEvent pairs an event with the context active at the
// time of publishing.
type deliverableEvent struct {
	ctx   context.Context
	event Event
}

// subscription holds the state of a single event subscriber.
type subscription struct {
	id      SubscriptionID
	filter  EventFilter
	handler EventHandler
	ch      chan deliverableEvent
	done    chan struct{}
}

// newSubscriptionID creates a SubscriptionID from a uint64.
func newSubscriptionID(n uint64) SubscriptionID {
	return SubscriptionID(fmt.Sprintf("sub-%d", n))
}

// newSubscription creates a subscription ready to be started.
func newSubscription(
	id SubscriptionID,
	filter EventFilter,
	handler EventHandler,
	bufferSize int,
) *subscription {
	return &subscription{
		id:      id,
		filter:  filter,
		handler: handler,
		ch:      make(chan deliverableEvent, bufferSize),
		done:    make(chan struct{}),
	}
}

// run processes events from the channel until stop is called or
// closeCh is closed.
func (s *subscription) run(closeCh <-chan struct{}) {
	defer close(s.done)
	for {
		select {
		case de, ok := <-s.ch:
			if !ok {
				return
			}
			s.dispatch(de)
		case <-closeCh:
			return
		}
	}
}

// dispatch invokes the subscriber's handler for a single delivery,
// containing any panic so one misbehaving subscriber cannot crash the
// process or kill this subscription's delivery goroutine. A recovered
// panic is surfaced honestly (logged with the subscriber id, the event
// that triggered it, the recovered value, and the stack) — never
// silently swallowed — while sibling subscribers and subsequent
// deliveries to this subscriber proceed unaffected.
func (s *subscription) dispatch(de deliverableEvent) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf(
				"event: subscriber %s panicked handling %q event "+
					"from %q: %v\n%s",
				s.id, de.event.Type, de.event.Source, r,
				debug.Stack(),
			)
		}
	}()
	s.handler(de.ctx, de.event)
}

// stop closes the event channel and waits for the goroutine to
// finish.
func (s *subscription) stop() {
	close(s.ch)
	<-s.done
}

// matches returns true if the event satisfies the subscription's
// filter criteria.
func (s *subscription) matches(e Event) bool {
	if len(s.filter.Types) > 0 {
		found := false
		for _, t := range s.filter.Types {
			if t == e.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(s.filter.Sources) > 0 {
		found := false
		for _, src := range s.filter.Sources {
			if src == e.Source {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
