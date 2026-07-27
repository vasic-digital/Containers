// Package lifecycle white-box guard suite for batch CT-HARDEN-LIFE-HARD,
// Wave-20 (constitution/Constitution.md §11.4.115 RED→GREEN polarity guards).
//
// This file lives in `package lifecycle` — not the external `lifecycle_test`
// package used by the other suites — because the LIFE-1 and LIFE-2 guards
// drive the unexported acquireRaceHook / startFollowerWaitHook package seams
// (the pkg/vm/teardown.go killByPortHook idiom) to reproduce the target
// interleavings deterministically, exactly as idle_test.go drives the
// unexported clock seam. LIFE-4 needs no seam — a fail-then-succeed
// orchestrator reproduces the poisoned-lazy-booter defect deterministically
// on its own.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the manager's lease /
// re-validation / start-coalescing / lazy-retry LOGIC behaves correctly under
// the reproduced interleaving injected through the seam. They do not exercise
// a live container runtime — the orchestrator/health dependencies are
// controllable fakes, which is exactly the point of a device-independent,
// host-side logic guard. Each guard is GREEN-polarity (committed-default =
// GUARD): it PASSES on the fixed tree and FAILS on a surgical revert of the
// specific fix under test (evidence captured in the batch report).
package lifecycle

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"digital.vasic.containers/pkg/compose"
)

// wave20Orch is a controllable ComposeOrchestrator for the wave-20 guards.
// upFunc, when set, decides each Up() call's outcome (1-based call index) and
// may block, letting a guard hold a boot mid-flight in a deterministic window.
type wave20Orch struct {
	upCalls   atomic.Int32
	downCalls atomic.Int32
	upFunc    func(n int32) error
}

func (o *wave20Orch) Up(
	_ context.Context, _ compose.ComposeProject, _ ...compose.UpOption,
) error {
	n := o.upCalls.Add(1)
	if o.upFunc != nil {
		return o.upFunc(n)
	}
	return nil
}

func (o *wave20Orch) Down(
	_ context.Context, _ compose.ComposeProject, _ ...compose.DownOption,
) error {
	o.downCalls.Add(1)
	return nil
}

