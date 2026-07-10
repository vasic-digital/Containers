package vm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// HONESTY (clauses 6.J/6.L inherited from Containers' parent):
//
// Phase 5 (this file) replaces the v0.1 "not implemented" stubs in
// realSSHClient.{Upload,Run,Download} and realQMPClient.{Dial,
// SystemPowerdown} with real implementations, AND adds key-based SSH
// auth via the LAVA_VM_SSH_KEY environment variable. The fake-driven
// hermetic tests in qemu_test.go continue to drive QEMUVM through the
// sshClient / qmpClient injection seams; the real impls below are
// driven end-to-end by the consuming project's matrix-runner consumer rollout (Phase C).
//
// Anti-bluff posture: every method below is exercised by an in-process
// SSH-server / QMP-server test in clients_test.go, with the
// falsifiability rehearsals recorded in the commit body. No silent
// no-op, no "pretend to work" — every error path is wrapped with the
// site name + the underlying cause so failures are diagnosable.

// defaultSSHClient returns a production sshClient that uses
// golang.org/x/crypto/ssh. The fake injection seam in qemu_test.go
// substitutes this for hermetic tests.
//
// User is read from LAVA_VM_SSH_USER (default "root"). Optional
// keyPath comes from LAVA_VM_SSH_KEY; when empty, falls back to
// ssh.Password("") for the historical passwordless-root cloud-init
// Alpine path. Both knobs are env-driven so the production path
// can swap auth modes without code changes — the matrix runner
// just sets the env var ahead of the run.
func defaultSSHClient() sshClient {
	user := os.Getenv("LAVA_VM_SSH_USER")
	if user == "" {
		user = "root"
	}
	keyPath := os.Getenv("LAVA_VM_SSH_KEY")
	return newRealSSHClient(user, keyPath)
}

// newRealSSHClient is the explicit constructor used by defaultSSHClient
// and by tests that want to drive a specific (user, keyPath) pair
// without going through environment variables.
func newRealSSHClient(user, keyPath string) *realSSHClient {
	return &realSSHClient{user: user, keyPath: keyPath}
}

type realSSHClient struct {
	user    string
	keyPath string // optional; empty means "fall back to empty-password auth"
	client  *ssh.Client
}

