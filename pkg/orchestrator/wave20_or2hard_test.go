package orchestrator

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
	"digital.vasic.containers/pkg/remote"
)

// -----------------------------------------------------------------------
// OR-1 (HIGH SECURITY / RCE) — startRemote's composeCmd/mkdirCmd interpolate
// svc.Profile / the compose-dir basename / remoteDest via fmt.Sprintf into a
// single string handed whole to `ssh` as one argv element (pkg/remote's
// SSHExecutor.Execute), so the REMOTE LOGIN SHELL parses it. Any shell
// metacharacter in those values is remote command execution.
// -----------------------------------------------------------------------

// TestWave20_OR2Hard_OR1_StartRemote_ProfileInjectionNeutralised is the
// physical shell-execution oracle (§11.4.107/§11.4.115), mirroring
// pkg/distribution's TestDeployRemote_CommandInjection: it drives the REAL
// startRemote with a decoy shell-injection payload in Service.Profile,
// captures the exact command string startRemote hands to the (fake) remote
// executor, then ACTUALLY EXECUTES that string through a real POSIX shell.
// If the payload's embedded `touch` fires, svc.Profile was NOT neutralised
// (RCE); if the sentinel file is absent, the metacharacters were quoted
// inert (the fix).
//
// A safe per-test sentinel path (t.TempDir()) is used instead of the
// literal "/tmp/pwned" from the finding description — same injection
// technique (`; touch <path> #`), no shared-host cleanup risk (§11.4.14).
func TestWave20_OR2Hard_OR1_StartRemote_ProfileInjectionNeutralised(t *testing.T) {
	sh, err := osexec.LookPath("sh")
	if err != nil {
		t.Skip("SKIP-with-reason (§11.4.3): no POSIX /bin/sh available")
	}

	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	sentinel := filepath.Join(tmpDir, "pwned")
	payload := "core; touch " + sentinel + " #"

	hostMgr := &moreTestHostMgr{
		hosts: []remote.RemoteHost{{Name: "h1", Address: "10.0.0.1", User: "u"}},
	}
	var capturedCmd string
	fakeExec := &moreTestRemoteExec{
		executeFunc: func(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			capturedCmd = cmd // last call wins: mkdir, then the compose command
			return &remote.CommandResult{ExitCode: 0}, nil
		},
		copyDirFunc: func(_ context.Context, _ remote.RemoteHost, _, _ string) error {
			return nil
		},
	}

	o := New(WithRemoteExecutor(fakeExec), WithHostManager(hostMgr))
	o.remoteEnabled = true

	svc := Service{Name: "victim", ComposeFile: "docker-compose.yml", Profile: payload}
	startErr := o.startRemote(context.Background(), svc, composePath)
	require.NoError(t, startErr)
	require.NotEmpty(t, capturedCmd, "startRemote must have executed a compose command")

	// Physical oracle: actually run the exact string startRemote would hand
	// to `ssh` as one argv element, through a real shell.
	_ = osexec.Command(sh, "-c", capturedCmd).Run() // exit code irrelevant
	_, statErr := os.Stat(sentinel)
	require.True(t, statErr != nil,
		"OR-1: svc.Profile shell metacharacters were NOT neutralised — the "+
			"decoy payload's `touch` executed via the (simulated) remote login "+
			"shell.\ncaptured cmd: %s", capturedCmd)
}

// -----------------------------------------------------------------------
// OR-2 (HIGH §11.4.14) — rollback (and StopAll) called ONLY
// o.localOrch.Down(...) regardless of whether a service was started via
// startRemote (SSH) or startLocal; when localOrch==nil (a supported
// remote-only config) both silently no-op, orphaning every remote-started
// service forever.
// -----------------------------------------------------------------------

// or2CapturingExec is a RemoteExecutor that records every command it is
// asked to Execute (so a teardown attempt is directly observable) and fails
// the compose "up" call for any service whose remote destination directory
// name contains "req" (used to force the Required-service failure that
// triggers rollback).
type or2CapturingExec struct {
	mu   sync.Mutex
	cmds []string
}

