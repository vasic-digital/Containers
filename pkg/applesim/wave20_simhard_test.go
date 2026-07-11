package applesim

// Wave-20 SIM-HARD guards. Each guard asserts the FIXED behaviour so `go test`
// is GREEN on the committed tree; the comment above each documents the EXACT
// one-line SURGICAL REVERT that flips it RED (per §11.4.115 committed-guard
// discipline). All guards use the injectable exec seam (fakeExec / blockingExec)
// or a benign POSIX helper process — NO real xcrun/simctl (§11.4.27).
//
// fakeExec + newToolWithExec are defined in applesim_test.go (same package).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// SIM-1 — BootAndWait bounds the WHOLE operation (Boot + bootstatus + Resolve).
// -----------------------------------------------------------------------------

// blockingExec routes each simctl subcommand (args[1]) to either a hang (until
// ctx is cancelled, exactly as a real exec.CommandContext-bound process would)
// or an immediate empty success, so a guard can wedge `boot` or `list` at will.
type blockingExec struct {
	block map[string]bool
}

func (b *blockingExec) run(ctx context.Context, _ string, args ...string) (string, error) {
	sub := ""
	if len(args) > 1 {
		sub = args[1]
	}
	if b.block[sub] {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", nil
}

// assertReturnsWithin fails (rather than hanging the whole test binary forever)
// if fn does not return within deadline — a hang surfaces as FAIL.
func assertReturnsWithin(t *testing.T, deadline time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("operation did not return within %s — it hung (SIM-1 regression: `timeout` must bound the whole BootAndWait)", deadline)
	}
}

func TestBootAndWait_bootLegBounded_SIM1(t *testing.T) {
	// SIM-1 GUARD (boot leg). `boot` hangs (respecting ctx); the fix threads the
	// bounded bctx into Boot, so `timeout` makes BootAndWait return promptly with
	// an error instead of hanging forever on the unbounded caller ctx.
	//
	// SURGICAL REVERT (flips this RED): in BootAndWait change
	//     if err := t.Boot(bctx, udid); err != nil {
	// back to
	//     if err := t.Boot(ctx, udid); err != nil {
	tool := &Tool{Path: "/usr/bin/xcrun", exec: (&blockingExec{block: map[string]bool{"boot": true}}).run}
	assertReturnsWithin(t, 2*time.Second, func() {
		if _, err := tool.BootAndWait(context.Background(), "UDID-1", 80*time.Millisecond); err == nil {
			t.Errorf("expected BootAndWait to surface the bounded-boot error, got nil")
		}
	})
}

func TestBootAndWait_resolveLegBounded_SIM1(t *testing.T) {
	// SIM-1 GUARD (resolve leg). boot + bootstatus succeed; `list` (used by the
	// trailing Resolve) hangs. The fix threads the bounded bctx into Resolve, so
	// BootAndWait returns promptly instead of hanging on the caller ctx.
	//
	// SURGICAL REVERT (flips this RED): in BootAndWait change the trailing
	//     return t.Resolve(bctx, udid)
	// back to
	//     return t.Resolve(ctx, udid)
	tool := &Tool{Path: "/usr/bin/xcrun", exec: (&blockingExec{block: map[string]bool{"list": true}}).run}
	assertReturnsWithin(t, 2*time.Second, func() {
		if _, err := tool.BootAndWait(context.Background(), "UDID-1", 80*time.Millisecond); err == nil {
			t.Errorf("expected BootAndWait to surface the bounded-resolve error, got nil")
		}
	})
}

// -----------------------------------------------------------------------------
// SIM-3 — Resolve(name) is deterministic across randomised map-iteration order.
// -----------------------------------------------------------------------------

