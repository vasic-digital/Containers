package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// shellQuote wraps s in single quotes so the guest login shell — which is what
// executes the string handed to crypto/ssh session.Run/Start — treats every byte
// of s literally (VM3-7). Without it, a guest path carrying a shell metacharacter
// (`;`, `$(...)`, backtick, `|`, `&`, whitespace, newline) breaks out of the
// `scp -t <dir>` / `scp -f <path>` command string and runs attacker-chosen
// commands on the guest — a real command-injection gap even though the blast
// radius is the local QEMU guest the matrix runner owns. Single-quoting is the
// canonical POSIX-shell neutralisation: inside single quotes every character is
// literal except `'` itself, which is emitted as the standard `'\”` splice
// (close-quote, backslash-escaped-quote, reopen-quote). The `--` the call sites
// prepend additionally stops scp's OWN getopt from parsing a leading-'-' path as
// an option once the shell has stripped the quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxSCPDownloadSize is a sanity ceiling on the size field the SCP sink
// protocol header declares (VM2-5). It is intentionally generous — real
// captured-evidence artifacts (screenshots, logs) are tiny — this exists
// only to reject a garbled/overflowed header, not to constrain real use.
const maxSCPDownloadSize = 1 << 40 // 1 TiB

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
	go func() { runErr <- session.Run("scp -t -- " + shellQuote(targetDir)) }()
	// SW1-1: run the blocking SCP source exchange in a goroutine and select on
	// ctx.Done(), mirroring Run()'s timeout pattern. The pre-fix body ran the
	// header write, io.Copy(stdin, f), terminator write, and the final
	// <-runErr synchronously with NO ctx path: a guest that wedges mid-transfer
	// (stdin buffer full so io.Copy blocks, or a scp -t that never returns so
	// <-runErr blocks) hung this call FOREVER, stalling matrix.go's bounded
	// worker pool past its deadline with no recovery. xferErr is buffered
	// (cap 1) so the exchange goroutine can always deliver its result and never
	// leaks when the ctx-cancel path returns first.
	xferErr := make(chan error, 1)
	go func() {
		if _, err := fmt.Fprintf(stdin, "C%#o %d %s\n", info.Mode().Perm(), info.Size(), targetName); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Upload: write header: %w", err)
			return
		}
		if _, err := io.Copy(stdin, f); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Upload: copy bytes: %w", err)
			return
		}
		if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Upload: write terminator: %w", err)
			return
		}
		_ = stdin.Close()
		if err := <-runErr; err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Upload: scp -t %s: %w", targetDir, err)
			return
		}
		xferErr <- nil
	}()
	select {
	case err := <-xferErr:
		return err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return fmt.Errorf("realSSHClient.Upload: timeout: %w", ctx.Err())
	}
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
	if err := session.Start("scp -f -- " + shellQuote(vmPath)); err != nil {
		return fmt.Errorf("realSSHClient.Download: scp -f %s: %w", vmPath, err)
	}
	// SW1-1: run the blocking SCP sink exchange in a goroutine and select on
	// ctx.Done(), mirroring Run()'s timeout pattern. The pre-fix body was fully
	// synchronous with NO ctx path: a guest whose scp -f never sends the header
	// (reader.ReadString blocks), stalls the body (io.CopyN blocks), or never
	// exits (session.Wait blocks) hung this call FOREVER. The VM2-5 negative /
	// oversize-size guards stay INSIDE this goroutine, BEFORE the header ack and
	// the dest-file open, so a garbled header still can never produce a false
	// "success" artifact. xferErr is buffered (cap 1) so the exchange goroutine
	// never leaks when the ctx-cancel path returns first.
	xferErr := make(chan error, 1)
	go func() {
		if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: send ready: %w", err)
			return
		}
		reader := bufio.NewReader(stdoutPipe)
		header, err := reader.ReadString('\n')
		if err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: read header: %w", err)
			return
		}
		var mode os.FileMode
		var size int64
		var name string
		if _, err := fmt.Sscanf(header, "C%o %d %s", &mode, &size, &name); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: parse header %q: %w", header, err)
			return
		}
		// VM2-5: a negative (or implausibly large) size in the SCP header is a
		// garbled/corrupted response. Without this guard, io.CopyN(out, reader,
		// size) with a negative size returns (0, nil) immediately — a "clean"
		// 0-byte file that matrix.go's captor (Download err == nil) records as
		// genuine captured evidence. Reject BEFORE ack'ing the header or
		// creating the destination file, so a garbled header can never produce
		// a false "success" artifact.
		if size < 0 {
			xferErr <- fmt.Errorf("realSSHClient.Download: header %q declares negative size %d", strings.TrimSpace(header), size)
			return
		}
		if size > maxSCPDownloadSize {
			xferErr <- fmt.Errorf("realSSHClient.Download: header %q declares implausible size %d (max %d)", strings.TrimSpace(header), size, maxSCPDownloadSize)
			return
		}
		if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: ack header: %w", err)
			return
		}
		out, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
		if err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: open %s: %w", hostPath, err)
			return
		}
		defer out.Close()
		if _, err := io.CopyN(out, reader, size); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: copy bytes: %w", err)
			return
		}
		if _, err := fmt.Fprint(stdin, "\x00"); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: ack terminator: %w", err)
			return
		}
		_ = stdin.Close()
		if err := session.Wait(); err != nil {
			xferErr <- fmt.Errorf("realSSHClient.Download: wait: %w", err)
			return
		}
		xferErr <- nil
	}()
	select {
	case err := <-xferErr:
		return err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return fmt.Errorf("realSSHClient.Download: timeout: %w", ctx.Err())
	}
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

