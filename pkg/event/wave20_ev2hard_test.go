package event

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wave20_ev2hard_test.go — CT-HARDEN-EV-1/EV-2/EV-3 (Wave-20) §11.4.115
// GREEN-polarity regression guards for pkg/event. Each guard was proven
// genuine by a SURGICAL REVERT of its fix in bus.go / subscriber.go: edit
// the fix OUT, run the guard, observe a real `--- FAIL` (captured in the
// batch handoff, not reproduced here), edit the fix back IN, observe
// `--- PASS`. No RED_MODE env toggle is needed here (unlike
// wave19_event_hardening_test.go) — the source-level revert IS the RED
// polarity for these three findings.
//
// HONEST BOUNDARY (§11.4.107/§11.4.108): these are in-package unit guards
// with no live container runtime. They exercise the real DefaultEventBus
// state machine directly (subscription map, channels, goroutines) — no
// mocks, no fakes (§11.4.27). Being in-package (`package event`, not
// `event_test`), they may read unexported fields (`bus.subs`, `bus.mu`,
// `sub.done`) purely for OBSERVATION, never to bypass the public API under
// test.

// ---------------------------------------------------------------------
// CT-HARDEN-EV-1 — Close() must not be wedged forever by one stuck
// handler, and must still signal+join every OTHER (healthy) subscriber.
// ---------------------------------------------------------------------

// TestDefaultEventBus_Close_StuckHandlerDoesNotWedgeClose is the permanent
// regression guard for CT-HARDEN-EV-1.
//
// Root cause (pre-fix): Close() tore down subscriptions SEQUENTIALLY via
// the old joining sub.stop() (requestStop() + `<-s.done`). A handler that
// never returns (blocked on I/O with no deadline, or a channel that never
// fires) makes that join block forever, so Close() itself never returns —
// hanging the caller's graceful shutdown indefinitely. Because the loop
// never reaches the OTHER subscriptions, a subscriber that iterates AFTER
// the stuck one in map order is starved of even its own requestStop()
// signal (Go map iteration order is randomized, so — unlike a slice — the
// stuck handler can land anywhere in the sequence; with only one stuck
// subscriber among the set, the sequential join loop reaches it sooner or
// later regardless of position and hangs there for good).
//
// Fix: Close() now (1) calls requestStop() on EVERY subscriber FIRST,
// unconditionally, before joining any of them — so a subscriber later in
// iteration order is never starved of its own stop signal by an earlier
// stuck sibling — then (2) joins all of them under a single SHARED
// deadline (closeSubscriberJoinTimeout), not an unbounded per-subscriber
// wait. A permanently stuck handler's own delivery goroutine is a known,
// isolated, single-goroutine leak this fix accepts (there is no way to
// force a running goroutine to stop from the outside) — the goal is that
// Close() itself, and every OTHER subscriber's teardown, is never held
// hostage by it.
func TestDefaultEventBus_Close_StuckHandlerDoesNotWedgeClose(t *testing.T) {
	bus := NewEventBus(4)

	stuckEntered := make(chan struct{})
	neverRelease := make(chan struct{})
	t.Cleanup(func() {
		// Release the permanently-blocked handler so its goroutine can
		// finally exit once the test is done observing it — good hygiene,
		// not required for the assertions above (which complete before
		// this runs).
		close(neverRelease)
	})

	normalDone := make(chan struct{})
	var normalOnce sync.Once

	stuckID := bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		close(stuckEntered)
		<-neverRelease // blocks forever until t.Cleanup releases it
	})
	normalID := bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		normalOnce.Do(func() { close(normalDone) })
	})

	// Capture the subscription pointers directly (in-package access) so we
	// can inspect each goroutine's `done` channel independently of what
	// Close() does to the bus's own bookkeeping (Close deletes every
	// entry from bus.subs before a caller could otherwise observe it).
	bus.mu.RLock()
	stuckSub := bus.subs[stuckID]
	normalSub := bus.subs[normalID]
	bus.mu.RUnlock()
	if stuckSub == nil || normalSub == nil {
		t.Fatal("setup: subscription lookup failed")
	}

	// bufferSize==4 means a single Publish() enqueues into BOTH
	// subscribers' buffered channels regardless of whether their
	// goroutines have started consuming yet (a buffered send does not
	// require a ready receiver up to capacity) — no retry/scheduling-luck
	// needed to deliver the bootstrap event.
	bus.Publish(
		context.Background(),
		NewEvent(EventContainerStarted, "ev1", "trigger"),
	)

	select {
	case <-stuckEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("setup: stuck handler never entered dispatch within 2s")
	}
	select {
	case <-normalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("setup: normal handler never fired within 2s")
	}
	// Give the normal subscriber's run() goroutine a moment to loop back
	// to its select statement after the handler returned, so Close()
	// signals it while genuinely idle (the realistic steady-state case).
	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() {
		bus.Close()
		close(closeDone)
	}()

	const bound = 3 * time.Second
	select {
	case <-closeDone:
	case <-time.After(bound):
		t.Fatalf("DEADLOCK: bus.Close() did not return within %s — a "+
			"single subscriber handler that never returns wedged the "+
			"sequential join loop, so Close() (graceful shutdown) hangs "+
			"forever (CT-HARDEN-EV-1 regression)", bound)
	}

	// The healthy sibling MUST have been signalled + joined even though
	// the stuck subscriber is (permanently) never going to finish.
	select {
	case <-normalSub.done:
	case <-time.After(1 * time.Second):
		t.Fatal("the normal subscriber's delivery goroutine was never " +
			"signalled/joined by Close() — starved by the stuck sibling " +
			"(CT-HARDEN-EV-1 regression)")
	}

	// Sanity / documentation of the accepted trade-off: the stuck
	// subscriber's goroutine is still running — Close() cannot force a
	// genuinely stuck goroutine to stop from the outside. Not asserted as
	// a failure; it records the known, isolated, single-goroutine leak
	// this fix accepts in exchange for Close() itself never hanging.
	select {
	case <-stuckSub.done:
		t.Fatal("test setup invariant broken: the stuck handler returned " +
			"unexpectedly before its release channel was closed")
	default:
	}
}

