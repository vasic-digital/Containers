package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/remote"
)

// -----------------------------------------------------------------------
// ORCH-1 — duplicate-named services: a single failing instance among
// healthy siblings must not starve a dependent nor trigger rollback of the
// healthy sibling.
// -----------------------------------------------------------------------

// TestWave20_OrchHard_ORCH1_DuplicateNameOneFailureDoesNotStarveDependent
// reproduces the exact scenario from the audit: two services named "db"
// (db1 succeeds, db2 fails), neither Required, plus a Required "app"
// depending on "db". Under the bug, failedNames["db"] was set true from db2
// ALONE, so "app" was skipped as a required-dependency failure, StartAll
// failed, and rollback() tore down the HEALTHY db1. Under the fix, "db" is
// only "failed" once EVERY instance sharing that name has failed, so app
// proceeds (db1 is a healthy provider of "db"), StartAll succeeds, and
// nothing is rolled back.
func TestWave20_OrchHard_ORCH1_DuplicateNameOneFailureDoesNotStarveDependent(t *testing.T) {
	tmpDir := t.TempDir()
	db1Path := filepath.Join(tmpDir, "db1.yml")
	db2Path := filepath.Join(tmpDir, "db2.yml")
	appPath := filepath.Join(tmpDir, "app.yml")
	require.NoError(t, os.WriteFile(db1Path, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(db2Path, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(appPath, []byte("version: '3'\n"), 0644))

	orch := &depComposeOrch{upErr: map[string]error{"db2.yml": fmt.Errorf("boom")}}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "db", ComposeFile: db1Path})                                                // healthy instance
	o.AddService(Service{Name: "db", ComposeFile: db2Path})                                                // failing sibling, NOT required
	o.AddService(Service{Name: "app", ComposeFile: appPath, Dependencies: []string{"db"}, Required: true}) // depends on "db"

	err := o.StartAll(context.Background())
	require.NoError(t, err,
		"app must not be skipped/fail StartAll: a healthy 'db' sibling (db1) satisfies the dependency")

	ups := orch.upFiles()
	assert.Contains(t, ups, "db1.yml", "the healthy db instance must have been attempted")
	assert.Contains(t, ups, "app.yml",
		"app must NOT be skipped as a failed-dependency casualty when a sibling named 'db' succeeded")

	downed := orch.downFiles()
	assert.NotContains(t, downed, "db1.yml",
		"the healthy db1 must never be rolled back due to its failing sibling db2")
	assert.Empty(t, downed, "StartAll succeeded overall; nothing should be rolled back")
}

// TestWave20_OrchHard_ORCH1_AllInstancesOfNameFailStillStarvesDependent is the
// negation-proof: when EVERY instance sharing a name fails, the dependent
// must still be skipped (Required, so StartAll fails). This guards against a
// fix that goes too far and never marks a name failed at all.
func TestWave20_OrchHard_ORCH1_AllInstancesOfNameFailStillStarvesDependent(t *testing.T) {
	tmpDir := t.TempDir()
	db1Path := filepath.Join(tmpDir, "db1.yml")
	db2Path := filepath.Join(tmpDir, "db2.yml")
	appPath := filepath.Join(tmpDir, "app.yml")
	require.NoError(t, os.WriteFile(db1Path, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(db2Path, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(appPath, []byte("version: '3'\n"), 0644))

	orch := &depComposeOrch{upErr: map[string]error{
		"db1.yml": fmt.Errorf("boom1"),
		"db2.yml": fmt.Errorf("boom2"),
	}}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "db", ComposeFile: db1Path})
	o.AddService(Service{Name: "db", ComposeFile: db2Path})
	o.AddService(Service{Name: "app", ComposeFile: appPath, Dependencies: []string{"db"}, Required: true})

	err := o.StartAll(context.Background())
	require.Error(t, err, "app (Required) must fail StartAll when every 'db' instance failed")

	ups := orch.upFiles()
	assert.NotContains(t, ups, "app.yml",
		"app must be skipped: every instance sharing its dependency name 'db' failed")
}

// -----------------------------------------------------------------------
// ORCH-2 — rollback's teardown context must carry a bounded deadline.
// -----------------------------------------------------------------------

