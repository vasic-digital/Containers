// Package remoteexec provides DURABLE remote (or local) execution of
// long-running jobs that MUST survive the end of the launching SSH/login
// session.
//
// # The problem this solves
//
// Long remote runs (QA matrices, builds, deploys) launched over SSH die when
// the SSH session ends. The remote host's systemd-logind reaps ALL of a user's
// processes when its last session closes (the default KillUserProcesses=yes
// behaviour on many hosts). tmux, nohup, setsid and disown all live inside the
// login session's `session-<n>.scope` cgroup, so they are reaped together with
// the session — detaching from the controlling terminal is irrelevant.
//
// # The fix (proven on the nezha host)
//
//   - loginctl enable-linger      → the user manager keeps running past logout
//   - systemd-run --user --unit=X --collect bash runner.sh
//     → the job runs as its OWN transient .service unit, owned by the user
//     systemd manager (cgroup .../user@UID.service/app.slice/X.service), NOT
//     the session scope. It survives the session ending.
//   - poll `systemctl --user is-active X`; the verdict persists on disk.
//   - a completion SENTINEL file (written by the wrapper) detects "done".
//
// Never pipe the long command through `tail -N`: the pipe buffers until the
// command exits, so progress is invisible. The wrapper redirects to a log file
// that is read independently via FetchLog.
//
// The package is transport-agnostic: a Runner abstracts WHERE commands run.
// LocalRunner runs on the local machine (also used by the deterministic tests
// and on localhost gate hosts); SSHRunner runs on a remote host via the
// existing pkg/remote.RemoteExecutor.
package remoteexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// Result is the outcome of running one command via a Runner.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner abstracts the execution surface (local shell or a remote host).
// Implementations MUST run command through a POSIX shell so that the
// XDG_RUNTIME_DIR export, command substitution ($(id -u)) and `||` fallbacks
// in the launch command behave identically local vs. remote.
type Runner interface {
	// Run executes a shell command and returns its result. A non-zero exit
	// code is reported in Result.ExitCode and is NOT, by itself, returned as a
	// Go error (callers decide whether a non-zero exit is fatal); err is only
	// non-nil when the command could not be executed at all.
	Run(ctx context.Context, command string) (Result, error)

	// WriteFile materialises content at remotePath on the runner's target with
	// the given Unix mode.
	WriteFile(ctx context.Context, remotePath string, content []byte, mode os.FileMode) error
}

// Spec describes a durable job to launch.
type Spec struct {
	// Unit is the systemd --user transient unit name (no ".service" suffix;
	// it is added if missing). Used as the on-disk artifact prefix too.
	Unit string

	// Script is the bash body to run durably. It is wrapped so that its
	// combined output is captured to the log file and a sentinel is written on
	// completion.
	Script string

	// Dir is the directory (on the runner's target) where the runner script,
	// log and sentinel are written. Empty defaults to ~/.cache/remoteexec.
	Dir string

	// EnableLinger controls whether `loginctl enable-linger` is run before the
	// job is launched. Defaults to true (the whole point of this package).
	EnableLinger bool

	// lingerSet tracks whether EnableLinger was explicitly provided so the
	// zero value can still default to true.
	lingerSet bool
}

// Handle identifies a launched durable job and the artifacts it produces.
type Handle struct {
	Unit         string
	ScriptPath   string
	LogPath      string
	SentinelPath string
}

// WithLinger returns a copy of the spec with EnableLinger explicitly set,
// distinguishing "explicitly false" from "unset" (which defaults to true).
func (s Spec) WithLinger(v bool) Spec {
	s.EnableLinger = v
	s.lingerSet = true
	return s
}

func (s Spec) linger() bool {
	if !s.lingerSet {
		return true // default: enable linger (zero value is "on")
	}
	return s.EnableLinger
}

// unitName normalises a unit name to carry the .service suffix exactly once.
func unitName(unit string) string {
	if strings.HasSuffix(unit, ".service") {
		return unit
	}
	return unit + ".service"
}

