package remote

// Wave-20 REMOTE2-HARD permanent regression guards (§11.4.115 GREEN
// polarity). Each guard below asserts the FIXED behavior of a fresh
// read-only audit finding in pkg/remote. The paired RED reproduction
// (surgical revert of the fix, capturing the real `--- FAIL` line, then
// restore) is recorded in the guard's own doc comment rather than
// committed as a live test — a deliberately-hanging or deliberately-
// broken test must never live in the permanent suite (mirrors the
// pkg/vm wave20_vm2hard_test.go convention).
//
//   - REMOTE-HIGH-1 (bootstrap.go sshDial — ssh.NewClientConn's post-
//     connect SSH handshake was never bounded by a conn deadline or ctx
//     cancellation, the VM2-1 sibling for the SSH path): guarded by
//     TestSSHDial_BoundedByDeadlineWhenPeerNeverSpeaks. RED reproduction:
//     with the `conn.SetDeadline(sshDialDeadline(ctx, config.Timeout))`
//     call removed from sshDial, the guard's own 5s watchdog fires and
//     the test FAILs with "REMOTE-HIGH-1 REGRESSION: sshDial hung past
//     the watchdog window against a peer that never speaks — the post-
//     connect SSH handshake is not bounded by a conn deadline" (observed
//     `--- FAIL: TestSSHDial_BoundedByDeadlineWhenPeerNeverSpeaks` at
//     ~5.00s during the authoring of this fix). Restored immediately
//     after capturing that line.
//   - REMOTE-HIGH-2 (ssh_executor.go CopyDir basename-mismatch branch —
//     the pre-clean `rm -rf` ran via a raw e.sshArgs(...) call that
//     acquires a pooled ControlMaster ref but is never Released): guarded
//     by TestCopyDir_BasenameMismatch_PreCleanReleasesPoolRef. RED
//     reproduction: with the pre-clean reverted to the raw
//     e.sshArgs(ctx, host) + exec.CommandContext(ctx, "ssh", ...) call
//     (bypassing e.Execute's defer Release), the guard FAILs with
//     "REMOTE-HIGH-2: pool ref leaked — refs=1 after CopyDir(basename-
//     mismatch) returned, want 0" (observed `--- FAIL:
//     TestCopyDir_BasenameMismatch_PreCleanReleasesPoolRef`). Restored
//     immediately after capturing that line.
//   - REMOTE-HIGH-3 (connection_pool.go masterArgs — never emitted
//     `-o ControlPersist=<seconds>` despite Options.ControlPersist being
//     a real, documented field): guarded by
//     TestMasterArgs_EmitsControlPersistWhenPositive +
//     TestMasterArgs_OmitsControlPersistWhenZero. RED reproduction: with
//     the `if p.opts.ControlPersist > 0 { ... }` block removed from
//     masterArgs, the first guard FAILs with "REMOTE-HIGH-3: masterArgs
//     must include -o ControlPersist=<seconds> when ControlPersist>0"
//     (observed `--- FAIL:
//     TestMasterArgs_EmitsControlPersistWhenPositive`). Restored
//     immediately after capturing that line.
//   - REMOTE-MED-1 (ssh_executor.go ExecuteStream/CopyFile/CopyDir used
//     the caller ctx verbatim with no self-bound CommandTimeout, unlike
//     Execute — the ORCH-3 sibling): guarded by
//     TestExecuteStream_CopyFile_CopyDir_BoundedByCommandTimeout. RED
//     reproduction: with each method's `if e.opts.CommandTimeout > 0 {
//     ctx, cancel = context.WithTimeout(...) }` guard removed (one at a
//     time), the corresponding subtest FAILs with "REMOTE-MED-1: <Method>
//     took 5.0xxs with context.Background() ... want bounded by
//     CommandTimeout=300ms" (observed `--- FAIL:
//     TestExecuteStream_CopyFile_CopyDir_BoundedByCommandTimeout/<Method>`
//     for each of ExecuteStream, CopyFile, CopyDir in turn). Restored
//     immediately after capturing each line.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"digital.vasic.containers/pkg/logging"
)