func (e *or2CapturingExec) Execute(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
	e.mu.Lock()
	e.cmds = append(e.cmds, cmd)
	e.mu.Unlock()

	if strings.Contains(cmd, "/req") && strings.Contains(cmd, "up") {
		return &remote.CommandResult{ExitCode: 1, Stderr: "compose: image not found"}, nil
	}
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (e *or2CapturingExec) CopyDir(_ context.Context, _ remote.RemoteHost, _, _ string) error {
	return nil
}

func (e *or2CapturingExec) commands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.cmds))
	copy(out, e.cmds)
	return out
}

// TestWave20_OR2Hard_OR2_RollbackTearsDownRemoteStartedServiceWhenNoLocalOrch
// reproduces the exact OR-2 repro scenario: a remote-only orchestrator (no
// localOrch configured at all — a supported deployment shape), a
// remote-started "ok" service, and a failing Required "req" service. When
// "req" fails, StartAll's rollback() must attempt to tear down "ok" via the
// remote executor (docker compose down on the host it was actually started
// on) — NOT silently no-op because o.localOrch is nil.
func TestWave20_OR2Hard_OR2_RollbackTearsDownRemoteStartedServiceWhenNoLocalOrch(t *testing.T) {
	tmpDir := t.TempDir()
	okDir := filepath.Join(tmpDir, "ok")
	reqDir := filepath.Join(tmpDir, "req")
	require.NoError(t, os.MkdirAll(okDir, 0755))
	require.NoError(t, os.MkdirAll(reqDir, 0755))
	okCompose := filepath.Join(okDir, "docker-compose.yml")
	reqCompose := filepath.Join(reqDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(okCompose, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(reqCompose, []byte("version: '3'\n"), 0644))

	hostMgr := &moreTestHostMgr{
		hosts: []remote.RemoteHost{{Name: "h1", Address: "10.0.0.1", User: "u"}},
	}
	fakeExec := &or2CapturingExec{}

	// Deliberately NO WithLocalOrchestrator — a remote-only orchestrator is a
	// supported configuration (the finding's exact repro shape).
	o := New(WithRemoteExecutor(fakeExec), WithHostManager(hostMgr), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "ok", ComposeFile: okCompose})
	o.AddService(Service{Name: "req", ComposeFile: reqCompose, Required: true})

	err := o.StartAll(context.Background())
	require.Error(t, err, "req (Required) failing must fail StartAll")

	cmds := fakeExec.commands()
	var teardownCmd string
	for _, c := range cmds {
		if strings.Contains(c, "down") && strings.Contains(c, "/ok") {
			teardownCmd = c
		}
	}
	assert.NotEmpty(t, teardownCmd,
		"OR-2: rollback must attempt to tear down the remote-started 'ok' "+
			"service via the remote executor when no local orchestrator is "+
			"configured, instead of silently no-op'ing.\ncaptured commands: %v",
		cmds)
}

// -----------------------------------------------------------------------
// OR-3 (HIGH) — StopAll's teardown loop uses the caller's ctx verbatim,
// sequentially, with NO bound (unlike rollback, which wraps rollbackTimeout)
// — one wedged Down hangs shutdown forever AND the remaining stoppable
// services are never even attempted.
// -----------------------------------------------------------------------

// countingBlockingDownOrch is a ComposeOrchestrator whose Down blocks until
// its ctx is Done and records how many times it was invoked, so a test can
// prove EVERY service's teardown was at least attempted even though each
// call blocks.
type countingBlockingDownOrch struct {
	mu        sync.Mutex
	downCalls int
}

func (b *countingBlockingDownOrch) Up(_ context.Context, _ compose.ComposeProject) error {
	return nil
}

func (b *countingBlockingDownOrch) Down(ctx context.Context, _ compose.ComposeProject) error {
	b.mu.Lock()
	b.downCalls++
	b.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (b *countingBlockingDownOrch) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.downCalls
}

// TestWave20_OR2Hard_OR3_StopAll_BoundedAndAttemptsSecondService proves
// StopAll returns within a bounded time even when the FIRST service's Down
// call blocks indefinitely (simulating a wedged docker daemon), AND that the
// second service's teardown is still attempted rather than starved by the
// first one's stall.
func TestWave20_OR2Hard_OR3_StopAll_BoundedAndAttemptsSecondService(t *testing.T) {
	orch := &countingBlockingDownOrch{}
	o := New(WithLocalOrchestrator(orch))
	o.AddService(Service{Name: "svcA", ComposeFile: "a.yml"})
	o.AddService(Service{Name: "svcB", ComposeFile: "b.yml"})

	done := make(chan error, 1)
	go func() { done <- o.StopAll(context.Background()) }()

	select {
	case <-done:
		// returned within the bound; fall through to the call-count check.
	case <-time.After(rollbackTimeout + 15*time.Second):
		t.Fatal("OR-3: StopAll did not return within a bounded time — a single " +
			"wedged Down call hangs shutdown forever")
	}

	assert.Equal(t, 2, orch.calls(),
		"OR-3: StopAll must attempt EVERY service's teardown even when an "+
			"earlier one's Down call blocks/wedges — a stuck first Down must "+
			"not prevent the second service from even being attempted")
}

