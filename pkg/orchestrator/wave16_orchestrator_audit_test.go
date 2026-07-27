package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
)

// recordingComposeOrch is a ComposeOrchestrator that lets a test fail a
// specific service's Up (keyed by compose-file basename) and records which
// compose files were Down'd, so rollback behaviour is directly observable.
type recordingComposeOrch struct {
	mu    sync.Mutex
	upErr map[string]error // basename -> error to return from Up
	downs []string         // basenames Down was called with
}

func (r *recordingComposeOrch) Up(_ context.Context, p compose.ComposeProject) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upErr != nil {
		if e, ok := r.upErr[filepath.Base(p.File)]; ok {
			return e
		}
	}
	return nil
}

func (r *recordingComposeOrch) Down(ctx context.Context, p compose.ComposeProject) error {
	// Honour context cancellation so a rollback that reuses an already-canceled
	// boot context is observably a no-op (records nothing) — this is what lets
	// the WithoutCancel guard discriminate the fix from the bug.
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downs = append(r.downs, filepath.Base(p.File))
	return nil
}

func (r *recordingComposeOrch) downedFiles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.downs))
	copy(out, r.downs)
	return out
}

// TestWave16_Orchestrator_StartAll_DoesNotHoldLockDuringBoot proves StartAll
// releases o.mu BEFORE the blocking per-service boot (MANDATORY PRINCIPLE #2).
// Previously o.mu was held across the entire multi-service boot, so any other
// o.mu user (here ServiceCount) blocked until the whole boot finished. The
// stub Up blocks until the test releases it; a concurrent ServiceCount must
// still return promptly.
func TestWave16_Orchestrator_StartAll_DoesNotHoldLockDuringBoot(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	entered := make(chan struct{})
	release := make(chan struct{})
	orch := &startTestComposeOrch{
		upFunc: func() error {
			close(entered) // signal: boot is in progress (single service => called once)
			<-release      // block until the test releases us
			return nil
		},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "svc", ComposeFile: composePath})

	done := make(chan error, 1)
	go func() { done <- o.StartAll(context.Background()) }()

	<-entered // StartAll is now mid-boot, blocked inside Up

	counted := make(chan int, 1)
	go func() { counted <- o.ServiceCount() }()

	select {
	case n := <-counted:
		assert.Equal(t, 1, n)
	case <-time.After(3 * time.Second):
		close(release) // let StartAll unwind before failing
		t.Fatal("ServiceCount() blocked while StartAll held o.mu across the boot")
	}

	close(release)
	require.NoError(t, <-done)
}

// TestWave16_Orchestrator_StopAll_DoesNotHoldLockDuringTeardown is the StopAll
// sibling of the StartAll lock-scope proof: the Down loop must not stall a
// concurrent o.mu user.
func TestWave16_Orchestrator_StopAll_DoesNotHoldLockDuringTeardown(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	entered := make(chan struct{})
	release := make(chan struct{})
	orch := &startTestComposeOrch{
		downFunc: func() error {
			close(entered)
			<-release
			return nil
		},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "svc", ComposeFile: composePath})

	done := make(chan error, 1)
	go func() { done <- o.StopAll(context.Background()) }()

	<-entered

	counted := make(chan int, 1)
	go func() { counted <- o.ServiceCount() }()

	select {
	case n := <-counted:
		assert.Equal(t, 1, n)
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("ServiceCount() blocked while StopAll held o.mu across teardown")
	}

	close(release)
	require.NoError(t, <-done)
}

// TestWave16_Orchestrator_StartAll_RollsBackStartedOnRequiredFailure proves
// that when a REQUIRED service fails to start, the services that DID start are
// rolled back (compose-down) so a partial boot leaves nothing orphaned.
// Previously StartAll returned the aggregate error but left already-started
// services running.
func TestWave16_Orchestrator_StartAll_RollsBackStartedOnRequiredFailure(t *testing.T) {
	tmpDir := t.TempDir()
	okPath := filepath.Join(tmpDir, "ok-compose.yml")
	reqPath := filepath.Join(tmpDir, "req-compose.yml")
	require.NoError(t, os.WriteFile(okPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(reqPath, []byte("version: '3'\n"), 0644))

	orch := &recordingComposeOrch{
		upErr: map[string]error{"req-compose.yml": fmt.Errorf("boom")},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "ok", ComposeFile: okPath})                   // starts fine
	o.AddService(Service{Name: "req", ComposeFile: reqPath, Required: true}) // required, fails

	err := o.StartAll(context.Background())
	require.Error(t, err, "a required service failing must fail StartAll")

	downed := orch.downedFiles()
	assert.Contains(t, downed, "ok-compose.yml",
		"the service that started must be rolled back (compose-down) when a required service fails")
	assert.NotContains(t, downed, "req-compose.yml",
		"the service that never started must not be rolled back")
}

