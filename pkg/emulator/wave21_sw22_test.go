package emulator

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestContainerized_WaitForBoot_TimeoutWrapsAuthorizeADBFailure_SW22 is the
// SW2-2 anti-tautology regression guard (§11.4.115 RED→GREEN polarity).
//
// Defect: WaitForBoot captured authorizeADB's error and then discarded it
// (`_ = err`); Containerized has no logger, so NOTHING surfaced it. When the
// subsequent boot-poll timed out, the returned error only said "WaitForBoot
// timed out ..." and never mentioned that adb authorization had failed —
// reproducing the exact diagnostic blindness that made the 2026-06-23
// adb-authorization blocker hard to root-cause.
//
// This test forces BOTH failures at once through the existing fakeExecutor
// seam: authorizeADB's container `cp` fails (podman errors), while adb's
// `connect` succeeds at the exec layer but `getprop sys.boot_completed` never
// returns "1", so the poll runs to the deadline and times out. It then asserts
// the returned error contains BOTH the timeout text AND the underlying
// authorization-failure text.
//
// Bluff-Audit (mutation rehearsal, §11.4.115): revert the fix in
// containerized.go WaitForBoot back to `_ = err` + the plain timeout message
// (drop the `authErr` capture + the `(adb authorization also failed: %w)`
// wrap). Re-run: the "adb authorization also failed" / "authorizeADB"
// assertions FAIL because the error no longer carries the auth cause. Restore
// the fix → GREEN. A test that passed on the reverted source would be blind.
func TestContainerized_WaitForBoot_TimeoutWrapsAuthorizeADBFailure_SW22(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			// authorizeADB's `podman cp <container>:<key> <host>` fails,
			// so authorizeADB returns a non-nil error (authErr != nil).
			"podman": {Out: []byte("Error: no such container\n"), Err: errors.New("exit 125")},
			// adb: `connect` returns a nil error (so WaitForBoot reaches the
			// poll loop rather than its early `adb connect: ...` return), and
			// `getprop sys.boot_completed` returns a non-"1" value so the boot
			// is never observed complete and the poll runs to the deadline.
			"adb": {Out: []byte("offline\n"), Err: nil},
		},
	}
	c, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	if err != nil {
		t.Fatalf("NewContainerized: %v", err)
	}
	// authorizeADB refuses to run without a container name (it returns
	// "no container name (Boot not called)"). Set it directly (same package)
	// to isolate WaitForBoot from Boot, so `podman` is invoked ONLY by
	// authorizeADB's `cp` and the failure is unambiguously the auth path.
	c.containerName = "sw22-fake-container"
	// authorizeADB creates a host temp dir (os.MkdirTemp) BEFORE the cp;
	// remove it so no stray dir survives the test (§11.4.14 cleanup).
	t.Cleanup(func() {
		if c.adbKeyTmpDir != "" {
			_ = os.RemoveAll(c.adbKeyTmpDir)
		}
	})

	// Tiny timeout so the poll deadline elapses fast. context.Background()
	// ensures the caller ctx never errors, so WaitForBoot returns via its own
	// internal-timeout branch (the SW2-2 wrapped-error path), not ctx.Err().
	_, werr := c.WaitForBoot(context.Background(), 5575, 40*time.Millisecond)
	if werr == nil {
		t.Fatal("WaitForBoot must return an error when sys.boot_completed never reaches 1")
	}
	msg := werr.Error()

	// Assertion 1: the base timeout text is preserved.
	if !strings.Contains(msg, "WaitForBoot timed out") {
		t.Errorf("returned error must state the boot timeout; got: %v", werr)
	}
	// Assertion 2 (the SW2-2 fix): the authorizeADB failure is surfaced INTO
	// the returned error. Reverting the fix drops this text → RED.
	if !strings.Contains(msg, "adb authorization also failed") {
		t.Errorf("SW2-2: timeout error must wrap the authorizeADB failure; got: %v", werr)
	}
	// Assertion 3: the wrapped error carries the underlying authorizeADB
	// cause, not just a generic phrase.
	if !strings.Contains(msg, "authorizeADB") {
		t.Errorf("SW2-2: wrapped error must carry the underlying authorizeADB cause; got: %v", werr)
	}
}
