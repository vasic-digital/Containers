package applesim

// Wave-20 DEEPER (§11.4.118 2nd-pass) SIM2 guards — NEW defects found beyond the
// first-pass SIM-1..SIM-5 (wave20_simhard_test.go). Each guard is GREEN against
// the fixed applesim.go and was proven RED by the EXACT single-line surgical
// revert documented above it (§11.4.115 committed-guard discipline). All guards
// drive the injectable exec seam / the guard-before-spawn path — NO real
// xcrun/simctl, so they are hermetic on this Linux host (§11.4.27, §11.4.3).
//
// fakeExec + newToolWithExec are defined in applesim_test.go (same package).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// SIM2-1 — SECURITY (argv flag-injection §11.4.174-adjacent). Every simctl
// lifecycle method built a bare argv (exec.CommandContext, no shell) placing the
// caller-supplied UDID / bundle id / .app path / output path as a POSITIONAL
// with NO end-of-options guard and NO leading-'-' rejection, so a value like
// "--help" / "--console" / "--type=jpeg" is parsed by simctl's OWN option parser
// as a FLAG, not the intended value — argv flag-injection. simctl's positional
// grammar is not documented to honour a "--" terminator, so the fix REFUSES a
// leading-'-' value BEFORE spawning (reject-before-exec, mirroring the sibling
// pkg/genymotion GENY2-1 / pkg/network NET3 / pkg/egress EG2-1 discipline)
// rather than GUESS that "--" would neutralise it (§11.4.6).
//
// SURGICAL REVERT (flips this RED — single line, count==1): in checkArgs change
//     if strings.HasPrefix(a, "-") {
// to
//     if strings.HasPrefix(a, "\x00") {
// → the guard never matches a leading dash, so EVERY method below spawns simctl
//   with the hostile value (or, for RecordVideo, fails to start the nonexistent
//   binary) instead of returning ErrUnsafeArg — the reject assertions FAIL.
// -----------------------------------------------------------------------------

func TestWave20_SIM2_RejectsLeadingDashArgsBeforeExec(t *testing.T) {
	t.Parallel()

	// mustReject: the method returned ErrUnsafeArg AND spawned NOTHING.
	mustReject := func(t *testing.T, name string, f *fakeExec, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, ErrUnsafeArg) {
			t.Fatalf("SIM2-1: %s: err = %v, want errors.Is ErrUnsafeArg (leading-dash arg not refused → argv flag-injection)", name, err)
		}
		if len(f.gotArgs) != 0 {
			t.Fatalf("SIM2-1: %s: spawned simctl %v — the guard MUST fire BEFORE exec", name, f.gotArgs)
		}
	}

	ctx := context.Background()

	// Hostile values chosen to look like REAL simctl options so the vector is
	// concrete: "--help"/"--set" (global), "--console" (launch), "--type=jpeg"
	// (io screenshot). Any leading '-' triggers the guard.
	const evilUDID = "--set" // simctl would treat this as the --set device-set flag

	// UDID position — every lifecycle method.
	f := &fakeExec{}
	mustReject(t, "Boot(evil udid)", f, newTool2(f).Boot(ctx, evilUDID))
	f = &fakeExec{}
	mustReject(t, "Shutdown(evil udid)", f, newTool2(f).Shutdown(ctx, evilUDID))
	f = &fakeExec{}
	mustReject(t, "Install(evil udid)", f, newTool2(f).Install(ctx, evilUDID, "/tmp/App.app"))
	f = &fakeExec{}
	_, e := newTool2(f).Launch(ctx, evilUDID, "com.example.app")
	mustReject(t, "Launch(evil udid)", f, e)
	f = &fakeExec{}
	mustReject(t, "Terminate(evil udid)", f, newTool2(f).Terminate(ctx, evilUDID, "com.example.app"))
	f = &fakeExec{}
	mustReject(t, "Screenshot(evil udid)", f, newTool2(f).Screenshot(ctx, evilUDID, "/tmp/s.png"))

	// Secondary-positional injection — app path / bundle id / output path.
	f = &fakeExec{}
	mustReject(t, "Install(evil appPath)", f, newTool2(f).Install(ctx, "UDID-1", "--foo=bar"))
	f = &fakeExec{}
	_, e = newTool2(f).Launch(ctx, "UDID-1", "--console")
	mustReject(t, "Launch(evil bundleID)", f, e)
	f = &fakeExec{}
	mustReject(t, "Terminate(evil bundleID)", f, newTool2(f).Terminate(ctx, "UDID-1", "--console"))
	f = &fakeExec{}
	mustReject(t, "Screenshot(evil outPath)", f, newTool2(f).Screenshot(ctx, "UDID-1", "--type=jpeg"))

	// RecordVideo does NOT route through Tool.exec (it owns the live process), so
	// its guard fires before exec.CommandContext(...).Start(). With the guard the
	// call returns (nil, ErrUnsafeArg); with the guard reverted it would attempt
	// to Start a nonexistent binary → a DIFFERENT ("failed to start") error, so
	// errors.Is(ErrUnsafeArg) is the discriminator, and no process is spawned.
	tool := &Tool{Path: "/nonexistent/xcrun", exec: (&fakeExec{}).run}
	if rec, err := tool.RecordVideo(ctx, evilUDID, "/tmp/o.mp4", ""); err == nil || !errors.Is(err, ErrUnsafeArg) || rec != nil {
		t.Fatalf("SIM2-1: RecordVideo(evil udid): rec=%v err=%v, want (nil, ErrUnsafeArg)", rec, err)
	}
	if rec, err := tool.RecordVideo(ctx, "UDID-1", "--codec=hevc", ""); err == nil || !errors.Is(err, ErrUnsafeArg) || rec != nil {
		t.Fatalf("SIM2-1: RecordVideo(evil outPath): rec=%v err=%v, want (nil, ErrUnsafeArg)", rec, err)
	}

	// Negative control (anti-tautology — proves the guard is NOT a blanket
	// "always refuse"): legitimate values pass the guard AND reach exec. A
	// mutation that rejects everything FAILs here.
	type nc struct {
		name string
		call func(f *fakeExec) error
	}
	for _, c := range []nc{
		{"Boot", func(f *fakeExec) error { return newTool2(f).Boot(ctx, "UDID-1") }},
		{"Shutdown", func(f *fakeExec) error { return newTool2(f).Shutdown(ctx, "UDID-1") }},
		{"Install", func(f *fakeExec) error { return newTool2(f).Install(ctx, "UDID-1", "/tmp/App.app") }},
		{"Launch", func(f *fakeExec) error { _, err := newTool2(f).Launch(ctx, "UDID-1", "com.example.app"); return err }},
		{"Terminate", func(f *fakeExec) error { return newTool2(f).Terminate(ctx, "UDID-1", "com.example.app") }},
		{"Screenshot", func(f *fakeExec) error { return newTool2(f).Screenshot(ctx, "UDID-1", "/tmp/s.png") }},
	} {
		f := &fakeExec{}
		if err := c.call(f); err != nil {
			t.Fatalf("SIM2-1 negative control: %s(normal) over-rejected a legitimate value: %v", c.name, err)
		}
		if len(f.gotArgs) != 1 {
			t.Fatalf("SIM2-1 negative control: %s(normal) did not reach exec (spawned %d times): %v", c.name, len(f.gotArgs), f.gotArgs)
		}
	}
}

