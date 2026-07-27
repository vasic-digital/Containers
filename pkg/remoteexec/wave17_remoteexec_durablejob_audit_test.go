package remoteexec

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/remote"
)

// TestWave17_BuildRunnerScript_AlwaysWritesSentinelOnEarlyExit proves the
// durable-completion contract (buildRunnerScript's docstring: "ALWAYS writes
// the exit code to the sentinel"). The pre-fix wrapper wrote the sentinel only
// AFTER the `{ }` group, so any path that exits the shell INSIDE the group — an
// explicit `exit`, a `set -u` unbound-variable abort, a user `set -e` failure —
// skipped the write entirely, and WaitForSentinel then hung to timeout on a job
// that had actually finished (a §11.4.1 FAIL-bluff). The fix installs a run-once
// EXIT trap that records the REAL exit code on every path. The wrapper is
// executed for real via LocalRunner (exactly as systemd-run runs it), so this is
// end-to-end behaviour, not a string-shape assertion.
func TestWave17_BuildRunnerScript_AlwaysWritesSentinelOnEarlyExit(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantRC string // "" = only require the sentinel exists (rc is shell-defined)
	}{
		{"explicit_exit_nonzero", "echo work; exit 3", "3"},
		{"explicit_exit_zero", "echo ok; exit 0", "0"},
		{"fallthrough_success", "echo start; echo done", "0"},
		{"unset_var_under_set_u", `echo "${REMOTEEXEC_UNSET_XYZ}"`, ""},
		{"user_set_e_failure", "set -e; false; echo never", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := handleForDir("earlyexit", dir)
			require.NoError(t, os.WriteFile(
				h.ScriptPath, []byte(buildRunnerScript(h, tc.body)), 0o755))

			// The Runner contract reports a non-zero wrapper exit via ExitCode
			// with a nil error, so executing the wrapper is not itself an error.
			_, err := NewLocalRunner(nil).Run(
				context.Background(), "bash "+shQuote(h.ScriptPath))
			require.NoError(t, err, "wrapper execution transport/exec failure")

			data, rerr := os.ReadFile(h.SentinelPath)
			require.NoError(t, rerr,
				"durable-completion contract broken: sentinel %s was not written on this exit path",
				h.SentinelPath)
			if tc.wantRC != "" {
				assert.Equal(t, tc.wantRC, strings.TrimSpace(string(data)),
					"trap must record the REAL exit code, not a stale default")
			}
		})
	}
}

// TestWave17_WaitForSentinel_TransportFailure_SurfacesError proves WaitForSentinel
// no longer masquerades an unreachable host as a job timeout. Pre-fix it discarded
// r.Run's error (`res, _ :=`) and polled until the deadline, then reported
// "sentinel ... did not appear within <timeout>" — misattributing a §11.4.144
// transport failure to job non-completion. The fix surfaces the transport error
// (empty stdout WITH err, per the Runner contract) immediately. Driven through the
// REAL SSHRunner (post-RX-F1: ssh 255 surfaces) via the mockRemoteExecutor.
func TestWave17_WaitForSentinel_TransportFailure_SurfacesError(t *testing.T) {
	host := remote.RemoteHost{Name: "nezha"}
	refused := fmt.Errorf("ssh exec on nezha exited with code 255: " +
		"ssh: connect to host nezha port 22: Connection refused")
	r := NewSSHRunner(&mockRemoteExecutor{
		execFunc: execReturning(&remote.CommandResult{ExitCode: 255, Stdout: ""}, refused),
	}, host)
	h := handleForDir("qa-matrix", "/abs/cache/remoteexec")

	rc, err := WaitForSentinel(context.Background(), r, h,
		5*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err,
		"an unreachable host must surface the transport failure, not poll to timeout")
	assert.Equal(t, -1, rc)
	assert.Contains(t, err.Error(), "Connection refused",
		"must wrap the REAL transport error")
	assert.NotContains(t, err.Error(), "did not appear within",
		"must NOT misattribute unreachability to a job timeout")
}

// TestWave17_WaitForSentinel_ReachableAbsentSentinel_KeepsPolling is the control:
// a REACHABLE host whose sentinel is not yet present (cat of a missing file → a
// benign exit 1 reported via ExitCode with a nil error) must NOT surface an error
// — it must keep polling and hit the honest timeout. Guards against the fix
// over-firing on the routine "not done yet" poll.
func TestWave17_WaitForSentinel_ReachableAbsentSentinel_KeepsPolling(t *testing.T) {
	host := remote.RemoteHost{Name: "nezha"}
	// cat of a missing file: exit 1, empty stdout, and (post-RX-F1) a swallowed
	// error — i.e. a genuine remote non-zero exit, NOT a transport failure.
	catMiss := fmt.Errorf("ssh exec on nezha exited with code 1: ")
	r := NewSSHRunner(&mockRemoteExecutor{
		execFunc: execReturning(&remote.CommandResult{ExitCode: 1, Stdout: ""}, catMiss),
	}, host)
	h := handleForDir("qa-matrix", "/abs/cache/remoteexec")

	rc, err := WaitForSentinel(context.Background(), r, h,
		5*time.Millisecond, 30*time.Millisecond)
	require.Error(t, err, "sentinel never appears → honest timeout")
	assert.Equal(t, -1, rc)
	assert.Contains(t, err.Error(), "did not appear within",
		"a reachable host with an absent sentinel must time out honestly, not surface a transport error")
}
