package remoteexec

import (
	"context"
	"fmt"
	"os"
	"path"

	"digital.vasic.containers/pkg/remote"
)

// SSHRunner is a Runner backed by the existing pkg/remote.RemoteExecutor, so a
// durable job can be launched on any host the executor can reach. This is the
// production path for "long remote runs survive the SSH session ending": the
// remote host's systemd user manager owns the job, not the SSH session.
type SSHRunner struct {
	Exec remote.RemoteExecutor
	Host remote.RemoteHost
}

// NewSSHRunner returns an SSHRunner for the given executor and host.
func NewSSHRunner(exec remote.RemoteExecutor, host remote.RemoteHost) *SSHRunner {
	return &SSHRunner{Exec: exec, Host: host}
}

// Run executes command on the remote host. A non-zero remote exit code is
// reported in Result.ExitCode; only a transport failure (where no result was
// produced) is returned as the error.
func (s *SSHRunner) Run(ctx context.Context, command string) (Result, error) {
	cr, err := s.Exec.Execute(ctx, s.Host, command)
	if cr == nil {
		return Result{}, err
	}
	res := Result{Stdout: cr.Stdout, Stderr: cr.Stderr, ExitCode: cr.ExitCode}
	// pkg/remote returns a non-nil error both for a non-zero *remote command*
	// exit AND for ssh's OWN transport failures. ssh propagates a remote
	// command's status as its own exit code in [1,254], but reserves 255 for
	// its own errors (connection refused / DNS / auth / host-key), and a
	// ctx-timeout SIGKILL surfaces as a negative code. Callers of Runner read a
	// genuine remote verdict from Result.ExitCode/Stdout, so only swallow the
	// error for a [1,254] remote exit; SURFACE it for 255/negative — reporting
	// an unreachable durable-job host as "finished" is the exact
	// §11.4.108/§11.4.144 bluff the liveness accessors (IsActive/MainPID/
	// FetchLog) exist to prevent. (255 is inherently ambiguous — ssh cannot
	// distinguish a remote command's 255 from its own — so we treat it as
	// transport-suspect; none of this package's commands exit 255.)
	if err != nil && cr.ExitCode >= 1 && cr.ExitCode <= 254 {
		return res, nil
	}
	return res, err
}

// WriteFile uploads content to remotePath on the host via a local temp file +
// the executor's CopyFile, then fixes the mode remotely.
func (s *SSHRunner) WriteFile(ctx context.Context, remotePath string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp("", "remoteexec-*")
	if err != nil {
		return fmt.Errorf("remoteexec: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("remoteexec: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remoteexec: close temp: %w", err)
	}
	if _, err := s.Exec.Execute(ctx, s.Host, "mkdir -p "+shQuote(path.Dir(remotePath))); err != nil {
		return fmt.Errorf("remoteexec: remote mkdir: %w", err)
	}
	if err := s.Exec.CopyFile(ctx, s.Host, tmp.Name(), remotePath); err != nil {
		return fmt.Errorf("remoteexec: copy to %s: %w", remotePath, err)
	}
	if _, err := s.Exec.Execute(ctx, s.Host, fmt.Sprintf("chmod %o %s", mode.Perm(), shQuote(remotePath))); err != nil {
		return fmt.Errorf("remoteexec: chmod %s: %w", remotePath, err)
	}
	return nil
}

var _ Runner = (*SSHRunner)(nil)
