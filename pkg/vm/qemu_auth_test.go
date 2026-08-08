package vm

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// authGuardingSSHClient mirrors realSSHClient's contract: Upload/Run/
// Download fail with "not authenticated" until Authenticate() has run
// for a port (realSSHClient guards on r.client == nil). Unlike the
// permissive fakeSSHClient, it proves QEMUVM actually performs the auth
// step before any guest interaction — the pre-fix production defect
// where QEMUVM never called Authenticate anywhere, so every real
// Upload/Run/Download failed one step past WaitForReady.
type authGuardingSSHClient struct {
	authedPort int
	authCalls  int
	closeCalls int
}

func (c *authGuardingSSHClient) WaitForListener(context.Context, int, time.Duration) error {
	return nil
}
func (c *authGuardingSSHClient) Authenticate(_ context.Context, port int, _ time.Duration) error {
	c.authCalls++
	c.authedPort = port
	return nil
}
func (c *authGuardingSSHClient) Upload(context.Context, string, string) error {
	if c.authedPort == 0 {
		return fmt.Errorf("not authenticated; call Authenticate first")
	}
	return nil
}
func (c *authGuardingSSHClient) Run(context.Context, string, map[string]string, time.Duration) (string, string, int, error) {
	if c.authedPort == 0 {
		return "", "", -1, fmt.Errorf("not authenticated; call Authenticate first")
	}
	return "ok", "", 0, nil
}
func (c *authGuardingSSHClient) Download(context.Context, string, string) error {
	if c.authedPort == 0 {
		return fmt.Errorf("not authenticated; call Authenticate first")
	}
	return nil
}
func (c *authGuardingSSHClient) Close() error { c.closeCalls++; return nil }

// TestQEMUVM_AuthenticatesBeforeGuestInteraction proves QEMUVM runs the
// SSH handshake before Run/Upload/Download, and does so exactly once per
// port (re-authenticating per op would re-dial and leak the prior
// *ssh.Client). Pre-fix, QEMUVM never called Authenticate, so Run fails
// with "not authenticated" even against a ready guest.
func TestQEMUVM_AuthenticatesBeforeGuestInteraction(t *testing.T) {
	ssh := &authGuardingSSHClient{}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, nil, true)
	const port = 10022

	if err := v.WaitForReady(context.Background(), port, time.Second); err != nil {
		t.Fatalf("WaitForReady against a ready listener: %v", err)
	}

	stdout, _, exit, err := v.Run(context.Background(), port, "echo ok", nil, time.Second)
	if err != nil {
		t.Fatalf("QEMUVM.Run against a ready guest failed: %v "+
			"(QEMUVM never authenticated before invoking the guest)", err)
	}
	if exit != 0 || stdout != "ok" {
		t.Fatalf("Run: stdout=%q exit=%d", stdout, exit)
	}
	if ssh.authCalls == 0 {
		t.Fatalf("QEMUVM.Run must Authenticate before invoking the guest; authCalls=0")
	}

	if err := v.Upload(context.Background(), port, "/tmp/h", "/tmp/v"); err != nil {
		t.Fatalf("Upload after auth: %v", err)
	}
	if err := v.Download(context.Background(), port, "/tmp/v", "/tmp/h"); err != nil {
		t.Fatalf("Download after auth: %v", err)
	}
	// Exactly ONE handshake for one port across Run+Upload+Download —
	// guards against the per-op re-auth that would leak connections.
	if ssh.authCalls != 1 {
		t.Fatalf("expected exactly 1 Authenticate for one port across "+
			"Run+Upload+Download, got %d (per-op re-auth leaks connections)", ssh.authCalls)
	}
}

// TestQEMUVM_Teardown_ClosesSSHClient proves Teardown releases the SSH
// client handle. Pre-fix, Teardown closed the QMP client but never
// v.ssh.Close(), so every VM lifecycle leaked its SSH connection +
// x/crypto/ssh mux/keepalive goroutines (unbounded across a serial
// matrix run reusing one QEMUVM).
func TestQEMUVM_Teardown_ClosesSSHClient(t *testing.T) {
	prev := killByPortHook
	killByPortHook = func(context.Context, int) (KillReport, error) {
		return KillReport{Matched: 1, Sigtermed: []int{1}}, nil
	}
	defer func() { killByPortHook = prev }()
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 50 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	ssh := &fakeSSHClient{}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, &fakeQMPClient{}, true)

	if err := v.Teardown(context.Background(), 14444, 10022); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if ssh.closedSessions == 0 {
		t.Fatalf("Teardown never called v.ssh.Close() — every VM lifecycle " +
			"leaks its SSH connection")
	}
}