func (o *wave20Orch) Status(
	_ context.Context, _ compose.ComposeProject,
) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (o *wave20Orch) Logs(
	_ context.Context, _ compose.ComposeProject, _ string,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

var _ compose.ComposeOrchestrator = (*wave20Orch)(nil)

// TestWave20_LIFE1_AcquireRevalidatesAfterConcurrentStop proves the LIFE-1
// dead-lease fix: an Acquire whose service is torn down (manually or by idle)
// in the window between the semaphore acquisition and the post-acquire state
// re-validation MUST fail — never hand back a live-looking lease on a stopped
// service (§11.4.108) — and MUST roll back the lease it counted so the busy
// predicate is not left permanently non-zero.
//
// The concurrent Stop is injected deterministically through acquireRaceHook
// (the nil-default package seam) rather than by racing the real-clock idle
// timer, so the exact interleaving is reproduced every run (§11.4.50/§11.4.115).
func TestWave20_LIFE1_AcquireRevalidatesAfterConcurrentStop(t *testing.T) {
	orch := &wave20Orch{}
	m := NewDefaultManager(orch, nil)
	if err := m.Register(ServiceSpec{Name: "svc", ComposeFile: "c.yml"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()
	if err := m.Start(ctx, "svc"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Inject exactly one Stop into the post-semaphore / pre-revalidation window.
	var once sync.Once
	prev := acquireRaceHook
	acquireRaceHook = func() {
		once.Do(func() { _ = m.Stop(ctx, "svc") })
	}
	defer func() { acquireRaceHook = prev }()

	rel, err := m.Acquire(ctx, "svc")
	if err == nil {
		if rel != nil {
			rel()
		}
		t.Fatalf("LIFE-1: Acquire returned a live-looking lease on a service " +
			"torn down mid-acquire; expected an error (dead-lease bluff)")
	}

	// The lease counted before the concurrent Stop must have been rolled back.
	m.mu.Lock()
	got := m.services["svc"].activeLeases.Load()
	m.mu.Unlock()
	if got != 0 {
		t.Fatalf("LIFE-1: activeLeases=%d after rolled-back Acquire, want 0 "+
			"(the counted lease leaked)", got)
	}
}

// TestWave20_LIFE2_CoalescedStartFollowerGetsLeaderError proves the LIFE-2
// coalesced-Start fix: a follower that coalesces onto an in-flight boot
// ("starting") MUST block and return the leader's REAL boot outcome, not a
// premature nil. The leader's boot is failed here (Up returns an error), so a
// follower that returned nil would be a fabricated success (§11.4.1).
//
// Sequencing is deterministic: the orchestrator's Up() blocks (holding the
// leader in "starting") until the follower has reached its wait point (signaled
// via startFollowerWaitHook), then the boot is released to fail.
func TestWave20_LIFE2_CoalescedStartFollowerGetsLeaderError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	bootErr := errors.New("leader boot exploded")
	var enterOnce sync.Once

	orch := &wave20Orch{
		upFunc: func(_ int32) error {
			enterOnce.Do(func() { close(entered) })
			<-release
			return bootErr
		},
	}
	m := NewDefaultManager(orch, nil)
	if err := m.Register(ServiceSpec{Name: "svc", ComposeFile: "c.yml"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	followerReached := make(chan struct{})
	var followerOnce sync.Once
	prevHook := startFollowerWaitHook
	startFollowerWaitHook = func() {
		followerOnce.Do(func() { close(followerReached) })
	}
	defer func() { startFollowerWaitHook = prevHook }()

	var leaderErr, followerErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); leaderErr = m.Start(ctx, "svc") }()

	<-entered // leader is inside Up(); state=="starting"

	wg.Add(1)
	go func() { defer wg.Done(); followerErr = m.Start(ctx, "svc") }()

	<-followerReached // follower observed "starting" and is about to block on op.done
	close(release)    // leader's Up() returns bootErr → boot fails → op closed with the error
	wg.Wait()

	if leaderErr == nil {
		t.Fatalf("LIFE-2 precondition: leader Start should have failed")
	}
	if followerErr == nil {
		t.Fatalf("LIFE-2: coalesced follower Start returned nil (fabricated " +
			"success) while the leader boot actually FAILED")
	}
	if !errors.Is(followerErr, bootErr) {
		t.Fatalf("LIFE-2: follower err = %v, want it to carry the leader's %v",
			followerErr, bootErr)
	}
	// Exactly one real boot happened — the follower coalesced, it did not re-boot.
	if n := orch.upCalls.Load(); n != 1 {
		t.Fatalf("LIFE-2: orchestrator Up called %d times, want 1 "+
			"(the follower must coalesce onto the leader's boot)", n)
	}
}

// TestWave20_LIFE2_LeaderPanicFollowerGetsErrorNotWedged proves the LIFE-2
// leader-panic fix: when the leader's boot PANICS (orchestrator.Up here) while
// a follower is coalesced onto it (blocked on op.done), the follower MUST wake
// with a REAL non-nil error, never a fabricated nil success for a boot that
// panicked (§11.4.1/§11.4.108), AND the service MUST NOT be left wedged in
// "starting" — a subsequent Start MUST genuinely re-attempt.
//
// Without the recover in the leader defer, the named retErr is still nil on a
// panic, so the plain defer publishes op.err=nil (follower fabricated success)
// and the error-path state resets are skipped (state stuck at "starting"
// forever). The surgical revert of the recover reproduces both: the follower
// gets nil AND the re-attempt is short-circuited (upCalls stays 1) — a genuine
// `--- FAIL`.
//
// HONEST BOUNDARY (§11.4.107): this guard proves the manager's coalescing
// LOGIC under the reproduced leader-panic interleaving injected through the
// startFollowerWaitHook seam with a controllable panicking orchestrator fake —
// it does not exercise a live runtime. The leader re-panics (preserving its own
// crash semantics), so the leader Start runs in a goroutine with its own
// recover — mirroring how lazy_panic_test.go's EnsureStarted recover contains
// the panic — so the re-panic does not tear down the test process.
func TestWave20_LIFE2_LeaderPanicFollowerGetsErrorNotWedged(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once

	orch := &wave20Orch{
		upFunc: func(n int32) error {
			if n == 1 {
				enterOnce.Do(func() { close(entered) })
				<-release
				panic("leader boot panicked inside orchestrator.Up")
			}
			// A genuine re-attempt (call 2+) succeeds, so a non-wedged manager
			// reaches "running" on the subsequent Start.
			return nil
		},
	}
	m := NewDefaultManager(orch, nil)
	if err := m.Register(ServiceSpec{Name: "svc", ComposeFile: "c.yml"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	followerReached := make(chan struct{})
	var followerOnce sync.Once
	prevHook := startFollowerWaitHook
	startFollowerWaitHook = func() {
		followerOnce.Do(func() { close(followerReached) })
	}
	defer func() { startFollowerWaitHook = prevHook }()

	var followerErr error
	var leaderPanicked atomic.Bool
	var wg sync.WaitGroup

	// Leader in its own goroutine WITH its own recover: the leader defer
	// re-panics to preserve crash semantics, so a direct Start caller sees the
	// panic — contain it here exactly as the lazy path's recover would.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				leaderPanicked.Store(true)
			}
		}()
		_ = m.Start(ctx, "svc")
	}()

	<-entered // leader is inside Up(); state=="starting"

	wg.Add(1)
	go func() { defer wg.Done(); followerErr = m.Start(ctx, "svc") }()

	<-followerReached // follower observed "starting"; about to block on op.done
	close(release)    // leader's Up() panics → defer recovers, resets state, sets op.err, re-panics
	wg.Wait()

	// Invariant 1: the coalesced follower receives a REAL non-nil error — not a
	// fabricated nil success for a boot that panicked.
	if followerErr == nil {
		t.Fatalf("LIFE-2 panic: coalesced follower Start returned nil " +
			"(fabricated success) while the leader boot PANICKED")
	}
	// The leader re-panicked (its own crash/propagation semantics preserved).
	if !leaderPanicked.Load() {
		t.Fatalf("LIFE-2 panic precondition: leader Start should have re-panicked")
	}

	// Invariant 2: state is NOT wedged in "starting". A subsequent Start must
	// genuinely re-attempt the boot (Up called again → succeeds → "running"),
	// not short-circuit on a stale closed op.
	if err := m.Start(ctx, "svc"); err != nil {
		t.Fatalf("LIFE-2 panic: subsequent Start failed %v; the service was left "+
			"wedged after the leader panic (never re-attempted)", err)
	}
	st, err := m.Status("svc")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "running" {
		t.Fatalf("LIFE-2 panic: state=%q after re-attempt, want \"running\" "+
			"(service wedged post-panic)", st.State)
	}
	if n := orch.upCalls.Load(); n != 2 {
		t.Fatalf("LIFE-2 panic: orchestrator Up called %d times, want 2 "+
			"(1 panicked boot + 1 genuine re-attempt); a wedged \"starting\" "+
			"state short-circuits the re-attempt so Up is never called again", n)
	}
}

// TestWave20_LIFE4_LazyBooterRecoversAfterFailedFirstBoot proves the LIFE-4
// poisoned-lazy-booter fix: after a lazy service's FIRST boot fails, the next
// Acquire MUST genuinely re-boot (a fresh LazyBooter) rather than return the
// same cached first-boot error forever. The orchestrator fails Up() only on
// its first call, so a correct manager reaches a running state on the second
// Acquire.
func TestWave20_LIFE4_LazyBooterRecoversAfterFailedFirstBoot(t *testing.T) {
	orch := &wave20Orch{
		upFunc: func(n int32) error {
			if n == 1 {
				return errors.New("first boot fails")
			}
			return nil
		},
	}
	m := NewDefaultManager(orch, nil)
	if err := m.Register(ServiceSpec{Name: "lazy", LazyBoot: true, ComposeFile: "c.yml"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	// First Acquire: lazy boot fails.
	if _, err := m.Acquire(ctx, "lazy"); err == nil {
		t.Fatalf("LIFE-4 precondition: first Acquire should fail (first boot errors)")
	}

	// Second Acquire: MUST re-boot (fresh LazyBooter), not return the stale
	// cached first-boot error forever.
	rel, err := m.Acquire(ctx, "lazy")
	if err != nil {
		t.Fatalf("LIFE-4: second Acquire returned a stale/poisoned error %v; the "+
			"lazy service was never re-booted after its failed first boot", err)
	}
	if rel == nil {
		t.Fatalf("LIFE-4: second Acquire returned a nil ReleaseFunc")
	}
	rel()

	if n := orch.upCalls.Load(); n != 2 {
		t.Fatalf("LIFE-4: orchestrator Up called %d times, want 2 "+
			"(1 failed boot + 1 successful re-boot)", n)
	}
}