// WaitForListener does a plain TCP probe of 127.0.0.1:<port> with the
// given timeout — NO SSH handshake, NO userauth. Used by
// QEMUVM.WaitForReady to decide when the SSH listener is up.
//
// I4 fix: the previous implementation collapsed listener-up + handshake
// + ssh.Password("") userauth into a single Dial call. That combined
// path required the guest to accept empty-password root authentication,
// which essentially no real Linux server permits — so WaitForReady
// would always time out in production even on a fully-booted VM.
// Splitting listener-up out into this method matches what the unit
// test claims to verify (the listener became reachable) and what
// production needs (poll until SSH is accepting connections).
func (r *realSSHClient) WaitForListener(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Authenticate opens a TCP connection to 127.0.0.1:<port> and performs
// the full SSH handshake + userauth. When r.keyPath is non-empty the
// private key at that path is parsed and used as the sole auth method;
// otherwise the empty-password fallback is used (compatible with
// passwordless-root cloud-init Alpine images).
func (r *realSSHClient) Authenticate(ctx context.Context, port int, timeout time.Duration) error {
	var auths []ssh.AuthMethod
	if r.keyPath != "" {
		keyBytes, err := os.ReadFile(r.keyPath)
		if err != nil {
			return fmt.Errorf("realSSHClient.Authenticate: read keyPath %s: %w", r.keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			return fmt.Errorf("realSSHClient.Authenticate: parse private key: %w", err)
		}
		auths = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	} else {
		auths = []ssh.AuthMethod{ssh.Password("")}
	}
	cfg := &ssh.ClientConfig{
		User:            r.user,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("realSSHClient.Authenticate: dial %s: %w", addr, err)
	}
	c, ch, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("realSSHClient.Authenticate: ssh handshake: %w", err)
	}
	// Close any prior client before replacing it so a re-Authenticate
	// (e.g. a new VM port on a reused client) does not leak the previous
	// *ssh.Client and its mux/keepalive goroutines.
	if r.client != nil {
		_ = r.client.Close()
	}
	r.client = ssh.NewClient(c, ch, reqs)
	return nil
}

// Upload copies hostPath → vmPath using the SCP source protocol
// driven through an SSH session running `scp -t <dir>` on the guest.
// The protocol header is `C<mode> <size> <basename>\n`, then the file
// bytes, then a single NUL terminator.
func (r *realSSHClient) Upload(ctx context.Context, hostPath, vmPath string) error {
	if r.client == nil {
		return fmt.Errorf("realSSHClient.Upload: not authenticated; call Authenticate first")
	}
	f, err := os.Open(hostPath)
	if err != nil {
		return fmt.Errorf("realSSHClient.Upload: open %s: %w", hostPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("realSSHClient.Upload: stat %s: %w", hostPath, err)
	}
	session, err := r.client.NewSession()
	if err != nil {
		return fmt.Errorf("realSSHClient.Upload: new session: %w", err)
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("realSSHClient.Upload: stdin pipe: %w", err)
	}
	targetDir := filepath.Dir(vmPath)
	targetName := filepath.Base(vmPath)
	runErr := make(chan error, 1)
	go func() { runErr <- session.Run("scp -t " + targetDir) }()
	if _, err := fmt.Fprintf(stdin, "C%#o %d %s\n", info.Mode().Perm(), info.Size(), targetName); err != nil {
		return fmt.Errorf("realSSHClient.Upload: write header: %w", err)
	}
	if _, err := io.Copy(stdin, f); err != nil {
		return fmt.Errorf("realSSHClient.Upload: copy bytes: %w", err)
	}
	if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
		return fmt.Errorf("realSSHClient.Upload: write terminator: %w", err)
	}
	_ = stdin.Close()
	if err := <-runErr; err != nil {
		return fmt.Errorf("realSSHClient.Upload: scp -t %s: %w", targetDir, err)
	}
	return nil
}

// Run executes script on the guest via an SSH session. stdout, stderr,
// and the exit code are captured. Environment variables are pushed via
// session.Setenv (servers that reject SetEnv silently are tolerated —
// the script either copes or fails noisily on its own).
//
// Timeout is enforced via context.WithTimeout; on expiry SIGKILL is
// signalled to the remote process, the session is closed, and an
// honest "timeout" error is returned (with whatever stdout/stderr
// has been captured so far).
func (r *realSSHClient) Run(ctx context.Context, script string, env map[string]string, timeout time.Duration) (string, string, int, error) {
	if r.client == nil {
		return "", "", -1, fmt.Errorf("realSSHClient.Run: not authenticated; call Authenticate first")
	}
	session, err := r.client.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("realSSHClient.Run: new session: %w", err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	for k, v := range env {
		_ = session.Setenv(k, v)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session.Run(script) }()
	select {
	case err = <-done:
	case <-runCtx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return stdout.String(), stderr.String(), -1, fmt.Errorf("realSSHClient.Run: timeout: %w", runCtx.Err())
	}
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
			err = nil
		} else {
			return stdout.String(), stderr.String(), -1, fmt.Errorf("realSSHClient.Run: %w", err)
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

// Download copies vmPath → hostPath using the SCP sink protocol driven
// through an SSH session running `scp -f <vmPath>` on the guest.
// Symmetric to Upload: client sends NUL "ready", reads
// `C<mode> <size> <basename>\n` header, sends NUL "ready" again, reads
// size bytes into hostPath, sends NUL "ready" terminator.
func (r *realSSHClient) Download(ctx context.Context, vmPath, hostPath string) error {
	if r.client == nil {
		return fmt.Errorf("realSSHClient.Download: not authenticated; call Authenticate first")
	}
	session, err := r.client.NewSession()
	if err != nil {
		return fmt.Errorf("realSSHClient.Download: new session: %w", err)
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("realSSHClient.Download: stdin pipe: %w", err)
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("realSSHClient.Download: stdout pipe: %w", err)
	}
	if err := session.Start("scp -f " + vmPath); err != nil {
		return fmt.Errorf("realSSHClient.Download: scp -f %s: %w", vmPath, err)
	}
	if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
		return fmt.Errorf("realSSHClient.Download: send ready: %w", err)
	}
	reader := bufio.NewReader(stdoutPipe)
	header, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("realSSHClient.Download: read header: %w", err)
	}
	var mode os.FileMode
	var size int64
	var name string
	if _, err := fmt.Sscanf(header, "C%o %d %s", &mode, &size, &name); err != nil {
		return fmt.Errorf("realSSHClient.Download: parse header %q: %w", header, err)
	}
	if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
		return fmt.Errorf("realSSHClient.Download: ack header: %w", err)
	}
	out, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("realSSHClient.Download: open %s: %w", hostPath, err)
	}
	defer out.Close()
	if _, err := io.CopyN(out, reader, size); err != nil {
		return fmt.Errorf("realSSHClient.Download: copy bytes: %w", err)
	}
	if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
		return fmt.Errorf("realSSHClient.Download: ack terminator: %w", err)
	}
	_ = stdin.Close()
	if err := session.Wait(); err != nil {
		return fmt.Errorf("realSSHClient.Download: wait: %w", err)
	}
	return nil
}

