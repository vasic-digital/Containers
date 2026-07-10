package event

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventBus_PanickingSubscriber_Contained is a §11.4.115 RED-on-the-broken-
// artifact / GREEN-guard test with a canonical RED_MODE polarity switch.
//
// CT-HARDEN-49: a subscriber handler is invoked inside its per-subscription
// goroutine (subscriber.go run() -> s.handler) with NO recover() anywhere in
// the package, so an unrecovered panic in ANY subscriber handler crashes the
// ENTIRE process (Go runtime aborts on an unrecovered panic in any goroutine),
// taking down every other subscriber and the host application.
//
// Because a crashing process would abort `go test` itself, the scenario runs
// in a SUBPROCESS (the classic stdlib TestHelperProcess pattern). The parent
// observes the child's exit code + a stdout sentinel as the containment oracle.
//
// Polarity switch (env RED_MODE):
//
//	RED_MODE=0 (DEFAULT — the permanent regression GUARD): assert the bus
//	  CONTAINED the panic (child exits 0 and prints CONTAINED_OK). FAILS
//	  deterministically on the UNFIXED code (child crashes, exit != 0, no
//	  sentinel); PASSES on the FIXED code. This is the value the guard ships
//	  with, so a normal `go test ./...` is RED on broken and GREEN on fixed.
//	RED_MODE=1 (reproduce-the-defect polarity): assert the defect is PRESENT
//	  (child crashed, no sentinel). PASSES on UNFIXED, FAILS on FIXED.
//
// Deviation note (§11.4.115): §11.4.115's suggested canonical default is 1;
// CT-49 uses default 0 so that (a) the task's "RED deterministically FAILs on
// the unfixed code" holds for a bare `go test`, and (b) the shipped guard is
// green on fixed code without an env flag. Both polarities are provided in one
// source, satisfying the single-source RED/GREEN requirement.
func TestEventBus_PanickingSubscriber_Contained(t *testing.T) {
	if os.Getenv("CT49_HELPER") == "1" {
		runPanicSubscriberChild()
		return
	}

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestEventBus_PanickingSubscriber_Contained$",
		"-test.v",
	)
	cmd.Env = append(os.Environ(), "CT49_HELPER=1")
	out, runErr := cmd.CombinedOutput()

	childExitedZero := runErr == nil
	contained := strings.Contains(string(out), "CONTAINED_OK")
	sawPanic := strings.Contains(string(out), "panic:")

	t.Logf("child exitedZero=%v contained=%v sawPanic=%v\n----- child output -----\n%s\n------------------------",
		childExitedZero, contained, sawPanic, string(out))

	redMode := os.Getenv("RED_MODE") == "1"

	if redMode {
		// Reproduce-the-defect polarity: the UNFIXED bus must crash the whole
		// process (no containment). Proves the defect is genuinely present.
		if childExitedZero || contained {
			t.Fatalf("RED_MODE=1: expected the panicking subscriber to CRASH the process "+
				"(no containment), but child exitedZero=%v contained=%v — defect not reproduced",
				childExitedZero, contained)
		}
		return
	}

	// DEFAULT guard polarity: the bus MUST contain the panicking subscriber —
	// the sibling still fires AND the bus is still usable afterwards, and the
	// process does NOT crash.
	if !childExitedZero {
		t.Fatalf("panicking subscriber was NOT contained: child process crashed "+
			"(exit != 0); a single misbehaving subscriber took down the whole process. "+
			"sawPanic=%v", sawPanic)
	}
	if !contained {
		t.Fatalf("panicking subscriber was NOT contained: child exited 0 but did not " +
			"report CONTAINED_OK (sibling did not fire and/or bus unusable after panic)")
	}
	// Anti-bluff (§11.4): the recovered panic MUST be surfaced honestly, not
	// silently swallowed — a swallowed panic that reports success is itself a
	// bluff. The fix logs a "panicked" notice; assert it appears.
	if !strings.Contains(string(out), "panicked") {
		t.Fatalf("panic was contained but NOT surfaced: child output has no " +
			"'panicked' notice — a silently-swallowed panic is a §11.4 bluff")
	}
}

// runPanicSubscriberChild executes the real scenario inside the child process.
// It registers a panicking subscriber plus a sibling subscriber, publishes,
// then verifies (a) the sibling still fired and (b) the bus is still usable by
// delivering a second event to a fresh probe subscriber. On the UNFIXED code
// the process crashes during the settle window and never reaches CONTAINED_OK.
func runPanicSubscriberChild() {
	bus := NewEventBus(16)

	var siblingFired atomic.Int32
	var siblingWG sync.WaitGroup
	siblingWG.Add(1)

	// Misbehaving subscriber.
	bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		panic("CT49 subscriber boom")
	})
	// Sibling subscriber that MUST still receive the event.
	bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		if siblingFired.Add(1) == 1 {
			siblingWG.Done()
		}
	})

	// Let both subscriber goroutines start.
	time.Sleep(20 * time.Millisecond)

	bus.Publish(context.Background(), NewEvent(EventContainerStarted, "ct49", "boom"))

	// Settle window: on the UNFIXED code the unrecovered panic in the run()
	// goroutine aborts the process here, well before CONTAINED_OK is printed.
	time.Sleep(250 * time.Millisecond)

	// (a) sibling must have fired.
	if !waitGroupDone(&siblingWG, 2*time.Second) || siblingFired.Load() == 0 {
		os.Stdout.WriteString("NOT_CONTAINED sibling-did-not-fire\n")
		os.Exit(1)
	}

	// (b) bus must still be usable after the panic.
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	var probeFired atomic.Int32
	bus.Subscribe(EventFilter{}, func(_ context.Context, _ Event) {
		if probeFired.Add(1) == 1 {
			probeWG.Done()
		}
	})
	time.Sleep(20 * time.Millisecond)
	bus.Publish(context.Background(), NewEvent(EventHealthChanged, "ct49", "probe"))
	if !waitGroupDone(&probeWG, 2*time.Second) {
		os.Stdout.WriteString("NOT_CONTAINED bus-unusable-after-panic\n")
		os.Exit(1)
	}

	bus.Close()
	os.Stdout.WriteString("CONTAINED_OK\n")
	os.Exit(0)
}

// waitGroupDone waits for wg with a deadline; returns true if it completed.
func waitGroupDone(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
