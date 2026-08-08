package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/logging"
)

// installFakeSSH writes an executable `ssh` into a fresh temp dir and
// prepends it to PATH for the test, so ConnectionPool's `iexec.Run(ctx,
// "ssh", ...)` invokes the fake instead of the real client. The child
// inherits the test process env (internal/exec leaves cmd.Env nil), so
// the fake can read WAVE15_SIG.
func installFakeSSH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestConnectionPool_Acquire_DifferentHostsDialConcurrently proves the
// pool mutex is NOT held across the blocking ssh dial: two Acquire calls
// for DIFFERENT hosts must dial at the same time. Pre-fix, Acquire held
// p.mu across iexec.Run, so the second host could not even start its
// dial until the first returned — only one dial was ever in flight
// (MANDATORY DEVELOPMENT PRINCIPLE #2: no blocking under a shared lock).
func TestConnectionPool_Acquire_DifferentHostsDialConcurrently(t *testing.T) {
	sig := t.TempDir()
	t.Setenv("WAVE15_SIG", sig)
	installFakeSSH(t, "#!/bin/sh\n"+
		"printf 'x\\n' >> \"$WAVE15_SIG/started\"\n"+
		"while [ ! -f \"$WAVE15_SIG/release\" ]; do sleep 0.02; done\n"+
		"exit 0\n")

	opts := DefaultOptions()
	opts.ControlSocketDir = t.TempDir()
	opts.ConnectTimeout = 30 * time.Second
	pool, err := NewConnectionPool(opts)
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	defer pool.Close()

	hostA := RemoteHost{Name: "a", Address: "host-a", User: "u"}
	hostB := RemoteHost{Name: "b", Address: "host-b", User: "u"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = pool.Acquire(context.Background(), hostA) }()
	go func() { defer wg.Done(); _, _ = pool.Acquire(context.Background(), hostB) }()

	startedFile := filepath.Join(sig, "started")
	deadline := time.Now().Add(5 * time.Second)
	concurrentStarted := 0
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(startedFile)
		concurrentStarted = strings.Count(string(b), "\n")
		if concurrentStarted >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.WriteFile(filepath.Join(sig, "release"), []byte("x"), 0o644)
	wg.Wait()

	if concurrentStarted < 2 {
		t.Fatalf("only %d of 2 different-host dials ran concurrently; the pool "+
			"mutex is held across the blocking ssh dial, serializing every "+
			"host behind one slow/hung dial (MANDATORY principle #2)", concurrentStarted)
	}
}

// TestConnectionPool_Acquire_EvictsStaleSocket proves the fast path does
// not hand back a cached entry whose ControlMaster socket is gone (dead
// master: remote reboot / partition / killed). Pre-fix, Acquire returned
// the stale socketPath with a nil error and never re-validated liveness
// (Gotcha #1), so callers trusted an unusable pooled connection.
func TestConnectionPool_Acquire_EvictsStaleSocket(t *testing.T) {
	installFakeSSH(t, "#!/bin/sh\n"+
		"prev=\n"+
		"for a in \"$@\"; do\n"+
		"  if [ \"$prev\" = \"-S\" ]; then : > \"$a\"; fi\n"+
		"  prev=\"$a\"\n"+
		"done\n"+
		"exit 0\n")

	opts := DefaultOptions()
	opts.ControlSocketDir = t.TempDir()
	pool, err := NewConnectionPool(opts)
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	defer pool.Close()

	host := RemoteHost{Name: "stale", Address: "10.0.0.9", User: "u"}
	key := hostKey(host)
	deadSocket := filepath.Join(opts.ControlSocketDir, "ctrl-dead-gone")

	pool.mu.Lock()
	pool.active[key] = &controlEntry{host: host, socketPath: deadSocket, refs: 0, createdAt: time.Now()}
	pool.mu.Unlock()

	got, err := pool.Acquire(context.Background(), host)
	if err != nil {
		t.Fatalf("Acquire after eviction should redial (fake ssh) and succeed: %v", err)
	}
	if got == deadSocket {
		t.Fatalf("Acquire returned the dead ControlMaster socket %q without a liveness check", got)
	}
	if _, statErr := os.Stat(got); statErr != nil {
		t.Fatalf("Acquire returned socket %q that does not exist: %v", got, statErr)
	}
}

// TestRemoteComposeOrchestrator_FallbackGuessNeverRetried proves a
// low-confidence fallback guess (made while the host was transiently
// unready) is NOT frozen for the orchestrator's life. Pre-fix, a
// sync.Once froze the first DetectWithFallback result, so a host that
// recovered kept being driven with the wrong binary forever.
func TestRemoteComposeOrchestrator_FallbackGuessNeverRetried(t *testing.T) {
	exec := &mockExecutor{}
	host := RemoteHost{Name: "test-host", Address: "192.168.1.100", User: "deploy", Runtime: "docker"}

	healthy := false
	exec.executeFunc = func(_ context.Context, _ RemoteHost, cmd string) (*CommandResult, error) {
		if !healthy {
			return &CommandResult{ExitCode: 1, Stderr: "not found"}, nil
		}
		if strings.HasPrefix(cmd, "podman-compose ") {
			return &CommandResult{ExitCode: 0, Stdout: "1.0.6"}, nil
		}
		return &CommandResult{ExitCode: 1, Stderr: "not found"}, nil
	}

	orch := NewRemoteComposeOrchestrator(host, exec, logging.NopLogger{})

	cmd1, err := orch.ComposeCommand(context.Background())
	if err != nil {
		t.Fatalf("first ComposeCommand: %v", err)
	}
	if cmd1.Binary != "docker" {
		t.Fatalf("first call (host unready) should fall back to configured runtime "+
			"'docker' guess, got %q", cmd1.Binary)
	}

	healthy = true // host recovered; the real compose tool is now detectable

	cmd2, err := orch.ComposeCommand(context.Background())
	if err != nil {
		t.Fatalf("second ComposeCommand: %v", err)
	}
	if cmd2.Binary != "podman-compose" {
		t.Fatalf("after the host recovered, a later call must re-attempt detection "+
			"and use the real tool (podman-compose), but got the frozen first-call "+
			"guess %q", cmd2.Binary)
	}
}
