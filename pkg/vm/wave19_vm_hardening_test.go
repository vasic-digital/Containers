package vm

// Wave-19 VM-hardening permanent regression guards (§11.4.115 GREEN
// polarity).
//
//   - CT-HARDEN-VM-1 (cross-target SSH/QMP session contamination under
//     --concurrent>1): guarded by TestQEMUVM_ConcurrentTargets_NoCross-
//     SessionContamination — 8 targets × 25 iters of GENUINELY concurrent
//     v.Run against the shared *QEMUVM. The PRIMARY oracle is the logical
//     assertion that each target only ever observes its OWN authenticated
//     port (contamination = a cross-port observation); -race is a
//     supplementary check on the real QEMUVM/ssh production paths (the
//     fake's own fields are mutex-guarded, so it does not itself trip
//     -race). (The gated deadlock-on-fix RED reproduction was evidence-only
//     and is not committed — a deadlocking test must never live in the
//     suite.)
//   - CT-HARDEN-VM-2 (network shaping ran against an unauthenticated
//     session): guarded by TestVMRunOne_NetworkShaping_Authenticates-
//     BeforeApplying — drives a real *QEMUVM through the full matrix
//     runner against an auth-gated client, asserting Authenticate ran AND
//     the tc-qdisc command genuinely executed (not merely that no error
//     surfaced).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// racingSharedSSHClient records the authenticated-port observed at the
// moment each guest op actually executes on the single shared connection.
// Under the pre-fix code (lock released before the guest op ran) a
// concurrent target's Authenticate could land in between, so target A's
// Run would observe target B's port. Post-fix (guestMu held across the
// whole authenticate-then-execute sequence) each op only ever observes
// its own port. The fake's fields are mutex-guarded, so the primary
// oracle is that observed-port assertion (not a -race report on the
// fake); -race additionally guards the real QEMUVM/ssh code paths.
type racingSharedSSHClient struct {
	resultMu          sync.Mutex
	authedPort        int
	observedPortAtRun map[string]int
}

func (c *racingSharedSSHClient) WaitForListener(context.Context, int, time.Duration) error {
	return nil
}

func (c *racingSharedSSHClient) Authenticate(_ context.Context, port int, _ time.Duration) error {
	c.resultMu.Lock()
	c.authedPort = port
	c.resultMu.Unlock()
	return nil
}

func (c *racingSharedSSHClient) Upload(context.Context, string, string) error { return nil }

func (c *racingSharedSSHClient) Run(_ context.Context, script string, _ map[string]string, _ time.Duration) (string, string, int, error) {
	c.resultMu.Lock()
	c.observedPortAtRun[script] = c.authedPort
	c.resultMu.Unlock()
	return "", "", 0, nil
}

func (c *racingSharedSSHClient) Download(context.Context, string, string) error { return nil }
func (c *racingSharedSSHClient) Close() error                                   { return nil }

// authRecordingSSHClient is auth-gated exactly like realSSHClient
// (Run/Upload/Download fail with "not authenticated" until Authenticate
// has run) AND additionally records every executed script so a test can
// assert the tc-qdisc command genuinely ran, not merely that no error
// surfaced.
type authRecordingSSHClient struct {
	mu         sync.Mutex
	authedPort int
	authCalls  int
	runScripts []string
	closeCalls int
}

func (c *authRecordingSSHClient) WaitForListener(context.Context, int, time.Duration) error {
	return nil
}
func (c *authRecordingSSHClient) Authenticate(_ context.Context, port int, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authCalls++
	c.authedPort = port
	return nil
}
func (c *authRecordingSSHClient) Upload(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authedPort == 0 {
		return fmt.Errorf("not authenticated; call Authenticate first")
	}
	return nil
}
func (c *authRecordingSSHClient) Run(_ context.Context, script string, _ map[string]string, _ time.Duration) (string, string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authedPort == 0 {
		return "", "", -1, fmt.Errorf("not authenticated; call Authenticate first")
	}
	c.runScripts = append(c.runScripts, script)
	return "ok", "", 0, nil
}
func (c *authRecordingSSHClient) Download(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authedPort == 0 {
		return fmt.Errorf("not authenticated; call Authenticate first")
	}
	return nil
}
func (c *authRecordingSSHClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCalls++
	return nil
}

