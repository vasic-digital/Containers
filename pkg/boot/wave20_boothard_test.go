package boot

// Wave-20 batch CT-HARDEN-BOOT-HARD — §11.4.115 GREEN-polarity regression
// guards for BOOT-1..BOOT-5. White-box (package boot) so they reuse the
// in-package test doubles (moreMock*) and assert on unexported invocation
// counters.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the fail-attribution
// (BOOT-1), partial-boot rollback (BOOT-2), Undistribute-on-shutdown
// (BOOT-3), Phase-3 no-double-count / root-cause-preservation (BOOT-4),
// and re-runnability (BOOT-5) LOGIC of BootManager via the in-package
// fakes. They exercise the orchestration decisions, NOT a live container
// runtime — which is what pkg/boot is (a device-independent composition
// layer, single-goroutine, host-testable per the batch brief).

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/endpoint"
)

// --- Local compose doubles for the BOOT-2 rollback guards ---

// perFileMockOrchestrator errors Up per compose file and records which
// files were ComposeDown'd (for the partial-boot rollback assertion).
type perFileMockOrchestrator struct {
	upErrByFile map[string]error
	downedFiles []string
}

func (m *perFileMockOrchestrator) Up(_ context.Context, p compose.ComposeProject, _ ...compose.UpOption) error {
	if m.upErrByFile != nil {
		if err, ok := m.upErrByFile[p.File]; ok {
			return err
		}
	}
	return nil
}
func (m *perFileMockOrchestrator) Down(_ context.Context, p compose.ComposeProject, _ ...compose.DownOption) error {
	m.downedFiles = append(m.downedFiles, p.File)
	return nil
}
func (m *perFileMockOrchestrator) Status(_ context.Context, _ compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}
func (m *perFileMockOrchestrator) Logs(_ context.Context, _ compose.ComposeProject, _ string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// cancelOnUpOrchestrator cancels the boot ctx the moment a group Up
// succeeds, so BootAll's between-phase ctx.Err() check fires and rolls
// back. It records Down'd files so the guard proves rollback ran.
type cancelOnUpOrchestrator struct {
	cancel      context.CancelFunc
	downedFiles []string
}

func (m *cancelOnUpOrchestrator) Up(_ context.Context, _ compose.ComposeProject, _ ...compose.UpOption) error {
	if m.cancel != nil {
		m.cancel()
	}
	return nil
}
func (m *cancelOnUpOrchestrator) Down(_ context.Context, p compose.ComposeProject, _ ...compose.DownOption) error {
	m.downedFiles = append(m.downedFiles, p.File)
	return nil
}
func (m *cancelOnUpOrchestrator) Status(_ context.Context, _ compose.ComposeProject) ([]compose.ServiceStatus, error) {
	return nil, nil
}
func (m *cancelOnUpOrchestrator) Logs(_ context.Context, _ compose.ComposeProject, _ string) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// --- BOOT-1: distributor total/partial failure MUST NOT report success ---

// TestWave20_BOOT1_RequiredRemoteShortfall_Fails proves that a distributor
// deploying 0 of 1 required-remote endpoints does NOT get marked success:
// the endpoint is failed, summary.Failed==1, and BootAll returns an error.
// Pre-fix, Phase 2.5 unconditionally marked every remote name "distributed"
// + Remote++ and BootAll returned nil (the swallowed-failure bug).
func TestWave20_BOOT1_RequiredRemoteShortfall_Fails(t *testing.T) {
	dist := &moreMockDistributor{deployed: 0, err: errors.New("no hosts available")}
	eps := map[string]endpoint.ServiceEndpoint{
		"remote-req": {
			Enabled:  true,
			Remote:   true,
			Required: true, // required → shortfall MUST fail the boot
			// no ComposeFile, no health checker
		},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err, "required remote shortfall must fail BootAll")
	assert.Equal(t, 1, summary.Failed, "the undeployed required endpoint counts as failed")
	assert.Equal(t, 0, summary.Remote, "an undeployed endpoint must not count as remote")
	require.NotNil(t, summary.Results["remote-req"])
	assert.Equal(t, "failed", summary.Results["remote-req"].Status)
}

// TestWave20_BOOT1_PartialDeploy_AttributesByCount proves count-based
// attribution: 1 of 2 required-remote endpoints deployed → exactly 1
// Remote + 1 Failed, and BootAll fails (a required endpoint was not
// deployed). Pre-fix, BOTH were marked "distributed"/Remote++ (==2) and
// BootAll returned nil.
func TestWave20_BOOT1_PartialDeploy_AttributesByCount(t *testing.T) {
	dist := &moreMockDistributor{deployed: 1, err: nil}
	eps := map[string]endpoint.ServiceEndpoint{
		"r1": {Enabled: true, Remote: true, Required: true},
		"r2": {Enabled: true, Remote: true, Required: true},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err, "1/2 deployed with required endpoints must fail BootAll")
	assert.Equal(t, 1, summary.Remote, "exactly one endpoint genuinely deployed")
	assert.Equal(t, 1, summary.Failed, "exactly one endpoint is the shortfall")
}

// --- BOOT-2: partial boot is rolled back on failure / cancel ---

// TestWave20_BOOT2_RollbackDownsBootedGroupOnFailure proves that when one
// compose group Up's successfully and another (required) fails, the
// already-booted group is ComposeDown'd before BootAll returns the error.
// Pre-fix, BootAll had no Down anywhere and the succeeded group leaked.
func TestWave20_BOOT2_RollbackDownsBootedGroupOnFailure(t *testing.T) {
	orch := &perFileMockOrchestrator{
		upErrByFile: map[string]error{"b.yml": errors.New("image pull failed")},
	}
	eps := map[string]endpoint.ServiceEndpoint{
		"a": {Enabled: true, Required: true, ComposeFile: "a.yml"},
		"b": {Enabled: true, Required: true, ComposeFile: "b.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err, "required group b failed → BootAll error")
	assert.GreaterOrEqual(t, summary.Failed, 1)
	// The succeeded group (a.yml) MUST have been torn down; the failed
	// group (b.yml) was never Up'd so must NOT be Down'd.
	assert.Contains(t, orch.downedFiles, "a.yml",
		"already-booted group must be rolled back")
	assert.NotContains(t, orch.downedFiles, "b.yml",
		"never-booted group must not be torn down")
}

// TestWave20_BOOT2_RollbackOnContextCancel proves that a ctx cancel between
// phases rolls back the booted group and returns the ctx error. Pre-fix,
// ctx.Err() was never checked and the booted group leaked on cancel.
func TestWave20_BOOT2_RollbackOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	orch := &cancelOnUpOrchestrator{cancel: cancel}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))
	_, err := bm.BootAll(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "cancelled boot returns ctx error")
	assert.Contains(t, orch.downedFiles, "c.yml",
		"booted group must be rolled back on cancel")
}