// installFakeSCP writes an executable `scp` into a fresh temp dir and
// prepends it to PATH for the test, mirroring installFakeSSH
// (wave15_audit_test.go) so SSHExecutor's `exec.CommandContext(ctx,
// "scp", ...)` invocations run the fake instead of the real client.
func installFakeSCP(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scp"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake scp: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// -----------------------------------------------------------------------------
// REMOTE-HIGH-1 — sshDial bounded by a deadline against a peer that never speaks
// -----------------------------------------------------------------------------

// TestSSHDial_BoundedByDeadlineWhenPeerNeverSpeaks is the permanent
// REMOTE-HIGH-1 guard. The in-process "server" accepts the TCP connect
// (so net.Dialer.DialContext itself succeeds) and then never writes a
// single byte — simulating a peer that accepts the TCP connection but
// stalls before speaking the SSH identification banner. Pre-fix,
// sshDial's ssh.NewClientConn call on that connection blocks forever
// (it consults neither ctx nor config.Timeout); post-fix, the ctx-
// derived conn deadline (sshDialDeadline) bounds it.
//
// The 5s "watchdog" select branch is the anti-hang safety net for THIS
// test itself: a regression that drops the deadline must not wedge the
// whole `go test` invocation, it must surface as an honest FAIL.
func TestSSHDial_BoundedByDeadlineWhenPeerNeverSpeaks(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- c
		// Deliberately never write anything — the peer stalls silently.
	}()

	// ctx carries a short deadline so the fix's sshDialDeadline(ctx, ...)
	// path is exercised (rather than the config.Timeout/fallback path),
	// so the guard runs fast in CI while still exercising the real code
	// path production uses when the caller's own ctx has a deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	config := &ssh.ClientConfig{
		User:            "test",
		Auth:            []ssh.AuthMethod{ssh.Password("x")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second, // large so the ctx deadline is what's exercised
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, dialErr := sshDial(ctx, "tcp", listener.Addr().String(), config)
		done <- dialErr
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("sshDial against a peer that never speaks: want a bounded error, got nil")
		}
		if elapsed > 3*time.Second {
			t.Fatalf("sshDial returned only after %s — expected bounded by the ~200ms ctx deadline, not a multi-second stall", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("REMOTE-HIGH-1 REGRESSION: sshDial hung past the watchdog window against a peer that never speaks — the post-connect SSH handshake is not bounded by a conn deadline")
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatalf("server-side accept never completed")
	}
}

// -----------------------------------------------------------------------------
// REMOTE-HIGH-2 — CopyDir basename-mismatch pre-clean releases its pool ref
// -----------------------------------------------------------------------------

// TestCopyDir_BasenameMismatch_PreCleanReleasesPoolRef is the permanent
// REMOTE-HIGH-2 guard. With ControlMaster pooling enabled, CopyDir's
// basename-mismatch branch runs a pre-clean `rm -rf` on the remote
// destination before scp'ing. Pre-fix, that pre-clean acquired a pooled
// ControlMaster ref (via the raw e.sshArgs call) but never released it;
// post-fix, it runs through e.Execute, whose own `defer
// e.pool.Release(host)` releases the ref on every return path.
func TestCopyDir_BasenameMismatch_PreCleanReleasesPoolRef(t *testing.T) {
	installFakeSSH(t, "#!/bin/sh\n"+
		"prev=\n"+
		"for a in \"$@\"; do\n"+
		"  if [ \"$prev\" = \"-S\" ]; then : > \"$a\"; fi\n"+
		"  prev=\"$a\"\n"+
		"done\n"+
		"exit 0\n")
	installFakeSCP(t, "#!/bin/sh\nexit 0\n")

	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(true),
		WithControlSocketDir(t.TempDir()),
		WithConnectTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
	localDir := t.TempDir()
	remoteDir := "/remote/differentname" // deliberately differs from filepath.Base(localDir)

	if filepath.Base(localDir) == filepath.Base(remoteDir) {
		t.Fatalf("test setup bug: local/remote basenames must differ to hit the basename-mismatch branch")
	}

	err = exec.CopyDir(context.Background(), host, localDir, remoteDir)
	require.NoError(t, err)

	key := hostKey(host)
	exec.pool.mu.Lock()
	entry, ok := exec.pool.active[key]
	refs := -1
	if ok {
		refs = entry.refs
	}
	exec.pool.mu.Unlock()

	if !ok {
		t.Fatalf("expected a pooled ControlMaster entry for %s after CopyDir (basename mismatch triggers the pre-clean, which acquires the pool)", key)
	}
	if refs != 0 {
		t.Fatalf("REMOTE-HIGH-2: pool ref leaked — refs=%d after CopyDir(basename-mismatch) returned, want 0 (the pre-clean rm must route through e.Execute so its Acquire is Released)", refs)
	}
}

// -----------------------------------------------------------------------------
// REMOTE-HIGH-3 — masterArgs emits -o ControlPersist=<seconds>
// -----------------------------------------------------------------------------

// TestMasterArgs_EmitsControlPersistWhenPositive is the permanent
// REMOTE-HIGH-3 guard (positive direction). Without -o
// ControlPersist=<seconds>, the -fNM -N master persists until an
// explicit `-O exit` and never self-expires, contradicting the
// documented Gotcha #1 semantics and, combined with any ref leak,
// producing permanent orphan ssh processes.
func TestMasterArgs_EmitsControlPersistWhenPositive(t *testing.T) {
	pool := &ConnectionPool{opts: Options{
		StrictHostKeyCheck: false,
		ConnectTimeout:     10 * time.Second,
		KeepAlive:          30 * time.Second,
		ControlPersist:     5 * time.Minute,
	}}
	host := RemoteHost{Name: "h", Address: "10.0.0.1", User: "u", Port: 22}

	args := pool.masterArgs(host, "/tmp/sock-controlpersist-pos")

	if !containsSequence(args, "-o", "ControlPersist=300") {
		t.Fatalf("REMOTE-HIGH-3: masterArgs must include -o ControlPersist=<seconds> when ControlPersist>0 (5min=300s); got %v", args)
	}
}

// TestMasterArgs_OmitsControlPersistWhenZero is the permanent
// REMOTE-HIGH-3 guard (negative direction / regression check for the
// fix's own guard): ControlPersist==0 must NOT emit the flag, matching
// the "0=disable" convention used elsewhere in this package
// (KeepAlive==0, CommandTimeout==0, the ConnectTimeout==0 guard in
// Acquire).
func TestMasterArgs_OmitsControlPersistWhenZero(t *testing.T) {
	pool := &ConnectionPool{opts: Options{
		ConnectTimeout: 10 * time.Second,
		KeepAlive:      30 * time.Second,
		ControlPersist: 0,
	}}
	host := RemoteHost{Name: "h", Address: "10.0.0.1", User: "u", Port: 22}

	args := pool.masterArgs(host, "/tmp/sock-controlpersist-zero")

	for i, a := range args {
		if strings.Contains(a, "ControlPersist") {
			t.Fatalf("masterArgs must NOT include ControlPersist when opts.ControlPersist==0; got %v at index %d", args, i)
		}
	}
}

// -----------------------------------------------------------------------------
// REMOTE-MED-1 — ExecuteStream/CopyFile/CopyDir bounded by CommandTimeout
// -----------------------------------------------------------------------------

// TestExecuteStream_CopyFile_CopyDir_BoundedByCommandTimeout is the
// permanent REMOTE-MED-1 guard. Execute wraps ctx with
// e.opts.CommandTimeout before dialing, but ExecuteStream, CopyFile, and
// CopyDir used to take the caller's ctx verbatim with no self-bound
// timeout — a caller passing bare context.Background() got an unbounded
// scp/ssh call (a wedged remote shell / stalled transfer would hang
// forever). Each fake ssh/scp sleeps far longer (5s) than the
// configured CommandTimeout (300ms); post-fix each method must return
// within a small bound well under the sleep duration.
func TestExecuteStream_CopyFile_CopyDir_BoundedByCommandTimeout(t *testing.T) {
	// "exec sleep 5" replaces the shell interpreter's own process image
	// with `sleep` (same PID, no forked grandchild) — critical so that
	// killing the immediate child process (what os/exec's ctx-cancellation
	// does) actually terminates the sleeping process and closes its
	// inherited stdout/stderr pipe fds. A plain "sleep 5\nexit 0" forks
	// `sleep` as a SEPARATE grandchild that inherits those pipe fds; SIGKILL
	// to the parent `sh` would leave the orphaned `sleep` holding the pipes
	// open for the full 5s regardless of ctx, giving a false "still hangs"
	// reading on cmd.Wait() (which blocks on pipe-copy goroutines, not just
	// process-reap) — a fixture artifact, not a real ssh/scp behavior (the
	// real client has no such grandchild).
	const sleepBody = "#!/bin/sh\nexec sleep 5\n"
	const cmdTimeout = 300 * time.Millisecond
	const bound = 3 * time.Second // well under the 5s sleep, well over cmdTimeout

	t.Run("ExecuteStream", func(t *testing.T) {
		installFakeSSH(t, sleepBody)
		exec, err := NewSSHExecutor(
			logging.NopLogger{},
			WithControlMaster(false),
			WithCommandTimeout(cmdTimeout),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = exec.Close() })

		host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
		start := time.Now()
		reader, err := exec.ExecuteStream(context.Background(), host, "echo hi")
		if err == nil {
			buf := make([]byte, 16)
			_, _ = reader.Read(buf)
			_ = reader.Close()
		}
		elapsed := time.Since(start)
		if elapsed > bound {
			t.Fatalf("REMOTE-MED-1: ExecuteStream took %s with context.Background() (no caller deadline) against a peer sleeping 5s — want bounded by CommandTimeout=%s, not the full sleep", elapsed, cmdTimeout)
		}
	})

	t.Run("CopyFile", func(t *testing.T) {
		installFakeSCP(t, sleepBody)
		exec, err := NewSSHExecutor(
			logging.NopLogger{},
			WithControlMaster(false),
			WithCommandTimeout(cmdTimeout),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = exec.Close() })

		host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
		localFile := filepath.Join(t.TempDir(), "f.txt")
		require.NoError(t, os.WriteFile(localFile, []byte("x"), 0o644))

		start := time.Now()
		_ = exec.CopyFile(context.Background(), host, localFile, "/remote/f.txt")
		elapsed := time.Since(start)
		if elapsed > bound {
			t.Fatalf("REMOTE-MED-1: CopyFile took %s with context.Background() (no caller deadline) against a peer sleeping 5s — want bounded by CommandTimeout=%s, not the full sleep", elapsed, cmdTimeout)
		}
	})

	t.Run("CopyDir", func(t *testing.T) {
		installFakeSCP(t, sleepBody)
		exec, err := NewSSHExecutor(
			logging.NopLogger{},
			WithControlMaster(false),
			WithCommandTimeout(cmdTimeout),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = exec.Close() })

		host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
		localDir := t.TempDir()
		// Matching basenames hit the fast (non-pre-clean) scp path, so
		// this subtest isolates the CommandTimeout guard for the final
		// scp call without also requiring a fake ssh binary.
		remoteDir := "/remote/" + filepath.Base(localDir)

		start := time.Now()
		_ = exec.CopyDir(context.Background(), host, localDir, remoteDir)
		elapsed := time.Since(start)
		if elapsed > bound {
			t.Fatalf("REMOTE-MED-1: CopyDir took %s with context.Background() (no caller deadline) against a peer sleeping 5s — want bounded by CommandTimeout=%s, not the full sleep", elapsed, cmdTimeout)
		}
	})
}