func TestResolve_byNameDeterministic_SIM3(t *testing.T) {
	// SIM-3 GUARD. Two devices share Name "iPad Air" under two runtimes.
	// parseListJSON iterates a Go map (randomised order), so a naive first-Name
	// match returned a different UDID across runs. The fix selects the
	// lexicographically-smallest UDID, so Resolve(name) is stable.
	//
	// SURGICAL REVERT (flips this RED): in Resolve change
	//     if !found || d.UDID < best.UDID {
	// to
	//     if !found || false {
	// → best stays the first map-iteration Name match (nondeterministic); across
	//   the iterations below Resolve returns > 1 distinct UDID and this fails.
	const dupNameJSON = `{
  "devices" : {
    "com.apple.CoreSimulator.SimRuntime.iOS-17-2" : [
      { "udid":"99999999-9999-9999-9999-999999999999","isAvailable":true,"state":"Shutdown","name":"iPad Air" }
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-18-5" : [
      { "udid":"11111111-1111-1111-1111-111111111111","isAvailable":true,"state":"Shutdown","name":"iPad Air" }
    ]
  }
}`
	const wantUDID = "11111111-1111-1111-1111-111111111111" // lexicographically smallest

	tool := newToolWithExec(&fakeExec{out: dupNameJSON})
	seen := map[string]int{}
	const iters = 200
	for i := 0; i < iters; i++ {
		d, err := tool.Resolve(context.Background(), "iPad Air")
		if err != nil {
			t.Fatalf("iteration %d: Resolve error: %v", i, err)
		}
		seen[d.UDID]++
	}
	if len(seen) != 1 {
		t.Fatalf("Resolve(name) nondeterministic across %d runs: returned UDIDs %v (SIM-3 regression)", iters, seen)
	}
	if _, ok := seen[wantUDID]; !ok {
		t.Fatalf("Resolve(name) = %v, want deterministic smallest UDID %q", seen, wantUDID)
	}
}

// -----------------------------------------------------------------------------
// SIM-2 — RecordVideo is detached from the per-call ctx (SIGKILL would truncate
// the mp4; only Stop()'s SIGINT finalises it). Behavioural guards use a benign
// POSIX helper process standing in for `simctl recordVideo` — NO real simctl.
// -----------------------------------------------------------------------------

// shq single-quotes s for safe embedding in a POSIX shell script body.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeShellScript writes an executable /bin/sh script; skips where unavailable.
func writeShellScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SIM-2 real-process guard uses a POSIX shell; skipped on windows")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("SIM-2 real-process guard needs /bin/sh: %v", err)
	}
	p := filepath.Join(t.TempDir(), "fakesimctl.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return p
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q did not appear within %s", path, timeout)
}

