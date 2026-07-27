// Package lifecycle white-box guard suite for batch CT-HARDEN-LIFE-HARD
// SECOND pass (LIFE2), Wave-20 DEEPER (constitution/Constitution.md §11.4.118
// loop-until-dry + §11.4.115 RED→GREEN polarity guards).
//
// This file lives in `package lifecycle` — like its sibling
// wave20_lifehard_test.go — because the LIFE2 guards drive the unexported
// wave20Orch fake and the manager's internal m.mu / entry.state / entry.startOp
// fields to reproduce a Stop-vs-in-flight-boot interleaving deterministically.
//
// The NEW defect this pass surfaces (missed by the first LIFE pass, which
// covered Acquire-vs-Stop dead-lease (LIFE-1), coalesced-Start follower/panic
// (LIFE-2), and poisoned-lazy-booter (LIFE-4)): a leader Start() commits
// entry.state="running" UNCONDITIONALLY after its boot sequence, even when a
// concurrent Stop() has already transitioned the service out of "starting" AND
// run compose Down() to tear the containers down. The leader then resurrects a
// torn-down service to "running" — a §11.4.108 "reports Running while the
// underlying service is down" bluff — and, on the coalescing path, hands every
// blocked follower a fabricated nil success for a service that was stopped.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the manager's start-commit
// LOGIC honors a concurrent Stop under the reproduced interleaving injected
// through the blocking wave20Orch fake — they do not exercise a live container
// runtime (the orchestrator/health dependencies are controllable fakes, which
// is exactly the point of a device-independent host-side logic guard). Each
// guard is GREEN-polarity: it PASSES on the fixed tree and FAILS on a surgical
// one-line revert of the ownership guard under test.
package lifecycle

import (
	"context"
	"sync"
	"testing"
)

// TestWave20_LIFE2_StopDuringStartingNoResurrect proves the LIFE2-1 fix: when a
// Stop() lands while a leader Start() is mid-boot ("starting") — running
// compose Down() and moving the service to "stopped" — the leader MUST NOT
// subsequently overwrite that "stopped" back to "running". Doing so reports a
// service whose containers were already torn down (Down ran) as running
// (§11.4.108 dead-service-reported-running).
//
// The interleave is deterministic: the orchestrator's Up() blocks (holding the
// leader in "starting") until the test has driven a full Stop() through, then
// the boot is released. The health checker is nil, so with the bug the leader
// unconditionally marks "running" after Up() returns — no health gate masks it.
func TestWave20_LIFE2_StopDuringStartingNoResurrect(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once

	orch := &wave20Orch{
		upFunc: func(_ int32) error {
			enterOnce.Do(func() { close(entered) })
			<-release // hold the leader in "starting" until the Stop has run
			return nil
		},
	}
	// nil health checker: no health gate to mask the resurrection.
	m := NewDefaultManager(orch, nil)
	if err := m.Register(ServiceSpec{Name: "svc", ComposeFile: "c.yml"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	var leaderErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); leaderErr = m.Start(ctx, "svc") }()

	<-entered // leader is inside Up(); state=="starting"

	// A concurrent Stop tears the service down (Down runs) and sets "stopped".
	if err := m.Stop(ctx, "svc"); err != nil {
		t.Fatalf("stop during starting: %v", err)
	}

	close(release) // let the leader's Up() return; leader reaches its commit
	wg.Wait()

	// Precondition: the Stop genuinely tore the service down.
	if n := orch.downCalls.Load(); n != 1 {
		t.Fatalf("LIFE2-1 precondition: Down called %d times, want 1 "+
			"(the concurrent Stop must have torn the service down)", n)
	}

	// Core invariant: the leader must NOT have resurrected the torn-down
	// service to "running". Down ran, so "running" is a §11.4.108 dead-service
	// -reported-running bluff.
	st, err := m.Status("svc")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "stopped" {
		t.Fatalf("LIFE2-1: state=%q after a Stop tore the service down "+
			"mid-boot; want \"stopped\" — the leader resurrected a torn-down "+
			"service to running (§11.4.108 dead-service-reported-running)",
			st.State)
	}

	// The leader Start must report the concurrent stop, not a fabricated nil.
	if leaderErr == nil {
		t.Fatalf("LIFE2-1: leader Start returned nil while its boot was " +
			"cancelled by a concurrent Stop (fabricated success)")
	}

	// After the honest "stopped", a fresh Start must still work end-to-end.
	if err := m.Start(ctx, "svc"); err != nil {
		t.Fatalf("LIFE2-1: subsequent Start failed %v; the entry was left "+
			"un-restartable after the concurrent-stop bail", err)
	}
	st, _ = m.Status("svc")
	if st.State != "running" {
		t.Fatalf("LIFE2-1: state=%q after a clean re-Start, want \"running\"",
			st.State)
	}
}

// TestWave20_LIFE2_StopDuringStartingFollowerNotFabricatedSuccess proves the
// same LIFE2-1 fix from the coalescing follower's angle: a follower blocked on
// the leader's in-flight boot MUST NOT receive a fabricated nil success when a
// concurrent Stop tore the service down before the leader committed. Without
// the ownership guard, the leader marks "running" and publishes op.err=nil, so
// every coalesced follower wakes reporting success for a service the Stop
// already tore down (§11.4.1/§11.4.108).
//
// Sequencing is deterministic: the leader blocks in Up(); the follower reaches
// its wait point (startFollowerWaitHook); a full Stop() is driven through; then
// the leader's boot is released.
func TestWave20_LIFE2_StopDuringStartingFollowerNotFabricatedSuccess(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once

	orch := &wave20Orch{
		upFunc: func(_ int32) error {
			enterOnce.Do(func() { close(entered) })
			<-release
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

	var leaderErr, followerErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); leaderErr = m.Start(ctx, "svc") }()

	<-entered // leader inside Up(); state=="starting"

	wg.Add(1)
	go func() { defer wg.Done(); followerErr = m.Start(ctx, "svc") }()

	<-followerReached // follower observed "starting"; about to block on op.done

	// Concurrent Stop tears the service down while leader + follower are pinned.
	if err := m.Stop(ctx, "svc"); err != nil {
		t.Fatalf("stop during starting: %v", err)
	}

	close(release) // leader's Up() returns → leader reaches its commit
	wg.Wait()

	if n := orch.downCalls.Load(); n != 1 {
		t.Fatalf("LIFE2-1 follower precondition: Down called %d times, want 1", n)
	}
	if leaderErr == nil {
		t.Fatalf("LIFE2-1 follower: leader Start returned nil while its boot " +
			"was cancelled by a concurrent Stop (fabricated success)")
	}
	// The coalesced follower must receive the SAME honest non-nil outcome, not
	// a fabricated nil success for a service the Stop tore down.
	if followerErr == nil {
		t.Fatalf("LIFE2-1 follower: coalesced follower Start returned nil " +
			"(fabricated success) while a concurrent Stop tore the service " +
			"down before the leader committed (§11.4.108)")
	}
	// Exactly one real boot happened.
	if n := orch.upCalls.Load(); n != 1 {
		t.Fatalf("LIFE2-1 follower: Up called %d times, want 1 "+
			"(the follower must coalesce, not re-boot)", n)
	}
	// State is honestly "stopped", never a resurrected "running".
	st, err := m.Status("svc")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "stopped" {
		t.Fatalf("LIFE2-1 follower: state=%q, want \"stopped\" (the leader "+
			"resurrected a torn-down service to running)", st.State)
	}
}
