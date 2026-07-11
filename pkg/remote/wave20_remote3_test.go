package remote

// Wave-20 REMOTE3-HARD permanent regression guards (§11.4.115 GREEN
// polarity) for the three residual MEDIUM findings from the follow-up
// read-only audit of pkg/remote. Each guard below asserts the FIXED
// behavior; the paired RED reproduction (surgical revert of the fix,
// capturing the real `--- FAIL` line, then restore) is recorded in the
// guard's own doc comment rather than committed as a live test — a
// deliberately-broken test must never live in the permanent suite
// (mirrors the wave20_remote2hard_test.go convention).
//
//   - REMOTE-MED-2 (ssh_executor.go scpArgs never acquired a pooled
//     ControlMaster connection, unlike sshArgs — every scp transfer
//     opened a brand-new TCP+SSH connection even with a live
//     multiplexable master): guarded by
//     TestCopyFile_ReusesControlMasterSocket and
//     TestCopyDir_MatchingBasename_ReusesControlMasterSocket. RED
//     reproduction: with scpArgs reverted to its pre-fix signature
//     (`func (e *SSHExecutor) scpArgs(host RemoteHost) []string`, no
//     ctx, no pool.Acquire, no `-o ControlPath=`) and both call sites
//     reverted to `args := e.scpArgs(host)`, both guards FAIL with
//     "REMOTE-MED-2: ... scp invocation did not carry -o
//     ControlPath=... (a live pooled ControlMaster socket)" (observed
//     `--- FAIL: TestCopyFile_ReusesControlMasterSocket` and `---
//     FAIL: TestCopyDir_MatchingBasename_ReusesControlMasterSocket`).
//     Restored immediately after capturing those lines.
//   - REMOTE-MED-3 (connection_pool.go Release had no floor — a
//     failed-Acquire caller's deferred Release could steal/underflow a
//     live holder's ref count): guarded by
//     TestConnectionPool_Release_NeverGoesBelowZero. RED reproduction:
//     with the `&& entry.refs > 0` guard removed from Release (back to
//     unconditional `entry.refs--`), the guard FAILs with
//     "REMOTE-MED-3: Release decremented refs below zero (refs=-1)"
//     (observed `--- FAIL:
//     TestConnectionPool_Release_NeverGoesBelowZero`). Restored
//     immediately after capturing that line.
//   - REMOTE-MED-4 (connection_pool.go NewConnectionPool trusted a
//     pre-existing ControlSocketDir without checking its permissions
//     or owner — os.MkdirAll only enforces 0700 on a directory it
//     creates): guarded by
//     TestNewConnectionPool_RefusesPreExistingWorldWritableDir and
//     TestNewConnectionPool_AcceptsPreExistingOwnedNonWritableDir. RED
//     reproduction: with the `preExisted` check and
//     verifySocketDirOwnership call removed from NewConnectionPool,
//     the first guard FAILs with "REMOTE-MED-4: NewConnectionPool must
//     refuse a pre-existing control socket dir writable by group/other
//     (mode 0777), got nil error" (observed `--- FAIL:
//     TestNewConnectionPool_RefusesPreExistingWorldWritableDir`).
//     Restored immediately after capturing that line.
//
// REMOTE-MED-4 note on the exact check chosen: the audit's suggested
// fix ("verify Mode().Perm() == 0700 ... refusing otherwise") was
// found, during reproduction, to be over-strict for this codebase —
// testing.T.TempDir() creates its directories via os.Mkdir(dir, 0777)
// (subject to process umask, commonly landing at 0755 in this
// environment), and every existing test in this package that seeds
// ControlSocketDir via t.TempDir() would then have NewConnectionPool
// refuse its own legitimate, current-user-owned, non-writable-by-
// others temp directory — a regression across
// connection_pool_test.go, wave15_audit_test.go,
// wave18_remote_hardening_test.go, and wave20_remote2hard_test.go. The
// actual security boundary the finding is protecting — a hostile
// co-resident user pre-staging or tampering with a predictably-named
// ControlMaster socket — is closed by checking OWNER UID (rejects a
// directory left behind by a different user regardless of its mode)
// and the GROUP/OTHER WRITE bits (rejects a directory any other user
// could write into regardless of who owns it), not by requiring an
// exact mode. This is implemented in verifySocketDirOwnership
// (connection_pool.go) and proven not to regress by
// TestNewConnectionPool_AcceptsPreExistingOwnedNonWritableDir below.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
)