func TestRecordVideo_detachedFromPerCallCtx_SIM2(t *testing.T) {
	// SIM-2 GUARD (the regression guard). The recorder must NOT be killed when
	// the caller's per-call ctx is cancelled — simctl finalises the mp4 only on
	// SIGINT, so a per-call-ctx SIGKILL truncates it. A benign long-lived helper
	// stands in for `simctl recordVideo`.
	//
	// SURGICAL REVERT (flips this RED): in RecordVideo change
	//     recCtx, recCancel := context.WithCancel(context.Background())
	// to
	//     recCtx, recCancel := context.WithCancel(ctx)
	// → the recorder is bound to the per-call ctx again; the cancel() below
	//   SIGKILLs it, `done` fires promptly, and this guard fails.
	script := writeShellScript(t, "exec sleep 30")
	tool := &Tool{Path: script, exec: (&fakeExec{}).run}

	perCall, cancel := context.WithCancel(context.Background())
	rec, err := tool.RecordVideo(perCall, "UDID-1", filepath.Join(t.TempDir(), "out.mp4"), "")
	if err != nil {
		cancel()
		t.Fatalf("RecordVideo start error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- rec.cmd.Wait() }()

	cancel() // cancel the PER-CALL ctx — must NOT affect the detached recorder

	select {
	case <-done:
		t.Errorf("recorder process was terminated by per-call ctx cancellation " +
			"(SIM-2 regression): the recorder must be detached and terminated only via Stop()")
		if rec.cancel != nil {
			rec.cancel()
		}
	case <-time.After(600 * time.Millisecond):
		// Still alive after per-call cancel → correctly detached. Clean up the
		// benign helper and reap it (§11.4.14), then release the recorder's ctx.
		_ = rec.cmd.Process.Kill()
		<-done
		if rec.cancel != nil {
			rec.cancel()
		}
	}
}

func TestRecordingStop_deliversSIGINT_SIM2(t *testing.T) {
	// Supporting evidence for SIM-2 (behaviour UNCHANGED by the fix — no surgical
	// revert): Recording.Stop() finalises the recorder via SIGINT (the signal
	// simctl needs to flush the mp4), NOT a hard kill. A benign helper traps INT,
	// writes a marker, and exits 0; after Stop() the marker proves a HANDLED
	// SIGINT was delivered — the exact mechanism SIM-2's detach protects.
	dir := t.TempDir()
	marker := filepath.Join(dir, "finalized")
	ready := filepath.Join(dir, "ready")
	body := "trap 'printf finalized > " + shq(marker) + "; exit 0' INT\n" +
		"printf ready > " + shq(ready) + "\n" +
		"while true; do sleep 0.02; done"
	tool := &Tool{Path: writeShellScript(t, body), exec: (&fakeExec{}).run}

	rec, err := tool.RecordVideo(context.Background(), "UDID-1", filepath.Join(dir, "out.mp4"), "")
	if err != nil {
		t.Fatalf("RecordVideo start error: %v", err)
	}
	waitForFile(t, ready, 3*time.Second) // ensure the INT trap is installed first

	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not written — Stop() did not deliver a handled SIGINT: %v", err)
	}
	if string(got) != "finalized" {
		t.Errorf("marker = %q, want %q", string(got), "finalized")
	}
}

// -----------------------------------------------------------------------------
// SIM-4 — Detect/verifySimctl wraps the underlying cause instead of discarding.
// -----------------------------------------------------------------------------

func TestVerifySimctl_wrapsUnderlyingCause_SIM4(t *testing.T) {
	// SIM-4 GUARD. When `simctl help` fails, verifySimctl wraps ErrNotInstalled
	// with the underlying cause (errors.Is stays true AND the real error text is
	// preserved) instead of discarding it.
	//
	// SURGICAL REVERT (flips this RED): in verifySimctl change
	//     return fmt.Errorf("%w: simctl help failed: %v (%s)", ErrNotInstalled, err, strings.TrimSpace(out))
	// to
	//     return ErrNotInstalled
	// → the underlying cause is discarded and the strings.Contains check fails.
	f := &fakeExec{out: `xcrun: error: unable to find utility "simctl"`, err: errors.New("exit status 72")}
	err := verifySimctl(context.Background(), newToolWithExec(f))
	if err == nil {
		t.Fatalf("expected verifySimctl to fail when simctl help errors, got nil")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("errors.Is(err, ErrNotInstalled) = false, want true (sentinel must stay wrappable)")
	}
	if !strings.Contains(err.Error(), "exit status 72") {
		t.Errorf("underlying cause discarded: err = %q, want it to include \"exit status 72\" (SIM-4 regression)", err.Error())
	}
}

// -----------------------------------------------------------------------------
// SIM-5 — the already-booted / already-shutdown benign match stays the PRECISE
// phrase (collapsed from a redundant two-clause OR; must not over-broaden).
// -----------------------------------------------------------------------------

func TestBoot_benignMatchIsPrecise_SIM5(t *testing.T) {
	// SIM-5 GUARD (Boot). An error merely containing the word "Booted" (but not
	// the phrase "state: Booted") must PROPAGATE, not be swallowed as benign.
	//
	// SURGICAL REVERT (flips this RED): in Boot change
	//     if strings.Contains(out, "state: Booted") {
	// to
	//     if strings.Contains(out, "Booted") {
	f := &fakeExec{out: "An error was encountered: the Booted-Test device is invalid", err: errors.New("exit 164")}
	if err := newToolWithExec(f).Boot(context.Background(), "bogus"); err == nil {
		t.Errorf("Boot swallowed a non-benign error containing the word \"Booted\" (SIM-5 over-broad regression)")
	}
	// A genuine already-booted message stays benign under the precise match.
	g := &fakeExec{out: "Unable to boot device in current state: Booted", err: errors.New("exit 164")}
	if err := newToolWithExec(g).Boot(context.Background(), "UDID-1"); err != nil {
		t.Errorf("Boot on already-booted device returned %v, want nil (benign)", err)
	}
}

func TestShutdown_benignMatchIsPrecise_SIM5(t *testing.T) {
	// SIM-5 GUARD (Shutdown). Mirror of Boot: an error merely containing
	// "Shutdown" (not the phrase "state: Shutdown") must PROPAGATE.
	//
	// SURGICAL REVERT (flips this RED): in Shutdown change
	//     if strings.Contains(out, "state: Shutdown") {
	// to
	//     if strings.Contains(out, "Shutdown") {
	f := &fakeExec{out: "An error was encountered: the Shutdown-Test device is invalid", err: errors.New("exit 164")}
	if err := newToolWithExec(f).Shutdown(context.Background(), "bogus"); err == nil {
		t.Errorf("Shutdown swallowed a non-benign error containing \"Shutdown\" (SIM-5 over-broad regression)")
	}
	g := &fakeExec{out: "Unable to shutdown device in current state: Shutdown", err: errors.New("exit 164")}
	if err := newToolWithExec(g).Shutdown(context.Background(), "UDID-1"); err != nil {
		t.Errorf("Shutdown on already-shutdown device returned %v, want nil (benign)", err)
	}
}