// qmpDefaultOpTimeout bounds a QMP conn read/write when ctx carries no
// deadline of its own (VM2-1). Chosen to comfortably exceed QEMU's
// normal command turnaround (milliseconds) while still failing fast
// against a genuinely wedged monitor socket.
const qmpDefaultOpTimeout = 10 * time.Second

// qmpDeadline derives an absolute deadline for one QMP conn operation:
// ctx's own deadline when it carries one, else now+qmpDefaultOpTimeout.
//
// VM2-1: realQMPClient previously used ctx for nothing beyond Dial's
// initial net.DialTimeout — every post-connect handshake read
// (ReadString('\n') for the greeting + the qmp_capabilities reply) and
// every SystemPowerdown/Screendump write+read had no conn deadline and
// no ctx cancellation. A QEMU that accepts the TCP connect but stalls
// before speaking (crashed mid-init, wedged monitor thread) blocks the
// call FOREVER. Because Dial is invoked while QEMUVM.guestMu is held
// (Teardown Stage 1 in teardown.go, CaptureScreenshotVM in
// screenshot.go), one wedged VM's monitor socket would permanently
// block Upload/Run/Download/ApplyNetworkConditions/CaptureScreenshot/
// Teardown for EVERY concurrent target sharing that *QEMUVM.
func qmpDeadline(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(qmpDefaultOpTimeout)
}

// Dial connects to QEMU's monitor TCP socket and runs the standard
// QMP capability negotiation: read greeting → send qmp_capabilities →
// read response. After Dial returns nil the connection is ready for
// command execution (e.g. system_powerdown). The whole post-connect
// handshake is bounded by a conn deadline (VM2-1) — see qmpDeadline's
// doc comment for why an unbounded handshake is unsafe under
// concurrent targets sharing one *QEMUVM.
func (r *realQMPClient) Dial(ctx context.Context, port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("realQMPClient.Dial: %w", err)
	}
	r.conn = conn
	r.reader = bufio.NewReader(conn)
	if err := conn.SetDeadline(qmpDeadline(ctx)); err != nil {
		_ = conn.Close()
		r.conn = nil
		r.reader = nil
		return fmt.Errorf("realQMPClient.Dial: set deadline: %w", err)
	}
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
// a QMP response wait, so we don't block here. The write itself is
// still bounded by a conn deadline (VM2-1): a wedged monitor socket
// must not hang the write indefinitely under guestMu.
func (r *realQMPClient) SystemPowerdown(ctx context.Context) error {
	if r.conn == nil {
		return fmt.Errorf("realQMPClient.SystemPowerdown: not dialed; call Dial first")
	}
	if err := r.conn.SetWriteDeadline(qmpDeadline(ctx)); err != nil {
		return fmt.Errorf("realQMPClient.SystemPowerdown: set write deadline: %w", err)
	}
	if _, err := fmt.Fprintln(r.conn, `{"execute":"system_powerdown"}`); err != nil {
		return fmt.Errorf("realQMPClient.SystemPowerdown: send: %w", err)
	}
	return nil
}