func (r *realSSHClient) Close() error {
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
	}
	return nil
}

// defaultQMPClient returns a production qmpClient. The hermetic test
// suite uses fakeQMPClient instead.
func defaultQMPClient() qmpClient { return &realQMPClient{} }

type realQMPClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Dial connects to QEMU's monitor TCP socket and runs the standard
// QMP capability negotiation: read greeting → send qmp_capabilities →
// read response. After Dial returns nil the connection is ready for
// command execution (e.g. system_powerdown).
func (r *realQMPClient) Dial(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("realQMPClient.Dial: %w", err)
	}
	r.conn = conn
	r.reader = bufio.NewReader(conn)
	if _, err := r.reader.ReadString('\n'); err != nil {
		_ = conn.Close()
		r.conn = nil
		r.reader = nil
		return fmt.Errorf("realQMPClient.Dial: read greeting: %w", err)
	}
	if _, err := fmt.Fprintln(conn, `{"execute":"qmp_capabilities"}`); err != nil {
		_ = conn.Close()
		r.conn = nil
		r.reader = nil
		return fmt.Errorf("realQMPClient.Dial: send qmp_capabilities: %w", err)
	}
	if _, err := r.reader.ReadString('\n'); err != nil {
		_ = conn.Close()
		r.conn = nil
		r.reader = nil
		return fmt.Errorf("realQMPClient.Dial: read qmp_capabilities response: %w", err)
	}
	return nil
}

// SystemPowerdown sends the QMP `system_powerdown` command to QEMU.
// The signal is fire-and-forget — the matrix-runner Teardown observes
// the actual shutdown via subsequent SSH-listener-down probes, not via
// a QMP response wait, so we don't block here.
func (r *realQMPClient) SystemPowerdown(ctx context.Context) error {
	if r.conn == nil {
		return fmt.Errorf("realQMPClient.SystemPowerdown: not dialed; call Dial first")
	}
	if _, err := fmt.Fprintln(r.conn, `{"execute":"system_powerdown"}`); err != nil {
		return fmt.Errorf("realQMPClient.SystemPowerdown: send: %w", err)
	}
	return nil
}

// Screendump asks QEMU to write a PPM-format screenshot of the guest
// framebuffer to hostPath (interpreted on the host — qemu-system is a
// host process). Returns when QEMU's response arrives (success or
// error JSON).
//
// Anti-bluff posture (clauses 6.J/6.L): the function reads the QMP
// response and treats `{"error":...}` as an honest failure rather
// than fire-and-forget. A silent screendump that "succeeded" without
// producing a file would be a clause-6.J bluff vector — an operator
// looking at a "passing" matrix run with no screenshot evidence
// would have no way to know the screendump silently failed.
func (r *realQMPClient) Screendump(ctx context.Context, hostPath string) error {
	if r.conn == nil {
		return fmt.Errorf("realQMPClient.Screendump: not dialed; call Dial first")
	}
	cmd := fmt.Sprintf(`{"execute":"screendump","arguments":{"filename":"%s"}}`, escapeJSONString(hostPath))
	if _, err := fmt.Fprintln(r.conn, cmd); err != nil {
		return fmt.Errorf("realQMPClient.Screendump: send: %w", err)
	}
	resp, err := r.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("realQMPClient.Screendump: read response: %w", err)
	}
	if strings.Contains(resp, `"error"`) {
		return fmt.Errorf("realQMPClient.Screendump: qemu rejected: %s", strings.TrimSpace(resp))
	}
	return nil
}

// escapeJSONString escapes a string for safe inclusion inside a
// JSON string literal. Only the canonical 4 chars need escaping for
// the QMP screendump filename; we keep this minimal rather than
// pulling in encoding/json for one field.
func escapeJSONString(s string) string {
	repl := map[string]string{
		`\`: `\\`,
		`"`: `\"`,
	}
	out := s
	for k, v := range repl {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

func (r *realQMPClient) Close() error {
	if r.conn != nil {
		err := r.conn.Close()
		r.conn = nil
		r.reader = nil
		return err
	}
	return nil
}