// -----------------------------------------------------------------------
// OR-4 (HIGH §11.4.108) — o.healthChecker (set by WithHealthChecker,
// documented "Required services failing = boot failure") is never
// read/invoked: StartAll reports success the instant `docker compose up -d`
// exits 0, even if the container crash-loops.
// -----------------------------------------------------------------------

// or4AlwaysUnhealthyChecker is a health.HealthChecker stub that reports
// every target unhealthy, regardless of what it is asked to check.
type or4AlwaysUnhealthyChecker struct{}

func (a *or4AlwaysUnhealthyChecker) Check(_ context.Context, target health.HealthTarget) *health.HealthResult {
	return &health.HealthResult{Target: target.Name, Healthy: false, Error: "always down"}
}

func (a *or4AlwaysUnhealthyChecker) CheckAll(_ context.Context, targets []health.HealthTarget) []*health.HealthResult {
	out := make([]*health.HealthResult, len(targets))
	for i, tgt := range targets {
		out[i] = a.Check(context.Background(), tgt)
	}
	return out
}

// TestWave20_OR2Hard_OR4_HealthCheckFailureFailsRequiredService is the exact
// OR-4 repro: WithHealthChecker(alwaysUnhealthy) + a Required service whose
// Up returns nil must now make StartAll return an error (was nil pre-fix).
func TestWave20_OR2Hard_OR4_HealthCheckFailureFailsRequiredService(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	orch := &startTestComposeOrch{} // Up returns nil (compose "succeeds")
	o := New(
		WithLocalOrchestrator(orch),
		WithHealthChecker(&or4AlwaysUnhealthyChecker{}),
		WithProjectDir(tmpDir),
	)
	o.AddService(Service{
		Name:        "req",
		ComposeFile: composePath,
		Required:    true,
		HealthPort:  8080,
	})

	err := o.StartAll(context.Background())
	require.Error(t, err,
		"OR-4: a configured healthChecker reporting a REQUIRED service "+
			"unhealthy must fail StartAll, even though docker compose up -d "+
			"exited 0")
}

// TestWave20_OR2Hard_OR4_HealthCheckNotConfigured_NoRegression proves a
// service with NO HealthPort declared (the overwhelming majority of existing
// services) is completely unaffected by a configured healthChecker — the
// health-check gate is opt-in per service.
func TestWave20_OR2Hard_OR4_HealthCheckNotConfigured_NoRegression(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	orch := &startTestComposeOrch{}
	o := New(
		WithLocalOrchestrator(orch),
		WithHealthChecker(&or4AlwaysUnhealthyChecker{}),
		WithProjectDir(tmpDir),
	)
	o.AddService(Service{
		Name:        "req",
		ComposeFile: composePath,
		Required:    true,
		// HealthPort intentionally left at zero-value: no health check.
	})

	err := o.StartAll(context.Background())
	require.NoError(t, err,
		"a service with no HealthPort declared must not be affected by a "+
			"configured (always-unhealthy) healthChecker")
}

// -----------------------------------------------------------------------
// OR-5 (MED) — DiscoverServices holds o.mu across the blocking
// filepath.Walk (MANDATORY PRINCIPLE #2 / Wave-16 class).
// -----------------------------------------------------------------------