// ---------------------------------------------------------------------
// CT-HARDEN-EV-2 — bufferSize==0 doc-honesty (§11.4.108): the doc
// promised "synchronous delivery" but Publish's non-blocking
// select/default send drops nearly every event delivered to a busy
// subscriber on an unbuffered channel. Chosen fix (b): correct the doc to
// describe the REAL best-effort/drop-on-busy behavior rather than (a)
// making the send genuinely blocking — see the doc comment on
// NewEventBus for the full assessment of why (a) was rejected (it
// reintroduces an unacceptable Publish-blocks-and-can-wedge-Close hazard
// held across b.mu.RLock, or a send-on-closed-channel panic risk if the
// lock were released first).
// ---------------------------------------------------------------------

// TestDefaultEventBus_ZeroBuffer_PublishNeverBlocks_AndDropsWhileSubscriberBusy
// reproduces and locks in the REAL runtime behavior of a bufferSize==0
// bus: Publish is ALWAYS a non-blocking best-effort send (never blocks,
// even when the receiving subscriber is busy) and genuinely DROPS (never
// queues) an event published while the subscriber's handler has not
// returned to its channel receive. This is independent of the doc-fix
// (the runtime behavior does not change) — it is the evidence that the
// OLD doc's "synchronous delivery" claim was false, and that the NEW doc
// (TestNewEventBus_DocComment_HonestAboutZeroBufferSemantics, below)
// describes the bus's actual, unchanged contract.
func TestDefaultEventBus_ZeroBuffer_PublishNeverBlocks_AndDropsWhileSubscriberBusy(t *testing.T) {
	bus := NewEventBus(0)
	defer bus.Close()

	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	var deliveries atomic.Int32

	bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		deliveries.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
	})

	ctx := context.Background()
	ev := NewEvent(EventContainerStarted, "ev2", "bootstrap")

	// Bootstrap: retry Publish until the handler is confirmed inside
	// dispatch (blocked on <-release). With bufferSize==0 a send only
	// succeeds when the subscriber's goroutine is AT its receive at that
	// exact instant, so a single attempt is not guaranteed to land. This
	// loop tolerates ordinary goroutine-scheduling startup latency — it
	// does not gamble on the defect under test, which is exercised only
	// AFTER this bootstrap completes and is itself checked via bounded,
	// deterministic timing (below), never via "did this one attempt
	// happen to land".
	started := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-entered:
			started = true
		default:
			bus.Publish(ctx, ev)
			time.Sleep(time.Millisecond)
		}
		if started {
			break
		}
	}
	if !started {
		t.Fatal("setup: handler never entered dispatch within the 2s " +
			"bootstrap budget")
	}

	// The handler is now confirmed BUSY (blocked on <-release). Any
	// further Publish() to this zero-buffer bus during this window MUST
	// NOT block (proving Publish is best-effort, never a genuine blocking
	// hand-off) and MUST NOT be delivered (proving the event is
	// genuinely DROPPED, not queued for later delivery).
	for i := 0; i < 20; i++ {
		before := time.Now()
		bus.Publish(ctx, NewEvent(EventContainerStarted, "ev2", "busy-drop"))
		elapsed := time.Since(before)
		if elapsed > 200*time.Millisecond {
			t.Fatalf("Publish() blocked for %s on a bufferSize==0 bus "+
				"while the subscriber was busy — Publish must always be "+
				"best-effort and never block, regardless of buffer size "+
				"(CT-HARDEN-EV-2 regression)", elapsed)
		}
	}

	if got := deliveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 delivery while the subscriber was "+
			"busy, got %d — an event published while busy was delivered "+
			"instead of dropped", got)
	}

	close(release)

	// Give the subscriber's goroutine a bounded window to loop back to
	// its select statement; if any busy-window publish had actually been
	// queued (impossible with a genuine bufferSize==0 channel, asserted
	// here as belt-and-suspenders), a second dispatch would occur here.
	time.Sleep(200 * time.Millisecond)
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("expected deliveries to remain 1 after releasing the "+
			"busy handler, got %d — an event published while the "+
			"subscriber was busy was queued rather than genuinely dropped "+
			"(CT-HARDEN-EV-2 regression)", got)
	}
}