// readArgvFile reads a newline-separated argv dump written by a fake
// scp/ssh script (see installFakeSCP in wave20_remote2hard_test.go)
// back into a []string, one element per line, dropping the trailing
// empty element produced by the final newline.
func readArgvFile(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err, "read captured argv file %s", path)
	lines := splitLines(string(b))
	return lines
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// -----------------------------------------------------------------------------
// REMOTE-MED-2 — scpArgs acquires + emits a pooled ControlMaster socket
// -----------------------------------------------------------------------------

// scpArgvCaptureScript is a fake scp that dumps its argv (one arg per
// line) to $SCP_ARGV_FILE before exiting successfully.
const scpArgvCaptureScript = "#!/bin/sh\n" +
	": > \"$SCP_ARGV_FILE\"\n" +
	"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$SCP_ARGV_FILE\"; done\n" +
	"exit 0\n"

// TestCopyFile_ReusesControlMasterSocket is the permanent REMOTE-MED-2
// guard for CopyFile. With ControlMaster pooling enabled and a live
// master already dialed (via the fake ssh below), CopyFile's
// underlying scp invocation must carry `-o ControlPath=<socket>` —
// the same multiplexed connection sshArgs would use — instead of
// opening a brand-new TCP+SSH connection for every file transfer.
func TestCopyFile_ReusesControlMasterSocket(t *testing.T) {
	installFakeSSH(t, "#!/bin/sh\nexit 0\n")

	argvFile := filepath.Join(t.TempDir(), "scp-argv")
	t.Setenv("SCP_ARGV_FILE", argvFile)
	installFakeSCP(t, scpArgvCaptureScript)

	socketDir := t.TempDir()
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(true),
		WithControlSocketDir(socketDir),
		WithConnectTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
	localFile := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("x"), 0o644))

	require.NoError(t, exec.CopyFile(context.Background(), host, localFile, "/remote/f.txt"))

	argv := readArgvFile(t, argvFile)
	// RM2-2 reconcile: the ControlMaster socket path now includes the SSH
	// user (host.User == "u") so it matches the user@address:port pool key.
	wantSocket := filepath.Join(socketDir, "ctrl-u-127.0.0.1-22")
	if !containsSequence(argv, "-o", "ControlPath="+wantSocket) {
		t.Fatalf("REMOTE-MED-2: CopyFile's scp invocation did not carry "+
			"-o ControlPath=%s (a live pooled ControlMaster socket); "+
			"got argv: %v", wantSocket, argv)
	}

	key := hostKey(host)
	exec.pool.mu.Lock()
	entry, ok := exec.pool.active[key]
	refs := -1
	if ok {
		refs = entry.refs
	}
	exec.pool.mu.Unlock()
	if !ok {
		t.Fatalf("expected a pooled ControlMaster entry for %s after CopyFile", key)
	}
	if refs != 0 {
		t.Fatalf("pool ref leaked after CopyFile: refs=%d, want 0 "+
			"(scpArgs' Acquire must be paired with a Release)", refs)
	}
}

// TestCopyDir_MatchingBasename_ReusesControlMasterSocket is the
// permanent REMOTE-MED-2 guard for CopyDir's common (matching-
// basename, no pre-clean) path — the case the finding calls out as
// "the file-shipping workload where it matters most".
func TestCopyDir_MatchingBasename_ReusesControlMasterSocket(t *testing.T) {
	installFakeSSH(t, "#!/bin/sh\nexit 0\n")

	argvFile := filepath.Join(t.TempDir(), "scp-argv")
	t.Setenv("SCP_ARGV_FILE", argvFile)
	installFakeSCP(t, scpArgvCaptureScript)

	socketDir := t.TempDir()
	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(true),
		WithControlSocketDir(socketDir),
		WithConnectTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	host := RemoteHost{Name: "h", Address: "127.0.0.1", User: "u"}
	localDir := t.TempDir()
	remoteDir := "/remote/" + filepath.Base(localDir) // matching basenames

	require.NoError(t, exec.CopyDir(context.Background(), host, localDir, remoteDir))

	argv := readArgvFile(t, argvFile)
	// RM2-2 reconcile: the ControlMaster socket path now includes the SSH
	// user (host.User == "u") so it matches the user@address:port pool key.
	wantSocket := filepath.Join(socketDir, "ctrl-u-127.0.0.1-22")
	if !containsSequence(argv, "-o", "ControlPath="+wantSocket) {
		t.Fatalf("REMOTE-MED-2: CopyDir's scp invocation did not carry "+
			"-o ControlPath=%s (a live pooled ControlMaster socket); "+
			"got argv: %v", wantSocket, argv)
	}

	key := hostKey(host)
	exec.pool.mu.Lock()
	entry, ok := exec.pool.active[key]
	refs := -1
	if ok {
		refs = entry.refs
	}
	exec.pool.mu.Unlock()
	if !ok {
		t.Fatalf("expected a pooled ControlMaster entry for %s after CopyDir", key)
	}
	if refs != 0 {
		t.Fatalf("pool ref leaked after CopyDir: refs=%d, want 0", refs)
	}
}