// resolveDir returns an ABSOLUTE artifact directory. When Spec.Dir is empty it
// asks the runner's shell to expand the XDG default, so the path is concrete on
// whichever target (local or remote) the runner addresses — this avoids
// embedding an unexpanded "$HOME" inside the single-quoted paths used by the
// launch command and the local WriteFile.
func resolveDir(ctx context.Context, r Runner, s Spec) (string, error) {
	if s.Dir != "" {
		return s.Dir, nil
	}
	res, err := r.Run(ctx, `printf %s "${XDG_CACHE_HOME:-$HOME/.cache}/remoteexec"`)
	if err != nil && strings.TrimSpace(res.Stdout) == "" {
		return "", fmt.Errorf("remoteexec: resolve default dir: %w", err)
	}
	d := strings.TrimSpace(res.Stdout)
	if d == "" {
		return "", fmt.Errorf("remoteexec: resolved default dir is empty")
	}
	return d, nil
}

// handleForDir computes the on-disk artifact paths for a unit under an
// already-absolute directory.
func handleForDir(unit, absDir string) Handle {
	base := strings.TrimSuffix(unit, ".service")
	return Handle{
		Unit:         unitName(unit),
		ScriptPath:   path.Join(absDir, base+".runner.sh"),
		LogPath:      path.Join(absDir, base+".log"),
		SentinelPath: path.Join(absDir, base+".COMPLETE"),
	}
}

// buildRunnerScript renders the wrapper script body. The wrapper runs the
// user's script with combined output redirected to the log, then ALWAYS writes
// the exit code to the sentinel — so completion is detectable on disk even
// after every session that launched it has gone away. It deliberately does NOT
// pipe through `tail` (which would buffer until exit).
func buildRunnerScript(h Handle, script string) string {
	return strings.Join([]string{
		"#!/usr/bin/env bash",
		"set -uo pipefail",
		fmt.Sprintf("__log=%s", shQuote(h.LogPath)),
		fmt.Sprintf("__sentinel=%s", shQuote(h.SentinelPath)),
		// A run-once EXIT trap writes the sentinel with the real exit status on
		// EVERY normal exit path — a fall-through, an early `exit` inside the
		// group, a `set -u` unbound-variable abort, or a user `set -e` failure
		// (the `{ }` is a group, NOT a subshell, so any of these exits THIS
		// shell without reaching a post-group write). The prior code wrote the
		// sentinel only AFTER the group, so those early-exit paths skipped it
		// entirely and WaitForSentinel then hung to timeout on a job that had in
		// fact finished — a §11.4.1 FAIL-bluff against this function's documented
		// "ALWAYS writes the exit code" contract. __finish's `__rc=$?` is its
		// first statement, so it records the terminating status on every path.
		// Honest boundary (§11.4.6): a job killed by an EXTERNAL signal
		// (SIGTERM/SIGINT) still writes the sentinel via the EXIT trap (never
		// reported as hung) but records the last-completed-command status, not
		// the 128+signo code — best-effort, as no caller consumes the recorded
		// rc today; the four normal exit paths above are exact.
		`__finish() { __rc=$?; printf '%s\n' "$__rc" > "$__sentinel"; }`,
		"trap __finish EXIT",
		"{",
		script,
		`} >"$__log" 2>&1`,
		"__rc=$?",
		"exit $__rc",
		"",
	}, "\n")
}