// TestNewEventBus_DocComment_HonestAboutZeroBufferSemantics is the
// doc-honesty guard for CT-HARDEN-EV-2 (§11.4.108). It reads bus.go's own
// source (relative to this test file's own directory, so it is
// cwd-independent) and asserts NewEventBus's doc comment no longer makes
// the false "synchronous delivery" promise and instead honestly discloses
// the real best-effort, drop-on-busy semantics that
// TestDefaultEventBus_ZeroBuffer_PublishNeverBlocks_AndDropsWhileSubscriberBusy
// (above) proves is the bus's actual, unchanged runtime behavior.
func TestNewEventBus_DocComment_HonestAboutZeroBufferSemantics(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	busGoPath := filepath.Join(filepath.Dir(thisFile), "bus.go")
	src, err := os.ReadFile(busGoPath)
	if err != nil {
		t.Fatalf("reading %s: %v", busGoPath, err)
	}
	content := string(src)

	const falseClaim = "use 0 for synchronous delivery"
	if strings.Contains(content, falseClaim) {
		t.Fatalf("bus.go's NewEventBus doc comment still contains the "+
			"false claim %q — bufferSize==0 does NOT provide synchronous, "+
			"guaranteed delivery (Publish's send is always a non-blocking "+
			"best-effort select, so an unbuffered channel drops nearly "+
			"every event delivered while the subscriber is busy); "+
			"§11.4.108 requires the doc to state the REAL behavior "+
			"(CT-HARDEN-EV-2 regression)", falseClaim)
	}

	const honestMarker = "does NOT provide synchronous, guaranteed delivery"
	if !strings.Contains(content, honestMarker) {
		t.Fatalf("bus.go's NewEventBus doc comment is missing the honest "+
			"disclosure %q describing bufferSize==0's real best-effort, "+
			"drop-on-busy semantics (CT-HARDEN-EV-2 regression)", honestMarker)
	}
}

// ---------------------------------------------------------------------
// CT-HARDEN-EV-3 — Subscribe() after Close() must not leak a map entry
// or hand out a normal-looking subscription id.
// ---------------------------------------------------------------------

// TestDefaultEventBus_SubscribeAfterClose_NoMapEntryLeak is the permanent
// regression guard for CT-HARDEN-EV-3.
//
// Root cause (pre-fix): Subscribe() had no post-Close() guard (unlike
// Publish(), which checks b.closed). After Close() had already run,
// Subscribe() would still insert a new map entry and spawn a delivery
// goroutine, returning a normal-looking SubscriptionID with no signal
// that the bus is shut down. Because Close() already ran (and does not
// run again), that map entry is NEVER reclaimed — an unbounded leak under
// any shutdown-ordering race that keeps calling Subscribe() after Close()
// (e.g. a retry loop).
//
// Fix: Subscribe() now checks b.closed (mirroring Publish()) and, when
// the bus is already closed, returns the empty SubscriptionID sentinel
// without inserting a map entry or spawning a goroutine.
func TestDefaultEventBus_SubscribeAfterClose_NoMapEntryLeak(t *testing.T) {
	bus := NewEventBus(4)
	bus.Close()

	id := bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {})

	if id != SubscriptionID("") {
		t.Fatalf("Subscribe() after Close() returned %q — expected the "+
			"empty sentinel SubscriptionID signalling the bus is shut "+
			"down (CT-HARDEN-EV-3 regression)", id)
	}

	bus.mu.RLock()
	n := len(bus.subs)
	bus.mu.RUnlock()
	if n != 0 {
		t.Fatalf("Subscribe() after Close() left %d entr(y/ies) in the "+
			"bus's subscription map — Close() already ran and will never "+
			"reclaim it, an unbounded leak under any shutdown-ordering "+
			"race that keeps calling Subscribe() after Close() "+
			"(CT-HARDEN-EV-3 regression)", n)
	}
}

