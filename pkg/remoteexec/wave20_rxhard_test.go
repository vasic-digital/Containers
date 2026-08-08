package remoteexec

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Wave-20 CT-HARDEN-RX-HARD guards (GREEN-polarity: they assert the FIXED
// behaviour, so `go test` is green on the committed tree). Each guard's defect
// is proven real by a SURGICAL REVERT of the fix (documented in the batch
// evidence): reverting the source edit flips the discriminating guard RED.
//
// All guards drive the package's Runner seam with recorded fake output — no
// real loginctl / systemd / network — per §11.4.27 (no real infra in unit
// tests).
// ---------------------------------------------------------------------------

// cmdFakeRunner is a command-AWARE Runner (the single-canned fakeRunner in the
// Wave-17 audit file returns one Result for every command, which cannot drive
// the multi-command Launch path). It returns lingerOut for the LingerActive
// `loginctl show-user` query and exit-0/empty for every other command
// (mkdir / systemd-run / reset-failed / ...), so a Launch runs end-to-end
// against a chosen linger verdict.
type cmdFakeRunner struct {
	lingerOut string // stdout returned for the `show-user ... -p Linger` query
}

func (f cmdFakeRunner) Run(_ context.Context, cmd string) (Result, error) {
	if strings.Contains(cmd, "show-user") { // the LingerActive verification query
		return Result{Stdout: f.lingerOut}, nil
	}
	return Result{}, nil
}
func (f cmdFakeRunner) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }

var _ Runner = cmdFakeRunner{}

// ===========================================================================
// RX-1 — linger-durability MUST be verified, never claimed unconfirmed.
//
// HONEST BOUNDARY (§11.4.107 / §11.4.123): these guards prove the linger-
// verification COMMAND is invoked through the Runner seam and its result is
// honestly surfaced (LingerActive verdict + Handle.LingerVerified). They do NOT
// — and no clean software oracle can — prove that the target OS actually
// persisted the transient .service unit across a REAL logout; that requires an
// operator-attended live logout on a systemd host. The guard closes the bluff
// "reported durable without confirming linger took", not "proved survival".
// ===========================================================================

// LingerActive: a real "yes" verdict is durable-confirmed.
func TestWave20_RX1_LingerActive_YesVerdict(t *testing.T) {
	active, err := LingerActive(context.Background(), fakeRunner{res: Result{Stdout: "yes\n"}})
	if err != nil || !active {
		t.Fatalf("a 'yes' Linger verdict must be (true, nil), got active=%v err=%v", active, err)
	}
}

// LingerActive: a real "no" verdict is honestly NOT durable (the enable-linger
// best-effort call may have run but linger did not take).
func TestWave20_RX1_LingerActive_NoVerdict(t *testing.T) {
	active, err := LingerActive(context.Background(), fakeRunner{res: Result{Stdout: "no\n"}})
	if err != nil {
		t.Fatalf("a 'no' Linger verdict is a genuine answer, must not error, got: %v", err)
	}
	if active {
		t.Errorf("'no' must report linger inactive (durable=false), not claim durability")
	}
}

// LingerActive: a transport failure (err + empty stdout — no verdict produced)
// must surface, never be reported as a silent "linger off" for an unreachable
// host (§11.4.144).
func TestWave20_RX1_LingerActive_TransportFailureSurfacesError(t *testing.T) {
	r := fakeRunner{
		res: Result{Stdout: ""},
		err: fmt.Errorf("ssh: connect to host nezha port 22: Connection refused"),
	}
	active, err := LingerActive(context.Background(), r)
	if err == nil {
		t.Fatalf("transport failure (err + empty stdout) must yield a non-nil error, got active=%v err=nil", active)
	}
	if active {
		t.Errorf("an unreachable host must not be reported as linger-active")
	}
}

