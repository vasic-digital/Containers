package remoteexec

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Wave-20 DEEPER (RX2-hardening) guards — §11.4.118 loop-until-dry 2nd pass over
// the durable remote/local execution surface. GREEN-polarity: they assert the
// FIXED behaviour, so `go test` is green on the committed tree. Each guard's
// defect is proven real by a SURGICAL single-line REVERT of its fix (documented
// in the batch evidence) that flips the discriminating guard RED.
//
// All guards drive the Runner seam with recorded fake output — no real
// loginctl / systemd / network — per §11.4.27 (no real infra in unit tests).
// fakeRunner is defined in wave17_remoteexec_audit_test.go (same package).
// ---------------------------------------------------------------------------

// ===========================================================================
// RX2-1 — resolveDir's DEFAULT (empty Spec.Dir / XDG-resolved) branch must
// enforce the ABSOLUTE-path contract, exactly as the explicit Spec.Dir branch
// (RX-2) does. A relative resolved dir makes the durable job write its
// sentinel/log relative to the systemd user-manager CWD while
// mkdir/WaitForSentinel/FetchLog resolve the SAME relative paths against the
// login-shell CWD — a different directory — so the completed job's sentinel is
// never seen and WaitForSentinel hangs to timeout on a job that finished
// (§11.4.108 durability bluff). RX-2 fixed only the explicit branch and missed
// this — the common empty-Dir production path.
// ===========================================================================

// relDefaultDirRunner returns a RELATIVE path for the resolveDir XDG-default
// expansion (as a runner whose environment sets a relative XDG_CACHE_HOME would)
// and exit-0/empty for every other command, so Launch can be driven end-to-end
// against a relative resolved default.
type relDefaultDirRunner struct{}

func (relDefaultDirRunner) Run(_ context.Context, cmd string) (Result, error) {
	if strings.Contains(cmd, "XDG_CACHE_HOME") { // the resolveDir default-dir expansion
		return Result{Stdout: "relcache/remoteexec"}, nil
	}
	return Result{}, nil
}
func (relDefaultDirRunner) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }

var _ Runner = relDefaultDirRunner{}

// resolveDir: an empty Spec.Dir whose XDG expansion yields a RELATIVE path must
// be rejected with a clear "absolute" error — never silently returned relative.
func TestWave20_RX2_ResolveDefaultDir_RelativeRejected(t *testing.T) {
	_, err := resolveDir(context.Background(),
		fakeRunner{res: Result{Stdout: "relcache/remoteexec"}}, Spec{})
	if err == nil {
		t.Fatalf("a relative resolved default dir must be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error must explain the dir must be absolute, got: %v", err)
	}
}

// resolveDir: an absolute resolved default passes through unchanged — negative
// control so the RX2-1 guard does not over-fire on the valid absolute case
// (the empty XDG_CACHE_HOME → $HOME/.cache path is absolute).
func TestWave20_RX2_ResolveDefaultDir_AbsoluteAccepted(t *testing.T) {
	got, err := resolveDir(context.Background(),
		fakeRunner{res: Result{Stdout: "/home/u/.cache/remoteexec"}}, Spec{})
	if err != nil {
		t.Fatalf("an absolute resolved default dir must be accepted, got: %v", err)
	}
	if got != "/home/u/.cache/remoteexec" {
		t.Errorf("resolveDir(default,absolute) = %q, want /home/u/.cache/remoteexec", got)
	}
}

// resolveDir: an empty resolved default (transport-suspect) is still rejected —
// second negative control ensuring the RX2-1 IsAbs guard did not displace the
// pre-existing empty-string guard.
func TestWave20_RX2_ResolveDefaultDir_EmptyStillRejected(t *testing.T) {
	_, err := resolveDir(context.Background(),
		fakeRunner{res: Result{Stdout: "   "}}, Spec{})
	if err == nil {
		t.Fatalf("an empty resolved default dir must still be rejected, got nil error")
	}
}

// Launch: the relative-default defect is reachable through the PUBLIC Launch API
// on the common empty-Dir path and must fail before any systemd-run runs
// (proves the defect is user-reachable, not merely an internal-helper corner).
func TestWave20_RX2_Launch_RelativeDefaultDirRejected(t *testing.T) {
	_, err := Launch(context.Background(), relDefaultDirRunner{}, Spec{Unit: "qa-matrix"})
	if err == nil {
		t.Fatalf("Launch with an empty Dir resolving to a relative default must fail, got nil error")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("Launch error must explain the dir must be absolute, got: %v", err)
	}
}