// =====================================================================
// Wave-20 DEEPER (§11.4.118 loop-until-dry) — SECOND-PASS findings.
// Named TestWave20_EV2_<Desc>. Each is a §11.4.115 GREEN-polarity guard
// proven genuine by a SURGICAL single-line REVERT of its fix (the RED
// polarity is the source-level revert, deterministic — not a timing
// gamble), captured in this stream's anti-tautology run.
// =====================================================================

// TestWave20_EV2_NilHandlerRejected is the permanent regression guard for
// CT-HARDEN-EV2-1 (Wave-20 DEEPER): a nil handler passed to Subscribe()
// must be rejected up front with the empty SubscriptionID sentinel — never
// accepted into a live map entry with a spawned delivery goroutine.
//
// Root cause (pre-fix): Subscribe() validated only b.closed, never the
// handler. A nil handler was accepted: it inserted a map entry and spawned
// go sub.run(...). Then EVERY matching Publish() reached
// subscription.dispatch, which called s.handler(...) == nil(...) and
// panicked (invalid memory address / nil-pointer dereference). dispatch's
// recover() contained the panic so the process did not crash — but that is
// precisely why it was insidious: an unbounded stream of recovered
// nil-deref panics, each with a full debug.Stack() dump, was logged on
// every matching event, for a "zombie" subscription that could never do
// useful work. A programming error (nil handler) was silently swallowed
// into permanent log spam + a wasted goroutine instead of failing fast.
//
// Fix: Subscribe() rejects handler == nil with the same empty-sentinel
// contract it already uses for a closed bus, BEFORE inserting a map entry
// or spawning a goroutine — so the zombie subscription is never created.
//
// This is an in-package unit guard: it reads bus.subs (unexported) purely
// to OBSERVE that no map entry leaked, never to bypass the public API.
func TestWave20_EV2_NilHandlerRejected(t *testing.T) {
	bus := NewEventBus(8)
	defer bus.Close()

	// A healthy subscriber alongside the nil one — used below to prove the
	// bus is still fully usable (the nil rejection is inert, not corrupting).
	var healthy sync.WaitGroup
	healthy.Add(1)
	var healthyOnce sync.Once
	var healthyHits atomic.Int32
	healthyID := bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		healthyHits.Add(1)
		healthyOnce.Do(healthy.Done)
	})
	if healthyID == SubscriptionID("") {
		t.Fatal("setup: a non-nil handler was unexpectedly rejected")
	}

	// The subject under test: a nil handler.
	nilID := bus.Subscribe(EventFilter{}, nil)

	// (1) A nil handler MUST be rejected with the empty sentinel — the same
	// signal a closed bus gives — never a normal-looking id.
	if nilID != SubscriptionID("") {
		t.Fatalf("Subscribe(_, nil) returned %q — a nil handler must be "+
			"rejected with the empty SubscriptionID sentinel, not accepted "+
			"as a live subscription whose every matching Publish() invokes "+
			"nil() and panics (recovered into unbounded log spam) "+
			"(CT-HARDEN-EV2-1 regression)", nilID)
	}

	// (2) No zombie map entry / delivery goroutine may have been created for
	// the nil handler: only the one healthy subscriber must be present.
	bus.mu.RLock()
	n := len(bus.subs)
	_, nilPresent := bus.subs[nilID]
	bus.mu.RUnlock()
	if n != 1 || nilPresent {
		t.Fatalf("Subscribe(_, nil) left the subscription map at size %d "+
			"(nil entry present=%v) — expected exactly the 1 healthy "+
			"subscriber and NO entry for the rejected nil handler; a "+
			"zombie nil-handler subscription leaks a goroutine and panics "+
			"on every matching Publish (CT-HARDEN-EV2-1 regression)",
			n, nilPresent)
	}

	// (3) Anti-bluff: the bus is still fully functional after the rejection —
	// a matching Publish reaches the healthy subscriber, and the rejected
	// nil handler contributes nothing (no panic, no spam, no delivery).
	bus.Publish(context.Background(),
		NewEvent(EventContainerStarted, "ev2-nil", "probe"))

	select {
	case <-waitGroupCh(&healthy):
	case <-time.After(2 * time.Second):
		t.Fatal("healthy subscriber never fired after a nil handler was " +
			"rejected — the rejection must be inert, leaving the bus fully " +
			"usable (CT-HARDEN-EV2-1 regression)")
	}
	if got := healthyHits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 delivery to the healthy subscriber, "+
			"got %d", got)
	}
}

// waitGroupCh returns a channel closed when wg is Done. Small local helper
// so the nil-handler guard can select on the WaitGroup with a timeout
// without importing anything new.
func waitGroupCh(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}