// TestQEMUVM_ConcurrentTargets_NoCrossSessionContamination is the
// permanent CT-HARDEN-VM-1 guard. A regression that drops guestMu
// re-exposes the cross-target port observation and fails the assertion
// (the primary oracle); run under -race, which additionally flags any
// unsynchronized access on the real QEMUVM/ssh code paths.
func TestQEMUVM_ConcurrentTargets_NoCrossSessionContamination(t *testing.T) {
	fake := &racingSharedSSHClient{observedPortAtRun: map[string]int{}}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, fake, nil, true)

	const numTargets = 8
	const itersPerTarget = 25
	var wg sync.WaitGroup
	errs := make(chan string, numTargets*itersPerTarget)
	for i := 0; i < numTargets; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			port := 100 + i
			for iter := 0; iter < itersPerTarget; iter++ {
				script := fmt.Sprintf("target-%d-iter-%d", i, iter)
				if _, _, _, err := v.Run(context.Background(), port, script, nil, time.Second); err != nil {
					errs <- fmt.Sprintf("target %d iter %d: Run error: %v", i, iter, err)
					continue
				}
				fake.resultMu.Lock()
				observed := fake.observedPortAtRun[script]
				fake.resultMu.Unlock()
				if observed != port {
					errs <- fmt.Sprintf("CROSS-TARGET SESSION CONTAMINATION: target %d observed port %d, want %d", i, observed, port)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestVMRunOne_NetworkShaping_AuthenticatesBeforeApplying is the
// permanent CT-HARDEN-VM-2 guard. It asserts the tc-qdisc command
// actually executed (against an authenticated session) and that
// Authenticate ran before it — a regression to the raw-unauthenticated-
// client path fails both.
func TestVMRunOne_NetworkShaping_AuthenticatesBeforeApplying(t *testing.T) {
	// A test that reaches Teardown MUST override teardownGracePeriod +
	// killByPortHook or it silently costs 30 real seconds.
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 10 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()
	prevKill := killByPortHook
	killByPortHook = func(context.Context, int) (KillReport, error) {
		return KillReport{Matched: 1, Sigtermed: []int{1}}, nil
	}
	defer func() { killByPortHook = prevKill }()

	ssh := &authRecordingSSHClient{}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, &fakeQMPClient{}, true)
	r := NewQEMUMatrixRunner(v, nil)

	dir := t.TempDir()
	res, err := r.RunMatrix(context.Background(), VMMatrixConfig{
		Targets:        []VMTarget{{ID: "alpine-x86_64", Arch: "x86_64", Distro: "alpine"}},
		Script:         "/tmp/x.sh",
		EvidenceDir:    dir,
		Concurrent:     1,
		ImageManifest:  "unused-manifest.json", // r.store==nil, never loaded
		NetworkProfile: "4g",
	})
	if err != nil {
		t.Fatalf("RunMatrix: %v", err)
	}
	row := res.Rows[0]

	for _, fs := range row.FailureSummaries {
		if fs.Type == "network-shaping-warning" {
			t.Fatalf("network-shaping-warning present post-fix: %s", fs.Message)
		}
	}
	sawTc := false
	for _, s := range ssh.runScripts {
		if strings.Contains(s, "tc qdisc") && strings.Contains(s, "rate 6000kbit") {
			sawTc = true
		}
	}
	if !sawTc {
		t.Fatalf("expected the tc-qdisc command to have actually executed; runScripts=%v", ssh.runScripts)
	}
	if ssh.authCalls == 0 {
		t.Fatalf("expected at least one Authenticate call before the tc-qdisc command ran")
	}
	if row.NetworkProfile != "4g" {
		t.Fatalf("attestation row MUST carry NetworkProfile=4g; got %q", row.NetworkProfile)
	}
}