// -----------------------------------------------------------------------------
// REMOTE-MED-3 — Release never decrements refs below zero
// -----------------------------------------------------------------------------

// TestConnectionPool_Release_NeverGoesBelowZero is the permanent
// REMOTE-MED-3 guard. It reproduces the invariant violation directly
// (an extra Release beyond the true holder's, modeling a failed-
// Acquire caller's deferred Release racing a live holder that already
// brought refs to 0) without needing a full concurrent-timing
// reproduction, mirroring the direct pool.active manipulation already
// used by TestConnectionPool_Acquire_EvictsStaleSocket
// (wave15_audit_test.go).
func TestConnectionPool_Release_NeverGoesBelowZero(t *testing.T) {
	opts := DefaultOptions()
	opts.ControlSocketDir = t.TempDir()
	pool, err := NewConnectionPool(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })

	host := RemoteHost{Name: "h", Address: "10.0.0.5", User: "u"}
	key := hostKey(host)

	pool.mu.Lock()
	pool.active[key] = &controlEntry{
		host: host, socketPath: "/tmp/does-not-matter", refs: 0, createdAt: time.Now(),
	}
	pool.mu.Unlock()

	// The true holder already released down to 0; a failed-Acquire
	// caller's deferred Release must not steal/underflow it.
	pool.Release(host)
	pool.Release(host)

	pool.mu.Lock()
	refs := pool.active[key].refs
	pool.mu.Unlock()

	if refs < 0 {
		t.Fatalf("REMOTE-MED-3: Release decremented refs below zero (refs=%d) — "+
			"a failed-Acquire caller's deferred Release must never steal/"+
			"underflow a live holder's ref count", refs)
	}
}

// -----------------------------------------------------------------------------
// REMOTE-MED-4 — NewConnectionPool refuses an untrustworthy pre-existing dir
// -----------------------------------------------------------------------------

// TestNewConnectionPool_RefusesPreExistingWorldWritableDir is the
// permanent REMOTE-MED-4 guard (negative direction): a pre-existing
// control socket directory writable by group/other must be refused,
// not silently trusted — its fully-predictable socket filenames
// (ctrl-<address>-<port>) would otherwise let another local user
// pre-stage or tamper with a ControlMaster socket.
func TestNewConnectionPool_RefusesPreExistingWorldWritableDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ctrl")
	require.NoError(t, os.Mkdir(dir, 0o777))
	// os.Mkdir is subject to umask; force the mode explicitly so the
	// test is not umask-dependent.
	require.NoError(t, os.Chmod(dir, 0o777))

	opts := DefaultOptions()
	opts.ControlSocketDir = dir

	pool, err := NewConnectionPool(opts)
	if err == nil {
		_ = pool.Close()
		t.Fatalf("REMOTE-MED-4: NewConnectionPool must refuse a pre-existing " +
			"control socket dir writable by group/other (mode 0777), got nil error")
	}
}

// TestNewConnectionPool_AcceptsPreExistingOwnedNonWritableDir is the
// permanent REMOTE-MED-4 guard (positive / non-regression direction):
// a pre-existing directory owned by the current user and NOT
// writable by group/other must still be trusted, even when its mode
// is not exactly 0700 — the realistic shape produced by
// testing.T.TempDir() (os.Mkdir(dir, 0777) subject to process umask,
// commonly 0755 in this environment; see the file-level doc comment
// for why an exact-0700 check would regress the rest of this
// package's test suite).
func TestNewConnectionPool_AcceptsPreExistingOwnedNonWritableDir(t *testing.T) {
	dir := t.TempDir() // already exists, current-user-owned, before NewConnectionPool runs

	info, err := os.Stat(dir)
	require.NoError(t, err)
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("test precondition violated: t.TempDir() %s is group/other-writable "+
			"(mode %04o) in this environment — cannot exercise the accept path", dir, perm)
	}

	opts := DefaultOptions()
	opts.ControlSocketDir = dir

	pool, err := NewConnectionPool(opts)
	require.NoError(t, err, "a pre-existing, current-user-owned, "+
		"non-group/other-writable directory must be accepted regardless of its exact mode")
	require.NoError(t, pool.Close())
}