// Launch: when linger is requested AND verification confirms it ("yes"), the
// returned Handle honestly records LingerVerified=true.
func TestWave20_RX1_Launch_LingerVerifiedTrueWhenActive(t *testing.T) {
	r := cmdFakeRunner{lingerOut: "yes\n"}
	h, err := Launch(context.Background(), r, Spec{Unit: "qa-matrix", Dir: "/abs/cache/remoteexec"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !h.LingerVerified {
		t.Errorf("linger requested and confirmed 'yes' — Handle.LingerVerified must be true")
	}
}

// Launch: DISCRIMINATING guard — a launch that "succeeded" (systemd-run exit 0)
// while linger did NOT take ("no") must NOT claim durability. Reverting the
// verification block in Launch leaves LingerVerified always-false, so the
// TrueWhenActive guard above flips RED; this guard proves the honest false.
func TestWave20_RX1_Launch_DurabilityNotClaimedWhenLingerNotTaken(t *testing.T) {
	r := cmdFakeRunner{lingerOut: "no\n"}
	h, err := Launch(context.Background(), r, Spec{Unit: "qa-matrix", Dir: "/abs/cache/remoteexec"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.LingerVerified {
		t.Errorf("linger did not take ('no') — Launch must NOT claim durability (LingerVerified must be false)")
	}
}

// Launch: when linger is explicitly disabled, LingerVerified stays false and no
// durability is claimed (the verification is skipped by construction).
func TestWave20_RX1_Launch_LingerDisabledNotVerified(t *testing.T) {
	r := cmdFakeRunner{lingerOut: "yes\n"} // even if the host WOULD say yes,
	h, err := Launch(context.Background(), r, Spec{Unit: "u", Dir: "/abs/d"}.WithLinger(false))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.LingerVerified {
		t.Errorf("linger disabled — LingerVerified must be false (durability not requested)")
	}
}

// ===========================================================================
// RX-2 — a relative Dir is ambiguous against the remote CWD; reject it.
// ===========================================================================

// resolveDir: a relative Dir must be rejected with a clear error.
func TestWave20_RX2_ResolveDir_RelativeRejected(t *testing.T) {
	_, err := resolveDir(context.Background(), fakeRunner{}, Spec{Dir: "relative/artifacts"})
	if err == nil {
		t.Fatalf("a relative spec.Dir must be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error must explain the Dir must be absolute, got: %v", err)
	}
}

// resolveDir: an absolute Dir passes through unchanged (negative control — the
// fix must not over-fire on the valid absolute case).
func TestWave20_RX2_ResolveDir_AbsoluteAccepted(t *testing.T) {
	got, err := resolveDir(context.Background(), fakeRunner{}, Spec{Dir: "/abs/cache/remoteexec"})
	if err != nil {
		t.Fatalf("an absolute spec.Dir must be accepted, got: %v", err)
	}
	if got != "/abs/cache/remoteexec" {
		t.Errorf("resolveDir(absolute) = %q, want unchanged /abs/cache/remoteexec", got)
	}
}

// Launch: the rejection matters at the public API — a relative Dir fails Launch
// before any command runs (proves the defect is user-reachable, not internal).
func TestWave20_RX2_Launch_RelativeDirRejected(t *testing.T) {
	r := cmdFakeRunner{lingerOut: "yes\n"}
	if _, err := Launch(context.Background(), r, Spec{Unit: "qa-matrix", Dir: "rel/dir"}); err == nil {
		t.Fatalf("Launch with a relative spec.Dir must fail, got nil error")
	}
}

// ===========================================================================
// RX-3 — FetchLog must not mask a truncated read as a complete log.
// ===========================================================================

// FetchLog: a transport error WITH a partial prefix already read means the log
// is TRUNCATED — the error MUST surface (and the partial prefix is still
// returned so the caller has both). The prior guard (`err && stdout==""`)
// swallowed this, returning the prefix with a nil error.
func TestWave20_RX3_FetchLog_TruncationSurfacesError(t *testing.T) {
	partial := "line1\nline2\n<TRUNCATED"
	r := fakeRunner{
		res: Result{Stdout: partial},
		err: fmt.Errorf("ssh: connection reset by peer mid-transfer"),
	}
	got, err := FetchLog(context.Background(), r, handleForDir("qa-matrix", "/abs/cache/remoteexec"))
	if err == nil {
		t.Fatalf("a transport error with a partial log must surface (truncation), got nil error")
	}
	if got != partial {
		t.Errorf("partial prefix must still be returned alongside the error, got %q want %q", got, partial)
	}
}

// FetchLog: a clean read (no error) returns the complete log with a nil error.
func TestWave20_RX3_FetchLog_CompleteNoError(t *testing.T) {
	r := fakeRunner{res: Result{Stdout: "START_MARKER\nDONE_MARKER\n"}}
	got, err := FetchLog(context.Background(), r, handleForDir("qa-matrix", "/abs/cache/remoteexec"))
	if err != nil {
		t.Fatalf("a clean read must not error, got: %v", err)
	}
	if got != "START_MARKER\nDONE_MARKER\n" {
		t.Errorf("complete log must be returned intact, got %q", got)
	}
}

// FetchLog: a missing / not-yet-created log (cat exits non-zero → both runners
// report a NIL error with empty stdout) is NOT truncation — it returns
// ("", nil) unchanged, so routine "log not yet written" polling is not broken.
func TestWave20_RX3_FetchLog_MissingLogNoError(t *testing.T) {
	r := fakeRunner{res: Result{Stdout: ""}} // nil error: benign missing-file cat
	got, err := FetchLog(context.Background(), r, handleForDir("qa-matrix", "/abs/cache/remoteexec"))
	if err != nil {
		t.Fatalf("a benign missing-log read (nil error) must not surface an error, got: %v", err)
	}
	if got != "" {
		t.Errorf("missing log must return empty, got %q", got)
	}
}