// newTool2 mirrors newToolWithExec but is local to this file to keep the SIM2
// guards self-contained.
func newTool2(f *fakeExec) *Tool { return &Tool{Path: "/usr/bin/xcrun", exec: f.run} }

// -----------------------------------------------------------------------------
// SIM2-2 — §11.4.108 reported-ready-while-not. BootAndWait's contract is to
// return the device "once it is fully booted", but after `bootstatus` exited 0
// it returned WHATEVER state the trailing Resolve read, paired with a nil error
// — so a device still "Booting" (or one that shut down again in the race between
// bootstatus exiting and the list snapshot) was handed back to the caller as
// though it were Booted. The fix confirms the resolved device IsBooted() and
// surfaces the discrepancy instead of bluffing a booted result.
//
// SURGICAL REVERT (flips this RED — single line, count==1): in BootAndWait
// change
//     if !dev.IsBooted() {
// to
//     if false {
// → the not-booted device is returned with a nil error (the reported-ready-
//   while-not bluff) and the assertion below FAILs.
// -----------------------------------------------------------------------------

func TestWave20_SIM2_BootAndWaitConfirmsBootedState(t *testing.T) {
	t.Parallel()

	const udid = "11111111-2222-3333-4444-555555555555"
	list := func(state string) string {
		return `{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-18-5":[` +
			`{"udid":"` + udid + `","isAvailable":true,"state":"` + state + `","name":"iPhone 15"}]}}`
	}

	// Not-booted: boot + bootstatus succeed, but the resolved device is still
	// "Booting". BootAndWait MUST return an error, NOT a nil-error not-booted
	// device. (fakeExec returns the same output for boot/bootstatus/list; a
	// non-error output means Boot/bootstatus succeed and Resolve reads the state.)
	f := &fakeExec{out: list("Booting")}
	dev, err := newTool2(f).BootAndWait(context.Background(), udid, 2*time.Second)
	if err == nil {
		t.Fatalf("SIM2-2: BootAndWait returned nil error with resolved state %q — reported-ready-while-not (§11.4.108); dev=%+v", dev.State, dev)
	}

	// Negative control (anti-tautology): when the resolved device IS Booted,
	// BootAndWait returns it with a nil error. A mutation that always errors
	// (or always returns the device regardless of state) FAILs one of these.
	g := &fakeExec{out: list("Booted")}
	dev, err = newTool2(g).BootAndWait(context.Background(), udid, 2*time.Second)
	if err != nil {
		t.Fatalf("SIM2-2 negative control: BootAndWait errored on a genuinely Booted device: %v", err)
	}
	if !dev.IsBooted() || dev.UDID != udid {
		t.Fatalf("SIM2-2 negative control: BootAndWait returned %+v, want the Booted target %q", dev, udid)
	}
}
