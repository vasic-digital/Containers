package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/remote"
)

// -----------------------------------------------------------------------
// OR3-1 (MED §11.4.14 / §11.4.108 symmetric-sibling, CF2-1/VM2-7 class) —
// StartService's REMOTE branch returned the post-start health-check error
// WITHOUT ever recording the running compose entity. StartAll deliberately
// keeps startedOK=true on a health failure precisely "so rollback/StopAll
// can tear it down" (orchestrator.go OR-4 comment) — StartService, the
// symmetric sibling, did not: on a remote-only deployment (localOrch nil) a
// service that came up remotely then failed its health check was left
// UNTRACKED, so a later StopAll — which can only route a remote teardown to a
// tracked-remote record — silently skipped it (untracked + localOrch nil =>
// the per-service "continue"), orphaning the running remote container forever.
//
// Local-started services are immune: StopAll's per-service loop issues a
// localOrch.Down for EVERY service (tracked or not) when a local orchestrator
// is configured, so the finding is specific to the remote-only shape.
// -----------------------------------------------------------------------

// or3CapturingExec is a RemoteExecutor that unconditionally succeeds (exit 0)
// and records every command string it is asked to Execute, so a remote
// teardown attempt (`docker compose ... down`) is directly observable.
type or3CapturingExec struct {
	mu   sync.Mutex
	cmds []string
}

func (e *or3CapturingExec) Execute(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
	e.mu.Lock()
	e.cmds = append(e.cmds, cmd)
	e.mu.Unlock()
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (e *or3CapturingExec) CopyDir(_ context.Context, _ remote.RemoteHost, _, _ string) error {
	return nil
}

func (e *or3CapturingExec) commands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.cmds))
	copy(out, e.cmds)
	return out
}

// TestWave20_OR3_StartServiceRemoteHealthFailStillReapableByStopAll is the
// physical teardown oracle for OR3-1: it drives the REAL StartService remote
// path with an always-unhealthy health checker and NO local orchestrator (a
// supported remote-only shape), then proves a subsequent StopAll actually
// issues the remote `docker compose down` for the running-but-unhealthy
// service. Pre-fix the service was never tracked, StopAll skipped it, and no
// down command was ever issued (the orphan); post-fix the running entity is
// recorded before the health gate, so StopAll reaps it.
func TestWave20_OR3_StartServiceRemoteHealthFailStillReapableByStopAll(t *testing.T) {
	tmpDir := t.TempDir()
	svcDir := filepath.Join(tmpDir, "svc")
	require.NoError(t, os.MkdirAll(svcDir, 0755))
	composePath := filepath.Join(svcDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	exec := &or3CapturingExec{}
	hostMgr := &moreTestHostMgr{
		hosts: []remote.RemoteHost{{Name: "h1", Address: "10.0.0.1", User: "u"}},
	}

	// Remote-only orchestrator: deliberately NO WithLocalOrchestrator (a
	// supported deployment shape), with an always-unhealthy health checker.
	// The service opts into the health gate via HealthPort > 0.
	o := New(
		WithRemoteExecutor(exec),
		WithHostManager(hostMgr),
		WithHealthChecker(&or4AlwaysUnhealthyChecker{}),
		WithProjectDir(tmpDir),
	)
	require.True(t, o.IsRemoteEnabled(), "remote must be enabled (host manager + remote executor set)")
	o.AddService(Service{Name: "svc", ComposeFile: composePath, HealthPort: 8080})

	// StartService: startRemote succeeds (fake exec exits 0 for mkdir + up) =>
	// the remote compose entity is UP => the always-unhealthy checker then
	// fails => StartService returns the health error.
	startErr := o.StartService(context.Background(), "svc")
	require.Error(t, startErr,
		"the always-unhealthy health checker must fail StartService for a HealthPort service")

	// The container is running on the remote host. A later StopAll MUST be able
	// to reap it — it must route a remote `docker compose down` to the host the
	// service was started on.
	require.NoError(t, o.StopAll(context.Background()))

	var sawDown bool
	for _, c := range exec.commands() {
		if strings.Contains(c, "down") && strings.Contains(c, "/svc") {
			sawDown = true
		}
	}
	assert.True(t, sawDown,
		"OR3-1: StopAll must issue a remote 'docker compose down' for a service "+
			"that StartService started remotely then found unhealthy — the running "+
			"remote container must be reapable, not orphaned by StartService "+
			"returning the health error before recording the started entity.\n"+
			"captured commands: %v", exec.commands())
}