// TestWave16_Orchestrator_StartService_DoesNotHoldLockDuringStart is the
// StartService sibling of the StartAll/StopAll lock-scope proofs: StartService
// must release o.mu BEFORE the blocking start (MANDATORY PRINCIPLE #2), so a
// concurrent o.mu user is not stalled by an in-flight single-service start.
func TestWave16_Orchestrator_StartService_DoesNotHoldLockDuringStart(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	entered := make(chan struct{})
	release := make(chan struct{})
	orch := &startTestComposeOrch{
		upFunc: func() error {
			close(entered) // single service => called once
			<-release
			return nil
		},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "svc", ComposeFile: composePath})

	done := make(chan error, 1)
	go func() { done <- o.StartService(context.Background(), "svc") }()

	<-entered

	counted := make(chan int, 1)
	go func() { counted <- o.ServiceCount() }()

	select {
	case n := <-counted:
		assert.Equal(t, 1, n)
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("ServiceCount() blocked while StartService held o.mu across the start")
	}

	close(release)
	require.NoError(t, <-done)
}

// TestWave16_Orchestrator_StartAll_RollsBackEvenWhenParentContextCanceled
// proves rollback tears down started services even when the parent context is
// already canceled — the failure mode where reusing the boot context would
// silently no-op every teardown. The stub Down honours ctx cancellation, so a
// rollback on the canceled context records nothing; the fix detaches
// cancellation (WithoutCancel) so the started service is genuinely torn down.
func TestWave16_Orchestrator_StartAll_RollsBackEvenWhenParentContextCanceled(t *testing.T) {
	tmpDir := t.TempDir()
	okPath := filepath.Join(tmpDir, "ok-compose.yml")
	reqPath := filepath.Join(tmpDir, "req-compose.yml")
	require.NoError(t, os.WriteFile(okPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(reqPath, []byte("version: '3'\n"), 0644))

	orch := &recordingComposeOrch{
		upErr: map[string]error{"req-compose.yml": fmt.Errorf("boom")},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "ok", ComposeFile: okPath})
	o.AddService(Service{Name: "req", ComposeFile: reqPath, Required: true})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent context already canceled before the boot

	err := o.StartAll(ctx)
	require.Error(t, err, "a required service failing must fail StartAll")

	downed := orch.downedFiles()
	assert.Contains(t, downed, "ok-compose.yml",
		"rollback must tear down started services even when the parent context is already canceled")
}

// TestWave16_Orchestrator_StartAll_NonRequiredFailureDoesNotRollBack pins the
// Required-gated trigger: a NON-required service failing must neither fail
// StartAll nor roll back the services that started successfully.
func TestWave16_Orchestrator_StartAll_NonRequiredFailureDoesNotRollBack(t *testing.T) {
	tmpDir := t.TempDir()
	okPath := filepath.Join(tmpDir, "ok-compose.yml")
	optPath := filepath.Join(tmpDir, "opt-compose.yml")
	require.NoError(t, os.WriteFile(okPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(optPath, []byte("version: '3'\n"), 0644))

	orch := &recordingComposeOrch{
		upErr: map[string]error{"opt-compose.yml": fmt.Errorf("boom")},
	}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "ok", ComposeFile: okPath})   // succeeds
	o.AddService(Service{Name: "opt", ComposeFile: optPath}) // NOT required, fails

	err := o.StartAll(context.Background())
	require.NoError(t, err, "a non-required service failing must NOT fail StartAll")

	downed := orch.downedFiles()
	assert.Empty(t, downed,
		"a non-required failure must not trigger rollback; the started service stays up")
}
