package remote

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCTHardenRemote1_StreamReaderCloseIdempotent is the §11.4.115 guard for
// CT-HARDEN-REMOTE-1: streamReader.Close() was not idempotent, so a second
// Close() (belt-and-suspenders `defer rc.Close()` + explicit Close) double-
// decremented the pool ref-count (underflow to -1, corrupting the leak-detection
// oracle) and called cmd.Wait() twice. Fixed with sync.Once. White-box: builds
// the exact struct ExecuteStream returns, seeds one pooled ref, calls Close twice.
//
//	default (RED_MODE unset / "1"): GUARD — assert Close is idempotent (refs == 0).
//	RED_MODE=0: forensic reproduce — assert the pre-fix underflow (refs == -1).
func TestCTHardenRemote1_StreamReaderCloseIdempotent(t *testing.T) {
	guard := os.Getenv("RED_MODE") != "0" // default guard; RED_MODE=0 = reproduce

	dir := t.TempDir()
	pool, err := NewConnectionPool(Options{ConnectTimeout: time.Second, ControlSocketDir: dir})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	host := RemoteHost{Name: "h1", Address: "203.0.113.9", Port: 22, User: "deploy"}
	key := hostKey(host)

	// Exactly the state ExecuteStream leaves after ONE successful Acquire.
	pool.mu.Lock()
	pool.active[key] = &controlEntry{host: host, socketPath: dir + "/ctrl-seed", refs: 1, createdAt: time.Now()}
	pool.mu.Unlock()

	cmd := exec.Command("true")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sr := &streamReader{cmd: cmd, reader: stdout, pool: pool, host: host}

	_ = sr.Close()
	_ = sr.Close() // idempotency expectation

	pool.mu.Lock()
	got := pool.active[key].refs
	pool.mu.Unlock()

	if guard {
		if got != 0 {
			t.Fatalf("refs underflow: double Close() left refs == %d, want 0 (Close must be idempotent)", got)
		}
	} else {
		if got != -1 {
			t.Fatalf("RED_MODE=0 reproduce: expected the double-Close underflow (refs == -1), got refs == %d", got)
		}
	}
}

// TestCTHardenRemote2_ConnectTimeoutZeroNotPreCancelled is the §11.4.115 guard
// for CT-HARDEN-REMOTE-2: Acquire wrapped the dial in context.WithTimeout(ctx,
// ConnectTimeout); with ConnectTimeout==0 that yields an already-expired context
// so every dial failed instantly with "context deadline exceeded" and the pool
// was permanently unusable. Fixed by only imposing the deadline when
// ConnectTimeout > 0. Oracle: dial a guaranteed-refusing loopback port.
//
//	default (RED_MODE unset / "1"): GUARD — assert a real dial was attempted (not pre-cancelled).
//	RED_MODE=0: forensic reproduce — assert the dial is pre-cancelled (ssh never ran).
func TestCTHardenRemote2_ConnectTimeoutZeroNotPreCancelled(t *testing.T) {
	guard := os.Getenv("RED_MODE") != "0" // default guard; RED_MODE=0 = reproduce

	dir := t.TempDir()
	pool, err := NewConnectionPool(Options{ConnectTimeout: 0, ControlSocketDir: dir})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	host := RemoteHost{Name: "z", Address: "127.0.0.1", Port: 1, User: "nobody"} // refuses instantly

	_, aerr := pool.Acquire(context.Background(), host)
	if aerr == nil {
		t.Fatalf("expected Acquire against 127.0.0.1:1 to fail")
	}
	msg := aerr.Error()
	preCancelled := strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "context canceled")

	if guard {
		if preCancelled {
			t.Fatalf("ConnectTimeout=0 STILL pre-cancels the dial: %v", aerr)
		}
	} else {
		if !preCancelled {
			t.Fatalf("RED_MODE=0 reproduce: expected a pre-cancelled-context error with ConnectTimeout=0, got: %v", aerr)
		}
	}
}