// qmpEnvelope is the line-delimited JSON envelope every QMP line uses —
// either the actual command response (`{"return":...}` / `{"error":...}`)
// or an asynchronous `{"event":...}` notification that QEMU can emit at
// any time, including in the window between a command and its own
// response (VM2-3).
type qmpEnvelope struct {
	Event  string          `json:"event,omitempty"`
	Return json.RawMessage `json:"return,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// qmpMaxAsyncEventsSkipped bounds how many async {"event":...} lines
// readCommandResponse will skip before giving up. Defence in depth
// alongside the conn deadline (VM2-1), which is the primary bound on
// wall-clock time; this bounds iteration count independent of timing.
const qmpMaxAsyncEventsSkipped = 32

// readCommandResponse reads QMP lines, skipping any asynchronous
// {"event":...} notification, until it finds the actual command
// response ({"return":...} or {"error":...}) or hits an error/limit.
//
// VM2-3: the pre-fix Screendump decided success/failure via
// strings.Contains(resp, `"error"`) on a SINGLE ReadString('\n') line,
// with no JSON discrimination of {"return":...} vs {"error":...} vs an
// async {"event":...}. A QMP async event landing between the command
// and its real response was mis-read: a false PASS when the event line
// itself had no "error" key, or a false FAIL if an unrelated event's
// payload happened to contain the substring "error". Parsing each line
// as JSON and skipping event lines removes both false-PASS and
// false-FAIL vectors.
func (r *realQMPClient) readCommandResponse() (qmpEnvelope, error) {
	for i := 0; i < qmpMaxAsyncEventsSkipped; i++ {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return qmpEnvelope{}, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var env qmpEnvelope
		if jerr := json.Unmarshal([]byte(trimmed), &env); jerr != nil {
			return qmpEnvelope{}, fmt.Errorf("parse QMP line %q: %w", trimmed, jerr)
		}
		if env.Event != "" {
			continue // async event landed between our command and its response; skip it
		}
		return env, nil
	}
	return qmpEnvelope{}, fmt.Errorf("no command response after skipping %d async event(s)", qmpMaxAsyncEventsSkipped)
}

// Screendump asks QEMU to write a PPM-format screenshot of the guest
// framebuffer to hostPath (interpreted on the host — qemu-system is a
// host process). Returns when QEMU's response arrives (success or
// error JSON).
//
// Anti-bluff posture (clauses 6.J/6.L): the function reads QMP lines
// via readCommandResponse (VM2-3), discriminating the real command
// response from any async {"event":...} line, and treats a genuine
// {"error":...} response as an honest failure rather than fire-and-
// forget. A silent screendump that "succeeded" without producing a
// file would be a clause-6.J bluff vector — an operator looking at a
// "passing" matrix run with no screenshot evidence would have no way
// to know the screendump silently failed. The write+read are bounded
// by a conn deadline (VM2-1).
func (r *realQMPClient) Screendump(ctx context.Context, hostPath string) error {
	if r.conn == nil {
		return fmt.Errorf("realQMPClient.Screendump: not dialed; call Dial first")
	}
	cmd := fmt.Sprintf(`{"execute":"screendump","arguments":{"filename":"%s"}}`, escapeJSONString(hostPath))
	if err := r.conn.SetDeadline(qmpDeadline(ctx)); err != nil {
		return fmt.Errorf("realQMPClient.Screendump: set deadline: %w", err)
	}
	if _, err := fmt.Fprintln(r.conn, cmd); err != nil {
		return fmt.Errorf("realQMPClient.Screendump: send: %w", err)
	}
	env, err := r.readCommandResponse()
	if err != nil {
		return fmt.Errorf("realQMPClient.Screendump: read response: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("realQMPClient.Screendump: qemu rejected: %s", string(env.Error))
	}
	return nil
}

// escapeJSONString escapes a string for safe inclusion inside a JSON
// string literal (the caller's command template supplies the
// surrounding quotes).
//
// VM2-6: the prior implementation escaped via a
// `for k, v := range map[string]string{...}` two-pass ReplaceAll. Go
// DELIBERATELY randomizes map-iteration order, so on the calls where the
// `"`→`\"` pass happened to run BEFORE the `\`→`\\` pass, the backslash
// that escaping a quote introduced was itself double-escaped — producing
// INVALID JSON (the `"` left unescaped, terminating the QMP `screendump`
// filename string early and letting whatever followed it be parsed as
// further QMP arguments) on a non-deterministic ~13% of calls for any
// filename containing a `"`. It also never escaped control characters
// (newline, tab, ...) at all, so a filename carrying one produced
// invalid JSON unconditionally. encoding/json (already imported for
// qmpEnvelope) escapes deterministically AND completely; we strip its
// surrounding quotes because the command template supplies its own.
func escapeJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
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
