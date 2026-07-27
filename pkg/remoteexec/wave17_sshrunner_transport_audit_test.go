package remoteexec

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/remote"
)

// mockRemoteExecutor is a remote.RemoteExecutor whose Execute is driven by a
// closure; the other methods are inert. It lets these tests exercise the REAL
// SSHRunner.Run (and the durable-job callers over it) against the exact
// (CommandResult, error) shapes pkg/remote emits — unlike fakeRunner, which
// mocks the Runner interface and so cannot catch a swallow inside SSHRunner.Run.
type mockRemoteExecutor struct {
	execFunc func(ctx context.Context, host remote.RemoteHost, command string) (*remote.CommandResult, error)
}

func (m *mockRemoteExecutor) Execute(ctx context.Context, host remote.RemoteHost, command string) (*remote.CommandResult, error) {
	return m.execFunc(ctx, host, command)
}
func (m *mockRemoteExecutor) ExecuteStream(context.Context, remote.RemoteHost, string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockRemoteExecutor) CopyFile(context.Context, remote.RemoteHost, string, string) error {
	return nil
}
func (m *mockRemoteExecutor) CopyDir(context.Context, remote.RemoteHost, string, string) error {
	return nil
}
func (m *mockRemoteExecutor) IsReachable(context.Context, remote.RemoteHost) bool { return true }

var _ remote.RemoteExecutor = (*mockRemoteExecutor)(nil)

func execReturning(cr *remote.CommandResult, err error) func(context.Context, remote.RemoteHost, string) (*remote.CommandResult, error) {
	return func(context.Context, remote.RemoteHost, string) (*remote.CommandResult, error) { return cr, err }
}

// TestWave17_SSHRunner_Run_TransportVsRemoteExit proves SSHRunner.Run only
// swallows the pkg/remote error for a GENUINE remote command exit (1..254). ssh
// reserves 255 for its OWN transport failures (connection refused / DNS / auth),
// and a ctx-timeout SIGKILL yields a negative exit code — both MUST surface as an
// error, not be reported as a benign remote exit (which would make IsActive/
// MainPID/FetchLog report an unreachable durable-job host as "finished" —
// §11.4.108 / §11.4.144 bluff). The pre-fix guard `cr.ExitCode != 0` swallowed
// all of these; the polarity harness reverts the window.
func TestWave17_SSHRunner_Run_TransportVsRemoteExit(t *testing.T) {
	host := remote.RemoteHost{Name: "nezha"}
	newRunner := func(cr *remote.CommandResult, err error) *SSHRunner {
		return NewSSHRunner(&mockRemoteExecutor{execFunc: execReturning(cr, err)}, host)
	}

	t.Run("SSH255_TransportFailure_SurfacesError", func(t *testing.T) {
		r := newRunner(&remote.CommandResult{ExitCode: 255, Stdout: ""},
			fmt.Errorf("ssh exec on nezha exited with code 255: ssh: connect to host nezha port 22: Connection refused"))
		_, err := r.Run(context.Background(), "systemctl --user is-active qa-matrix")
		require.Error(t, err, "ssh's own 255 transport error must NOT be swallowed as a benign remote exit")
	})

	t.Run("NegativeExit_SIGKILL_SurfacesError", func(t *testing.T) {
		r := newRunner(&remote.CommandResult{ExitCode: -1, Stdout: ""},
			fmt.Errorf("ssh exec on nezha exited with code -1: signal: killed"))
		_, err := r.Run(context.Background(), "cat /sentinel")
		require.Error(t, err, "a SIGKILL'd ssh (ctx timeout, exit -1) must surface as an error, not a benign exit")
	})

	// Control: a real remote non-zero exit (systemctl is-active → 3) is a verdict,
	// reported via ExitCode with a nil error. Passes pre- AND post-fix.
	t.Run("BenignRemoteExit3_Swallowed_VerdictViaExitCode", func(t *testing.T) {
		r := newRunner(&remote.CommandResult{ExitCode: 3, Stdout: "inactive\n"},
			fmt.Errorf("ssh exec on nezha exited with code 3: "))
		res, err := r.Run(context.Background(), "systemctl --user is-active qa-matrix")
		require.NoError(t, err, "a genuine remote non-zero exit (1..254) is a verdict, not a transport error")
		assert.Equal(t, 3, res.ExitCode)
		assert.Equal(t, "inactive\n", res.Stdout)
	})

	// Control: a could-not-exec failure (non-ExitError → ExitCode 0 + err) already
	// surfaced pre-fix and must keep surfacing.
	t.Run("ExecFailure_ExitCode0_SurfacesError", func(t *testing.T) {
		r := newRunner(&remote.CommandResult{ExitCode: 0, Stdout: ""},
			fmt.Errorf("ssh exec on nezha: fork/exec ssh: no such file or directory"))
		_, err := r.Run(context.Background(), "systemctl --user is-active qa-matrix")
		require.Error(t, err, "a could-not-execute failure (ExitCode 0 + err) must surface")
	})
}

// TestWave17_SSHRunner_UnreachableHost_NotReportedFinished is the end-to-end
// proof that the §11.4.108 bluff is closed THROUGH the real SSHRunner: an
// unreachable host (ssh 255 + empty stdout) must not be reported by any durable-
// job liveness accessor as a finished/inactive job with a nil error. Pre-fix
// SSHRunner.Run swallowed the 255 error, so IsActive/MainPID/FetchLog all
// returned their "finished/empty" verdict with err==nil.
func TestWave17_SSHRunner_UnreachableHost_NotReportedFinished(t *testing.T) {
	host := remote.RemoteHost{Name: "nezha"}
	refused := fmt.Errorf("ssh exec on nezha exited with code 255: ssh: connect to host nezha port 22: Connection refused")
	r := NewSSHRunner(&mockRemoteExecutor{
		execFunc: execReturning(&remote.CommandResult{ExitCode: 255, Stdout: ""}, refused),
	}, host)

	t.Run("IsActive", func(t *testing.T) {
		active, err := IsActive(context.Background(), r, "qa-matrix")
		require.Error(t, err, "an unreachable host must NOT be reported as a finished/inactive durable job")
		assert.False(t, active)
	})
	t.Run("MainPID", func(t *testing.T) {
		pid, err := MainPID(context.Background(), r, "qa-matrix")
		require.Error(t, err, "MainPID on an unreachable host must surface the transport failure")
		assert.Zero(t, pid)
	})
	t.Run("FetchLog", func(t *testing.T) {
		h := handleForDir("qa-matrix", "/abs/cache/remoteexec")
		_, err := FetchLog(context.Background(), r, h)
		require.Error(t, err, "FetchLog on an unreachable host must surface the transport failure, not return empty output")
	})
}