// --- BOOT-3: Shutdown tears down distributed remote state ---

// TestWave20_BOOT3_ShutdownInvokesUndistribute proves Shutdown calls the
// distributor's Undistribute (so remote endpoints/tunnels/volumes do not
// leak). Pre-fix, Shutdown only ran ComposeDown and never touched the
// distributor.
func TestWave20_BOOT3_ShutdownInvokesUndistribute(t *testing.T) {
	dist := &moreMockDistributor{}
	orch := &moreMockOrchestrator{}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch), WithDistributor(dist))
	err := bm.Shutdown(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, dist.undistributeCalled,
		"Shutdown must invoke Undistribute exactly once")
}

// TestWave20_BOOT3_ShutdownAggregatesUndistributeError proves an
// Undistribute error surfaces from Shutdown (not swallowed).
func TestWave20_BOOT3_ShutdownAggregatesUndistributeError(t *testing.T) {
	dist := &moreMockDistributor{undistributeErr: errors.New("tunnel close failed")}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(&moreMockOrchestrator{}), WithDistributor(dist))
	err := bm.Shutdown(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnel close failed")
	assert.Equal(t, 1, dist.undistributeCalled)
}

// --- BOOT-4: compose-failed required endpoint counted once, root cause kept ---

// TestWave20_BOOT4_ComposeFailThenHealthProbe_NoDoubleCount proves that a
// required endpoint whose compose Up failed is counted in summary.Failed
// exactly ONCE and keeps its compose-up root-cause error even though Phase 3
// re-probes it (unhealthy). Pre-fix, Phase 3 re-recorded it → Failed==2 and
// replaced the compose error with the health error.
func TestWave20_BOOT4_ComposeFailThenHealthProbe_NoDoubleCount(t *testing.T) {
	orch := &moreMockOrchestrator{upErr: errors.New("compose up failed")}
	hc := &moreMockHealthChecker{results: map[string]bool{"svc": false}}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {
			Enabled:     true,
			Required:    true,
			ComposeFile: "c.yml",
			HealthType:  "tcp",
		},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch), WithHealthChecker(hc))
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err)
	assert.Equal(t, 1, summary.Failed, "one endpoint → Failed counted once, not twice")
	require.NotNil(t, summary.Results["svc"])
	require.Error(t, summary.Results["svc"].Error)
	assert.Contains(t, summary.Results["svc"].Error.Error(), "compose up failed",
		"compose-up root cause must be preserved, not clobbered by the health error")
}

// --- BOOT-5: BootAll is re-runnable; counters agree with the results map ---

// TestWave20_BOOT5_ReRunnable_CountsMatchResults proves a 2nd BootAll on the
// same manager produces a summary whose counters match ITS OWN results map.
// Pre-fix, bm.results was never reset: the Phase-2 already-recorded guard
// skipped run-1 endpoints (Started stayed 0) while stale run-1 "started"
// results leaked into run-2's summary — counters and map disagreed.
func TestWave20_BOOT5_ReRunnable_CountsMatchResults(t *testing.T) {
	orch := &moreMockOrchestrator{}
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": {Enabled: true, ComposeFile: "c.yml"},
	}

	bm := NewBootManager(eps, WithOrchestrator(orch))

	s1, err1 := bm.BootAll(context.Background())
	require.NoError(t, err1)
	require.Equal(t, 1, s1.Started)

	s2, err2 := bm.BootAll(context.Background())
	require.NoError(t, err2)

	startedInMap := 0
	for _, r := range s2.Results {
		if r.Status == "started" {
			startedInMap++
		}
	}
	assert.Equal(t, 1, s2.Started, "2nd run must re-boot the endpoint (not skip it)")
	assert.Equal(t, s2.Started, startedInMap,
		"2nd run Started counter must equal the number of 'started' results in its own map")
	assert.Len(t, s2.Results, 1, "2nd run results map holds only this run's entries")
}
