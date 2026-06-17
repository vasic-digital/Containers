//go:build integration

package remote

import (
	"context"
	"os"
	"testing"

	"digital.vasic.containers/pkg/logging"
)

// TestSSHExecutor_Execute_ReleasesPoolRefOnCommandError is the regression guard
// for the connection-pool ref-count leak: Execute acquired a ControlMaster
// connection (refs++) in sshArgs but Released it only on the success path, so a
// command that exited non-zero leaked a ref and the socket was never
// ControlPersist-reaped. The fix defers Release on every return path.
//
// White-box (package remote): asserts the pooled entry's refs returns to 0
// after a failing Execute followed by a successful one. RED on the pre-fix
// code (refs == 1), GREEN after (refs == 0).
//
// Requires a reachable SSH host with ControlMaster support:
//
//	TMX_TEST_SSH_ADDR=nezha.local TMX_TEST_SSH_USER=milosvasic \
//	TMX_TEST_SSH_KEY=/Users/<you>/.ssh/id_ed25519 \
//	go test -tags=integration -run ReleasesPoolRefOnCommandError ./pkg/remote/
func TestSSHExecutor_Execute_ReleasesPoolRefOnCommandError(t *testing.T) {
	addr := os.Getenv("TMX_TEST_SSH_ADDR")
	user := os.Getenv("TMX_TEST_SSH_USER")
	key := os.Getenv("TMX_TEST_SSH_KEY")
	if addr == "" || user == "" {
		t.Skip("SKIP-OK: set TMX_TEST_SSH_ADDR + TMX_TEST_SSH_USER (+ TMX_TEST_SSH_KEY) to run")
	}

	exec, err := NewSSHExecutor(logging.NewStdLogger("t"))
	if err != nil {
		t.Fatalf("NewSSHExecutor: %v", err)
	}
	defer func() { _ = exec.Close() }()
	if exec.pool == nil {
		t.Skip("SKIP-OK: ControlMaster pool not enabled in this build/config")
	}

	host := RemoteHost{Name: "t", Address: addr, Port: 22, User: user, KeyPath: key, Runtime: "podman"}
	ctx := context.Background()

	// 1) command that EXITS NON-ZERO — the pre-fix leak path.
	if _, e := exec.Execute(ctx, host, "exit 7"); e == nil {
		t.Fatalf("expected non-nil error for `exit 7`")
	}
	// 2) a successful command (re-acquires the same pooled entry).
	if _, e := exec.Execute(ctx, host, "true"); e != nil {
		t.Fatalf("unexpected error for `true`: %v", e)
	}

	exec.pool.mu.Lock()
	entry, ok := exec.pool.active[hostKey(host)]
	refs := -1
	if ok {
		refs = entry.refs
	}
	exec.pool.mu.Unlock()

	if !ok {
		t.Skip("SKIP-OK: ControlMaster fell back to direct (no pooled entry) — cannot assert refs")
	}
	if refs != 0 {
		t.Fatalf("connection-pool refs leaked: got %d, want 0 — Execute did not Release on the command-error path", refs)
	}
}
