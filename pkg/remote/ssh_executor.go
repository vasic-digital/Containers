package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// SSHExecutor implements RemoteExecutor using the ssh/scp CLI tools
// with optional ControlMaster connection pooling.
type SSHExecutor struct {
	opts   Options
	pool   *ConnectionPool
	logger logging.Logger
}

// NewSSHExecutor creates an SSHExecutor with the given options.
func NewSSHExecutor(
	logger logging.Logger, opts ...Option,
) (*SSHExecutor, error) {
	o := ApplyOptions(opts)
	if logger == nil {
		logger = logging.NopLogger{}
	}

	var pool *ConnectionPool
	if o.ControlMasterEnabled {
		var err error
		pool, err = NewConnectionPool(o)
		if err != nil {
			return nil, fmt.Errorf(
				"create connection pool: %w", err,
			)
		}
	}

	return &SSHExecutor{
		opts:   o,
		pool:   pool,
		logger: logger,
	}, nil
}

// Execute runs a command on the remote host and returns its output.
func (e *SSHExecutor) Execute(
	ctx context.Context,
	host RemoteHost,
	command string,
) (*CommandResult, error) {
	start := time.Now()

	if e.opts.CommandTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.opts.CommandTimeout)
		defer cancel()
	}

	args, err := e.sshArgs(ctx, host)
	if err != nil {
		return nil, err
	}
	// sshArgs may have acquired a pooled ControlMaster connection (refs++).
	// Release on EVERY return path (defer), not just the success path below —
	// otherwise the ref-count leaks on the error returns and the socket is
	// never ControlPersist-reaped (a long-lived consumer pins one socket per
	// host that ever errored). Release is a no-op when nothing was acquired.
	if e.pool != nil {
		defer e.pool.Release(host)
	}
	args = append(args, command)

	e.logger.Debug("ssh exec on %s: %s", host.Name, command)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	result := &CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, fmt.Errorf(
				"ssh exec on %s exited with code %d: %s",
				host.Name, result.ExitCode, stderr.String(),
			)
		}
		return result, fmt.Errorf(
			"ssh exec on %s: %w", host.Name, runErr,
		)
	}

	return result, nil
}

// ExecuteStream runs a command on the remote host and returns a
// reader for streaming its output.
func (e *SSHExecutor) ExecuteStream(
	ctx context.Context,
	host RemoteHost,
	command string,
) (io.ReadCloser, error) {
	args, err := e.sshArgs(ctx, host)
	if err != nil {
		return nil, err
	}
	args = append(args, command)

	e.logger.Debug(
		"ssh stream on %s: %s", host.Name, command,
	)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf(
			"ssh stream start on %s: %w", host.Name, err,
		)
	}

	return &streamReader{
		cmd:    cmd,
		reader: stdout,
		pool:   e.pool,
		host:   host,
	}, nil
}

// CopyFile copies a local file to a remote host using scp.
func (e *SSHExecutor) CopyFile(
	ctx context.Context,
	host RemoteHost,
	localPath, remotePath string,
) error {
	args := e.scpArgs(host)
	args = append(args,
		localPath,
		fmt.Sprintf(
			"%s@%s:%s", host.User, host.Address, remotePath,
		),
	)

	e.logger.Debug(
		"scp file to %s: %s -> %s",
		host.Name, localPath, remotePath,
	)

	cmd := exec.CommandContext(ctx, "scp", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"scp to %s: %w (stderr: %s)",
			host.Name, err, stderr.String(),
		)
	}
	return nil
}

// CopyDir copies a local directory to a remote host so that, after
// the call, `remoteDir` on the remote host has the same contents as
// `localDir` on the local host.
//
// The natural `scp -r local remote` doesn't give those semantics:
// when `remote` already exists, scp nests (creates
// `remote/<basename(local)>`). To get rsync-style "make dest contain
// what source contains" without race-hostile pre-clean (which broke
// concurrent CopyFile siblings whose parent overlapped with the
// destination), this implementation invokes scp with the PARENT of
// remoteDir as the destination, relying on scp's "copy named source
// into named parent" rule. As long as basename(localDir) ==
// basename(remoteDir) — the universal case for the helixagent build
// context flow — the result is deterministic regardless of pre-state:
//
//	scp -r /local/external/cognee user@host:/remote/external
//	  → creates/merges at /remote/external/cognee
//
// Forensic note (2026-04-29): the previous implementation passed
// remoteDir directly to scp. When a sibling CopyFile created remoteDir
// as a side-effect of its own `mkdir -p`, the subsequent CopyDir
// nested its source inside the now-existing remoteDir. Cognee broke
// remotely because /remote/external/cognee/README.md ended up at
// /remote/external/cognee/cognee/README.md (one level too deep), and
// the COPY directive in the Dockerfile failed with
// `copier: stat: "/README.md": no such file or directory`. The
// "scp into parent" pattern eliminates the dependence on remoteDir's
// pre-state.
func (e *SSHExecutor) CopyDir(
	ctx context.Context,
	host RemoteHost,
	localDir, remoteDir string,
) error {
	localBase := filepath.Base(localDir)
	remoteBase := filepath.Base(remoteDir)
	remoteParent := filepath.Dir(remoteDir)

	args := e.scpArgs(host)
	args = append([]string{"-r"}, args...)

	if localBase == remoteBase {
		// Universal case: copy source into the PARENT of the remote
		// destination so scp's basename rule lands the contents where
		// the caller expects, without any pre-state assumption.
		args = append(args,
			localDir,
			fmt.Sprintf("%s@%s:%s", host.User, host.Address, remoteParent),
		)
		e.logger.Debug(
			"scp dir to %s: %s -> %s (via parent %s)",
			host.Name, localDir, remoteDir, remoteParent,
		)
	} else {
		// Rare: basenames differ. We cannot rely on scp's basename rule
		// to land the contents at the right location, so we fall back
		// to a defensive pre-clean of the destination + scp directly
		// to the destination. This path is race-prone if a sibling
		// CopyFile is concurrently writing into the same parent, but
		// it's also rarely exercised in practice.
		cleanCmd := fmt.Sprintf("rm -rf -- %s", shellEscape(remoteDir))
		if sshArgsList, sshErr := e.sshArgs(ctx, host); sshErr == nil {
			cleanArgs := append(sshArgsList, cleanCmd)
			cleanCmdRun := exec.CommandContext(ctx, "ssh", cleanArgs...)
			var cleanStderr bytes.Buffer
			cleanCmdRun.Stderr = &cleanStderr
			if err := cleanCmdRun.Run(); err != nil {
				e.logger.Debug(
					"pre-scp rm on %s (ignored): %s -> %s: %v (stderr: %s)",
					host.Name, localDir, remoteDir, err, cleanStderr.String(),
				)
			}
		}
		args = append(args,
			localDir,
			fmt.Sprintf("%s@%s:%s", host.User, host.Address, remoteDir),
		)
		e.logger.Debug(
			"scp dir to %s (basename mismatch): %s -> %s",
			host.Name, localDir, remoteDir,
		)
	}

	cmd := exec.CommandContext(ctx, "scp", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"scp dir to %s: %w (stderr: %s)",
			host.Name, err, stderr.String(),
		)
	}
	return nil
}