// buildLaunchCommand renders the EXACT durable-launch command. This is the
// load-bearing anti-bluff surface: it MUST use linger + systemd-run --user
// (so the job outlives the launching session) and MUST NOT fall back to
// nohup/tmux/setsid (all of which die with the session) nor pipe through tail.
func buildLaunchCommand(h Handle, dir string, enableLinger bool) string {
	var b strings.Builder
	b.WriteString("export XDG_RUNTIME_DIR=/run/user/$(id -u); ")
	b.WriteString(fmt.Sprintf("mkdir -p %s; ", shQuote(dir)))
	if enableLinger {
		// enable-linger is what makes the user manager (and thus the unit)
		// persist past the session ending. Best-effort: already-enabled or
		// no-permission hosts must not abort the launch.
		b.WriteString("loginctl enable-linger >/dev/null 2>&1 || true; ")
	}
	// Clear any prior failed/loaded instance so the unit name is reusable.
	b.WriteString(fmt.Sprintf("systemctl --user reset-failed %s >/dev/null 2>&1 || true; ", shQuote(h.Unit)))
	// Remove any stale sentinel + log left by a PRIOR, ALREADY-FINISHED run of
	// this same unit BEFORE launching. Without this, a re-Launch that reuses the
	// unit name without an intervening Stop() lets WaitForSentinel read the
	// previous run's exit code (and FetchLog its output) in the window before the
	// fresh wrapper writes its own — systemd-run registers the unit
	// asynchronously and the wrapper writes the sentinel only at the very end.
	// This rm runs synchronously in the same shell invocation as systemd-run, so
	// it completes before Launch() returns; relying on the async wrapper's own
	// `>` truncation would leave that race window open. The `is-active || rm`
	// guard makes the removal apply ONLY when the unit is not currently running,
	// so re-Launching a still-active unit (which systemd-run then rejects as
	// "already exists") never unlinks that live job's in-flight log.
	b.WriteString(fmt.Sprintf("systemctl --user is-active --quiet %s || rm -f %s %s; ",
		shQuote(h.Unit), shQuote(h.SentinelPath), shQuote(h.LogPath)))
	b.WriteString(fmt.Sprintf(
		"systemd-run --user --unit=%s --collect bash %s",
		shQuote(strings.TrimSuffix(h.Unit, ".service")),
		shQuote(h.ScriptPath),
	))
	return b.String()
}

// Launch writes the wrapper script then starts the durable unit. It returns as
// soon as systemd-run has registered the unit — at which point the launching
// shell is already gone but the job keeps running under the user manager.
func Launch(ctx context.Context, r Runner, spec Spec) (Handle, error) {
	if spec.Unit == "" {
		return Handle{}, fmt.Errorf("remoteexec: spec.Unit is required")
	}
	if r == nil {
		return Handle{}, fmt.Errorf("remoteexec: runner is required")
	}
	dir, err := resolveDir(ctx, r, spec)
	if err != nil {
		return Handle{}, err
	}
	h := handleForDir(spec.Unit, dir)

	// Ensure the artifact directory exists, then write the runner script.
	if _, err := r.Run(ctx, "mkdir -p "+shQuote(dir)); err != nil {
		return Handle{}, fmt.Errorf("remoteexec: mkdir %s: %w", dir, err)
	}
	script := buildRunnerScript(h, spec.Script)
	if err := r.WriteFile(ctx, h.ScriptPath, []byte(script), 0o755); err != nil {
		return Handle{}, fmt.Errorf("remoteexec: write runner script: %w", err)
	}

	cmd := buildLaunchCommand(h, dir, spec.linger())
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return Handle{}, fmt.Errorf("remoteexec: launch (%s): %w (stderr: %s)", cmd, err, res.Stderr)
	}
	if res.ExitCode != 0 {
		return Handle{}, fmt.Errorf(
			"remoteexec: systemd-run exited %d (stderr: %s)", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return h, nil
}

// IsActive reports whether the user unit is currently active. A unit that has
// finished (or never existed) reports false with a nil error — only a transport
// failure yields a non-nil error.
func IsActive(ctx context.Context, r Runner, unit string) (bool, error) {
	u := unitName(unit)
	res, err := r.Run(ctx,
		"export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user is-active "+shQuote(u))
	if err != nil && strings.TrimSpace(res.Stdout) == "" {
		// `is-active` exits non-zero for inactive/failed units; pkg/remote's SSH
		// executor surfaces that non-zero exit as an error — but so does a real
		// transport failure (host unreachable, could not exec). A non-empty
		// stdout ("active"/"inactive"/"failed") is a genuine verdict from the
		// remote systemctl and is NOT an error; an EMPTY stdout WITH an error
		// means no verdict was ever produced, so surface it rather than report a
		// false "not active" for a host we could not even reach.
		return false, fmt.Errorf("remoteexec: is-active %s: %w", u, err)
	}
	return strings.TrimSpace(res.Stdout) == "active", nil
}

// MainPID returns the MainPID of the unit (0 if not running).
func MainPID(ctx context.Context, r Runner, unit string) (int, error) {
	u := unitName(unit)
	res, err := r.Run(ctx,
		"export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user show -p MainPID --value "+shQuote(u))
	if err != nil && strings.TrimSpace(res.Stdout) == "" {
		return 0, err
	}
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &pid)
	return pid, nil
}

