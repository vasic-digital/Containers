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
	// pkg/remote returns a non-nil error for a non-zero remote exit. Callers
	// of Runner treat exit codes via Result.ExitCode, so only surface the
	// error when the command did not run (no exit code captured).
	if err != nil && cr.ExitCode != 0 {
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