// blockingDownOrch is a ComposeOrchestrator whose Down blocks until its ctx
// is Done (deadline/cancel) and never returns on its own. Used to prove
// rollback's teardown context is bounded (ORCH-2): with the pre-fix
// context.WithoutCancel(ctx) (no deadline, no cancellation reachable from a
// background parent), Down would block forever and StartAll — which calls
// rollback synchronously on a required-service failure — would never return.
type blockingDownOrch struct {
	upErr map[string]error
}

func (b *blockingDownOrch) Up(_ context.Context, p compose.ComposeProject) error {
	if b.upErr != nil {
		if e, ok := b.upErr[filepath.Base(p.File)]; ok {
			return e
		}
	}
	return nil
}

func (b *blockingDownOrch) Down(ctx context.Context, _ compose.ComposeProject) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestWave20_OrchHard_ORCH2_RollbackTeardownContextHasDeadline proves StartAll
// returns within a bounded time even when the rollback Down call for an
// already-started service blocks indefinitely (simulating a wedged compose
// down). This exercises the exact failure mode a Wave-16 rollback goroutine
// could hit in production: a hung teardown must not hang the whole boot.
func TestWave20_OrchHard_ORCH2_RollbackTeardownContextHasDeadline(t *testing.T) {
	tmpDir := t.TempDir()
	okPath := filepath.Join(tmpDir, "ok-compose.yml")
	reqPath := filepath.Join(tmpDir, "req-compose.yml")
	require.NoError(t, os.WriteFile(okPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(reqPath, []byte("version: '3'\n"), 0644))

	orch := &blockingDownOrch{upErr: map[string]error{"req-compose.yml": fmt.Errorf("boom")}}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "ok", ComposeFile: okPath})                   // starts fine, then rolled back
	o.AddService(Service{Name: "req", ComposeFile: reqPath, Required: true}) // required, fails

	done := make(chan error, 1)
	go func() { done <- o.StartAll(context.Background()) }()

	select {
	case err := <-done:
		require.Error(t, err, "a required service failing must still fail StartAll")
	case <-time.After(rollbackTimeout + 15*time.Second):
		t.Fatal("StartAll did not return within rollbackTimeout — rollback's teardown " +
			"context has no bounded deadline (ORCH-2)")
	}
}

// -----------------------------------------------------------------------
// ORCH-3 — startRemote's CopyDir must run on a bounded context.
// -----------------------------------------------------------------------

// blockingCopyDirExec is a RemoteExecutor whose Execute succeeds immediately
// but whose CopyDir blocks until its ctx is Done. Used to prove startRemote
// derives a bounded context for the CopyDir call (ORCH-3): with the pre-fix
// raw, unbounded ctx, a stalled scp would hang that service's goroutine
// forever, and because StartAll's level loop drains resultChan only after
// wg.Wait() completes, the stall would hang StartAll for EVERY service in
// the level.
type blockingCopyDirExec struct{}

func (b *blockingCopyDirExec) Execute(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (b *blockingCopyDirExec) CopyDir(ctx context.Context, _ remote.RemoteHost, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestWave20_OrchHard_ORCH3_StartRemoteCopyDirHasDeadline proves StartAll
// returns within a bounded time even when startRemote's CopyDir call blocks
// indefinitely (simulating a stalled scp to a remote host).
func TestWave20_OrchHard_ORCH3_StartRemoteCopyDirHasDeadline(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	exec := &blockingCopyDirExec{}
	hostMgr := &mockHostManager{hosts: []remote.RemoteHost{{Name: "h1", Address: "10.0.0.1", User: "deploy"}}}

	o := New(WithRemoteExecutor(exec), WithHostManager(hostMgr), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "svc", ComposeFile: composePath}) // not Required

	done := make(chan error, 1)
	go func() { done <- o.StartAll(context.Background()) }()

	select {
	case err := <-done:
		// The remote copy times out and no local orchestrator is configured,
		// so the (non-Required) service ends up failed but StartAll itself
		// reports success overall — the point of this test is the BOUND, not
		// this particular outcome.
		require.NoError(t, err)
	case <-time.After(remoteCopyDirTimeout + 15*time.Second):
		t.Fatal("StartAll did not return within remoteCopyDirTimeout — startRemote's " +
			"CopyDir call has no bounded context (ORCH-3)")
	}
}
