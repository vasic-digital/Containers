package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// -----------------------------------------------------------------------------
// SW1-1 — realSSHClient.{Upload,Download} MUST honour ctx cancellation
// -----------------------------------------------------------------------------
//
// Anti-bluff posture (§11.4.115): these tests drive the REAL Upload/Download
// code paths against a REAL loopback SSH server (the clients_test.go harness)
// whose session handler deliberately WEDGES the SCP exchange — it accepts the
// `scp -t` / `scp -f` exec, then never completes the protocol (never sends an
// exit-status, never sends the download header). On the pre-fix source
// (synchronous body, no select on ctx.Done()) both calls block FOREVER, so
// each test's outer 8s guard fires → RED. With the SW1-1 fix each call returns
// shortly after its 200ms ctx deadline with an error containing "timeout" →
// GREEN. The bug-catcher IS the regression guard.
//
// The wedge is driven purely in-process (no real network, no real guest), so
// the tests are hermetic and re-runnable end-to-end (§11.4.98).

const (
	sw1CtxDeadline   = 200 * time.Millisecond // ctx timeout handed to Upload/Download
	sw1ReturnByGuard = 8 * time.Second        // must return well before this (else it hung)
)

// callReturnedInTime runs fn in a goroutine and asserts it returns before the
// guard fires. On the broken (pre-fix) source fn hangs forever and the guard
// t.Fatalf's — the RED signal. Returns fn's error for the caller to assert on.
func callReturnedInTime(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if elapsed > sw1ReturnByGuard {
			t.Fatalf("%s returned but took %v (> %v) — ctx path too slow", what, elapsed, sw1ReturnByGuard)
		}
		t.Logf("%s returned after %v (ctx deadline was %v)", what, elapsed, sw1CtxDeadline)
		return err
	case <-time.After(sw1ReturnByGuard):
		t.Fatalf("%s did NOT return within %v after a %v ctx deadline — ctx.Done() is not wired; the wedged SCP exchange hung the caller (SW1-1 defect)", what, sw1ReturnByGuard, sw1CtxDeadline)
		return nil // unreachable; t.Fatalf stops the test goroutine
	}
}

// TestRealSSHClient_Upload_HonoursContextDeadline proves Upload's SCP source
// exchange respects ctx cancellation when the guest wedges (accepts scp -t but
// never returns an exit-status, so the client blocks on <-runErr).
func TestRealSSHClient_Upload_HonoursContextDeadline(t *testing.T) {
	dir := t.TempDir()
	hostSrc := filepath.Join(dir, "host-source")
	if err := os.WriteFile(hostSrc, []byte("payload that will never be acknowledged\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok || !strings.HasPrefix(cmd, "scp -t ") {
				_ = ch.Close()
				return
			}
			// WEDGE: drain the client's header + file bytes so its small-file
			// writes succeed, but NEVER send an exit-status — so the client's
			// session.Run never returns and it blocks on <-runErr. io.Copy
			// unblocks with EOF only once the client's ctx-timeout path closes
			// the session (so this goroutine cannot leak past the test).
			_, _ = io.Copy(io.Discard, ch)
		},
	})

	c := connectAuthenticated(t, port, "")
	ctx, cancel := context.WithTimeout(context.Background(), sw1CtxDeadline)
	defer cancel()

	err := callReturnedInTime(t, "Upload", func() error {
		return c.Upload(ctx, hostSrc, "/tmp/dest/host-source")
	})
	if err == nil {
		t.Fatalf("Upload returned nil error on a wedged guest; expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Upload error %q does not contain %q", err.Error(), "timeout")
	}
}

// TestRealSSHClient_Download_HonoursContextDeadline proves Download's SCP sink
// exchange respects ctx cancellation when the guest wedges (accepts scp -f,
// reads the ready NUL, but never sends the header — so the client blocks in
// reader.ReadString).
func TestRealSSHClient_Download_HonoursContextDeadline(t *testing.T) {
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			cmd, ok := readExecCommand(reqs)
			if !ok || !strings.HasPrefix(cmd, "scp -f ") {
				_ = ch.Close()
				return
			}
			// WEDGE: read the client's initial ready NUL (and anything else) but
			// NEVER send the "C<mode> <size> <name>" header — the client blocks
			// in reader.ReadString('\n'). io.Copy unblocks with EOF only once the
			// client's ctx-timeout path closes the session.
			_, _ = io.Copy(io.Discard, ch)
		},
	})

	c := connectAuthenticated(t, port, "")
	dst := filepath.Join(t.TempDir(), "host-dest")
	ctx, cancel := context.WithTimeout(context.Background(), sw1CtxDeadline)
	defer cancel()

	err := callReturnedInTime(t, "Download", func() error {
		return c.Download(ctx, "/guest/path/guest-source", dst)
	})
	if err == nil {
		t.Fatalf("Download returned nil error on a wedged guest; expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Download error %q does not contain %q", err.Error(), "timeout")
	}
}