// WaitForSentinel polls until the job's sentinel file appears (completion) or
// the context/timeout expires. It returns the recorded exit code from the
// sentinel and nil error on completion.
func WaitForSentinel(ctx context.Context, r Runner, h Handle, poll, timeout time.Duration) (int, error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		res, err := r.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shQuote(h.SentinelPath)))
		if s := strings.TrimSpace(res.Stdout); s != "" {
			var rc int
			_, _ = fmt.Sscanf(s, "%d", &rc)
			return rc, nil
		}
		if err != nil {
			// Per the Runner contract, err is non-nil ONLY when the command could
			// not be executed at all (host unreachable / could-not-exec). A
			// not-yet-present sentinel is a benign `cat` non-zero exit reported via
			// ExitCode with a NIL error, which the empty-stdout path below already
			// treats as "not done, keep polling". A transport failure (empty stdout
			// WITH err) must NOT masquerade as "sentinel did not appear within
			// timeout": surface it immediately, the same §11.4.144 availability-
			// following honesty IsActive/MainPID/FetchLog enforce. The signature is
			// unchanged, so callers already handling the error path get a truthful
			// transport error instead of a misattributed timeout.
			return -1, fmt.Errorf("remoteexec: wait sentinel %s: %w", h.SentinelPath, err)
		}
		if timeout > 0 && time.Now().After(deadline) {
			return -1, fmt.Errorf("remoteexec: sentinel %s did not appear within %s", h.SentinelPath, timeout)
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// FetchLog returns the durable job's captured combined output.
func FetchLog(ctx context.Context, r Runner, h Handle) (string, error) {
	res, err := r.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shQuote(h.LogPath)))
	if err != nil && res.Stdout == "" {
		return "", fmt.Errorf("remoteexec: fetch log %s: %w", h.LogPath, err)
	}
	return res.Stdout, nil
}

// Stop best-effort stops and reaps the unit and removes its artifacts. Useful
// for cleanup; safe to call on an already-finished or missing unit.
func Stop(ctx context.Context, r Runner, h Handle) error {
	u := shQuote(h.Unit)
	_, err := r.Run(ctx, strings.Join([]string{
		"export XDG_RUNTIME_DIR=/run/user/$(id -u)",
		"systemctl --user stop " + u + " >/dev/null 2>&1 || true",
		"systemctl --user reset-failed " + u + " >/dev/null 2>&1 || true",
		fmt.Sprintf("rm -f %s %s %s >/dev/null 2>&1 || true",
			shQuote(h.ScriptPath), shQuote(h.LogPath), shQuote(h.SentinelPath)),
	}, "; "))
	return err
}

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

// LocalRunner executes commands on the local machine via `bash -c`. Used on
// localhost gate hosts and by the deterministic tests.
type LocalRunner struct {
	Logger logging.Logger
}

// NewLocalRunner returns a LocalRunner.
func NewLocalRunner(logger logging.Logger) *LocalRunner {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &LocalRunner{Logger: logger}
}

// Run executes command via the local bash.
func (l *LocalRunner) Run(ctx context.Context, command string) (Result, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		// Runner contract (see the Runner interface doc): a non-zero exit is
		// reported via Result.ExitCode and is NOT, by itself, a Go error —
		// callers decide whether a non-zero exit is fatal. Only a genuine
		// "could not execute at all" failure (the non-ExitError branch below)
		// returns a non-nil error. This mirrors SSHRunner.Run so local and
		// remote hosts behave identically, and keeps the "err && empty stdout"
		// transport-failure guard in IsActive/MainPID/FetchLog meaningful.
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// WriteFile writes content locally.
func (l *LocalRunner) WriteFile(_ context.Context, p string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, content, mode)
}

var _ Runner = (*LocalRunner)(nil)

// shQuote single-quotes s for safe inclusion in a POSIX shell command,
// escaping embedded single quotes. An empty string becomes ”.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