// shellEscape wraps an arbitrary path in single quotes for safe inclusion
// in a remote shell command, doubling any embedded single quotes.
func shellEscape(s string) string {
	if !needsShellEscape(s) {
		return s
	}
	const single = `'`
	const escape = `'\''`
	return single + replaceAll(s, single, escape) + single
}

func needsShellEscape(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '/' || r == '.' || r == '-':
		default:
			return true
		}
	}
	return false
}

func replaceAll(s, old, new string) string {
	if old == new || old == "" {
		return s
	}
	out := ""
	i := 0
	for i < len(s) {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			out += new
			i += len(old)
		} else {
			out += string(s[i])
			i++
		}
	}
	return out
}

// IsReachable checks whether the host accepts SSH connections.
func (e *SSHExecutor) IsReachable(
	ctx context.Context, host RemoteHost,
) bool {
	timeout := e.opts.ConnectTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := e.Execute(ctx, host, "echo ok")
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Stdout) == "ok"
}

// Close releases any resources held by the executor.
func (e *SSHExecutor) Close() error {
	if e.pool != nil {
		return e.pool.Close()
	}
	return nil
}

func (e *SSHExecutor) sshArgs(
	ctx context.Context, host RemoteHost,
) ([]string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=" +
			boolToYesNo(e.opts.StrictHostKeyCheck),
		"-o", fmt.Sprintf(
			"ConnectTimeout=%d",
			int(e.opts.ConnectTimeout.Seconds()),
		),
		"-o", "BatchMode=yes",
	}

	// Keep-alive probes: send every KeepAlive seconds; drop the
	// session after KeepAliveCountMax missed probes. This keeps
	// the TCP channel alive during long-running remote operations
	// (e.g. multi-minute image builds over compose up).
	if e.opts.KeepAlive > 0 && e.opts.KeepAliveCountMax > 0 {
		args = append(args,
			"-o", fmt.Sprintf(
				"ServerAliveInterval=%d",
				int(e.opts.KeepAlive.Seconds()),
			),
			"-o", fmt.Sprintf(
				"ServerAliveCountMax=%d",
				e.opts.KeepAliveCountMax,
			),
		)
	}

	args = append(args, "-p", strconv.Itoa(host.SSHPort()))

	if e.pool != nil && e.opts.ControlMasterEnabled {
		socketPath, err := e.pool.Acquire(ctx, host)
		if err != nil {
			e.logger.Warn(
				"ControlMaster unavailable for %s, "+
					"falling back to direct: %v",
				host.Name, err,
			)
		} else {
			args = append(args, "-S", socketPath)
		}
	}

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", host.User, host.Address),
	)
	return args, nil
}

func (e *SSHExecutor) scpArgs(host RemoteHost) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=" +
			boolToYesNo(e.opts.StrictHostKeyCheck),
		"-o", fmt.Sprintf(
			"ConnectTimeout=%d",
			int(e.opts.ConnectTimeout.Seconds()),
		),
		"-o", "BatchMode=yes",
	}

	// Keep scp transfers alive across network blips (same
	// rationale as sshArgs).
	if e.opts.KeepAlive > 0 && e.opts.KeepAliveCountMax > 0 {
		args = append(args,
			"-o", fmt.Sprintf(
				"ServerAliveInterval=%d",
				int(e.opts.KeepAlive.Seconds()),
			),
			"-o", fmt.Sprintf(
				"ServerAliveCountMax=%d",
				e.opts.KeepAliveCountMax,
			),
		)
	}

	args = append(args, "-P", strconv.Itoa(host.SSHPort()))

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	return args
}

// streamReader wraps an SSH command's stdout pipe.
type streamReader struct {
	cmd    *exec.Cmd
	reader io.ReadCloser
	pool   *ConnectionPool
	host   RemoteHost
}

func (r *streamReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *streamReader) Close() error {
	_ = r.reader.Close()
	err := r.cmd.Wait()
	if r.pool != nil {
		r.pool.Release(r.host)
	}
	return err
}
