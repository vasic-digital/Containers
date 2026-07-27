package remoteexec

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeRunner is a deterministic Runner returning a canned Result + error, for
// the Wave-17 audit guards. It never touches the network or systemd, so these
// tests run everywhere and cannot flake.
type fakeRunner struct {
	res Result
	err error
}

func (f fakeRunner) Run(context.Context, string) (Result, error)                  { return f.res, f.err }
func (f fakeRunner) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }

var _ Runner = fakeRunner{}

// Wave-17 finding 1 (CRITICAL): a transport failure — the runner returns an
// error AND empty stdout, i.e. no verdict was ever produced — MUST surface as a
// non-nil error, never a silent false "not active". Reporting a host we could
// not even reach as "inactive" is the exact bluff §11.4.1 / §11.4.108 forbid:
// a caller polling IsActive would conclude the durable job finished when in
// truth the host was unreachable.
func TestWave17_Remoteexec_IsActive_TransportFailureSurfacesError(t *testing.T) {
	r := fakeRunner{
		res: Result{Stdout: ""},
		err: fmt.Errorf("ssh: connect to host nezha port 22: Connection refused"),
	}
	active, err := IsActive(context.Background(), r, "qa-matrix")
	if err == nil {
		t.Fatalf("transport failure (err + empty stdout) must yield a non-nil error, got active=%v err=nil", active)
	}
	if active {
		t.Errorf("a host we could not reach must not be reported active")
	}
}

// Negative control for finding 1: a benign non-active verdict — the runner
// produced real stdout ("inactive"), even though pkg/remote also surfaced the
// non-zero is-active exit as an error — is NOT a transport failure and MUST
// return a nil error. Without this control, an over-broad fix that errored on
// any non-nil err would false-error every inactive unit.
func TestWave17_Remoteexec_IsActive_BenignInactiveVerdictNoError(t *testing.T) {
	r := fakeRunner{res: Result{Stdout: "inactive\n"}, err: fmt.Errorf("exit status 3")}
	active, err := IsActive(context.Background(), r, "qa-matrix")
	if err != nil {
		t.Fatalf("a real 'inactive' verdict must not be reported as a transport error, got: %v", err)
	}
	if active {
		t.Errorf("'inactive' must report active=false")
	}
}

// Negative control: a genuine "active" verdict returns (true, nil).
func TestWave17_Remoteexec_IsActive_ActiveVerdict(t *testing.T) {
	r := fakeRunner{res: Result{Stdout: "active\n"}, err: nil}
	active, err := IsActive(context.Background(), r, "qa-matrix")
	if err != nil || !active {
		t.Fatalf("an 'active' verdict must be (true, nil), got active=%v err=%v", active, err)
	}
}

// Wave-17 finding 2 (HIGH): a re-Launch of the same unit without an intervening
// Stop() must not let WaitForSentinel/FetchLog observe the PRIOR run's exit code
// or output. buildLaunchCommand must `rm -f` the stale sentinel AND log BEFORE
// systemd-run, within the one synchronous shell invocation Launch runs (so the
// removal completes before Launch() returns, closing the race window the async
// wrapper's own truncation cannot).
func TestWave17_Remoteexec_BuildLaunchCommand_ClearsStaleArtifactsBeforeLaunch(t *testing.T) {
	h := handleForDir("qa-matrix", "/abs/cache/remoteexec")
	cmd := buildLaunchCommand(h, "/abs/cache/remoteexec", true)

	rmIdx := strings.Index(cmd, "rm -f")
	runIdx := strings.Index(cmd, "systemd-run")
	if rmIdx == -1 {
		t.Fatalf("launch command never removes the stale sentinel/log\ngot: %s", cmd)
	}
	if runIdx == -1 || rmIdx > runIdx {
		t.Fatalf("stale-artifact rm -f must run BEFORE systemd-run\ngot: %s", cmd)
	}
	seg := cmd[rmIdx:runIdx]
	if !strings.Contains(seg, h.SentinelPath) {
		t.Errorf("rm -f segment must remove the stale sentinel %q\nseg: %s", h.SentinelPath, seg)
	}
	if !strings.Contains(seg, h.LogPath) {
		t.Errorf("rm -f segment must remove the stale log %q\nseg: %s", h.LogPath, seg)
	}

	// The removal must be GUARDED by an is-active check so re-Launching a
	// still-running unit never unlinks that live job's in-flight log/sentinel:
	// `is-active --quiet <unit> || rm -f ...` runs the rm only when not active.
	guardIdx := strings.Index(cmd, "is-active")
	if guardIdx == -1 || guardIdx > rmIdx {
		t.Errorf("stale-artifact rm -f must be guarded by an is-active check before it\ngot: %s", cmd)
	} else if !strings.Contains(cmd[guardIdx:rmIdx], "||") {
		t.Errorf("expected `is-active ... || rm -f` so rm runs only when the unit is not active\ngot: %s", cmd)
	}
}

// Wave-17 finding 3 (MEDIUM): LocalRunner.Run must honor the Runner contract —
// a non-zero exit is reported via Result.ExitCode and is NOT a Go error (only a
// genuine cannot-execute-at-all failure returns an error). This mirrors
// SSHRunner.Run so local and remote hosts behave identically.
func TestWave17_Remoteexec_LocalRunner_NonZeroExitIsNotGoError(t *testing.T) {
	r := NewLocalRunner(nil)
	res, err := r.Run(context.Background(), "exit 3")
	if err != nil {
		t.Fatalf("a non-zero shell exit must not be a Go error per the Runner contract, got: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// Negative control for finding 3: a genuine "could not execute at all" failure
// (an already-canceled context launches no process) MUST still return an error
// — the contract reserves a non-nil error for exactly this case, and the fix
// (which touches only the *exec.ExitError branch) must not swallow it. This is
// what keeps IsActive's transport-failure guard meaningful on LocalRunner.
func TestWave17_Remoteexec_LocalRunner_CannotExecuteStillErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := NewLocalRunner(nil)
	if _, err := r.Run(ctx, "echo hi"); err == nil {
		t.Fatal("a canceled context (command never executes) must still return an error")
	}
}