// TestWave20_OR2Hard_OR5_DiscoverServices_DoesNotHoldLockDuringWalk proves
// DiscoverServices releases o.mu BEFORE the blocking directory walk, mirroring
// the existing Wave-16 StartAll/StopAll/StartService lock-scope proofs. The
// package-level walkDir indirection is substituted with a blocking stub so
// the proof is deterministic (no reliance on real-filesystem walk timing).
func TestWave20_OR2Hard_OR5_DiscoverServices_DoesNotHoldLockDuringWalk(t *testing.T) {
	tmpDir := t.TempDir()
	o := New(WithProjectDir(tmpDir))
	o.AddService(Service{Name: "pre-existing", ComposeFile: "x.yml"})

	entered := make(chan struct{})
	release := make(chan struct{})
	origWalk := walkDir
	walkDir = func(root string, fn filepath.WalkFunc) error {
		close(entered)
		<-release
		return origWalk(root, fn)
	}
	defer func() { walkDir = origWalk }()

	done := make(chan error, 1)
	go func() { done <- o.DiscoverServices(tmpDir) }()

	<-entered // DiscoverServices is now mid-walk

	counted := make(chan int, 1)
	go func() { counted <- o.ServiceCount() }()

	select {
	case n := <-counted:
		assert.Equal(t, 1, n)
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("OR-5: ServiceCount() blocked while DiscoverServices held o.mu " +
			"across the filepath.Walk")
	}

	close(release)
	require.NoError(t, <-done)
}

// -----------------------------------------------------------------------
// OR-6 (MED) — DiscoverServices matches only *.yml; *.yaml is an equally
// canonical compose-file extension.
// -----------------------------------------------------------------------

func TestWave20_OR2Hard_OR6_DiscoverServices_AcceptsYamlExtension(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "yamlservice")
	require.NoError(t, os.MkdirAll(subDir, 0755))
	composeFile := filepath.Join(subDir, "docker-compose.yaml")
	require.NoError(t, os.WriteFile(composeFile, []byte("version: '3'\n"), 0644))

	o := New(WithProjectDir(tmpDir))
	err := o.DiscoverServices(tmpDir)
	require.NoError(t, err)
	require.Len(t, o.services, 1,
		"OR-6: docker-compose.yaml (the .yaml extension) must be discovered, "+
			"not just .yml")
	assert.Equal(t, "yamlservice", o.services[0].Name)
}

// -----------------------------------------------------------------------
// OR-7 (LOW) — the filepath.Walk per-entry error is silently swallowed; a
// partial scan must not look like a complete one.
// -----------------------------------------------------------------------

// or7CapturingLogger is a logging.Logger stub that records every Warn call.
type or7CapturingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *or7CapturingLogger) Debug(msg string, args ...any) {}
func (l *or7CapturingLogger) Info(msg string, args ...any)  {}
func (l *or7CapturingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, fmt.Sprintf(msg, args...))
	l.mu.Unlock()
}
func (l *or7CapturingLogger) Error(msg string, args ...any) {}

func (l *or7CapturingLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

// TestWave20_OR2Hard_OR7_DiscoverServices_LogsWalkErrorsInsteadOfSwallowing
// forces a real filepath.Walk per-entry error (an unreadable subdirectory)
// and proves (a) the readable sibling is still discovered — a partial scan
// must not silently look like "nothing found" — and (b) the walk error is
// logged, not swallowed.
func TestWave20_OR2Hard_OR7_DiscoverServices_LogsWalkErrorsInsteadOfSwallowing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("SKIP-with-reason (§11.4.3): running as root — the unreadable-directory permission trick does not deny root")
	}

	tmpDir := t.TempDir()
	goodDir := filepath.Join(tmpDir, "gooddir")
	require.NoError(t, os.MkdirAll(goodDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(goodDir, "docker-compose.yml"), []byte("version: '3'\n"), 0644))

	blockedDir := filepath.Join(tmpDir, "blockeddir")
	require.NoError(t, os.MkdirAll(blockedDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(blockedDir, "docker-compose.yml"), []byte("version: '3'\n"), 0644))
	require.NoError(t, os.Chmod(blockedDir, 0000))
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0755) }) // restore so t.TempDir() cleanup can remove it

	logger := &or7CapturingLogger{}
	o := New(WithProjectDir(tmpDir), WithLogger(logger))
	err := o.DiscoverServices(tmpDir)
	require.NoError(t, err, "a partial scan (one unreadable subdir) must not fail the whole discovery")

	found := false
	for _, svc := range o.services {
		if svc.Name == "gooddir" {
			found = true
		}
	}
	assert.True(t, found, "the readable sibling directory's service must still be discovered")

	assert.Greater(t, logger.warnCount(), 0,
		"OR-7: a filepath.Walk per-entry error must be logged, not silently swallowed")
}
