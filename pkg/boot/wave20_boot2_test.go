package boot

// Wave-20 DEEPER (§11.4.118) 2nd-pass hardening — batch CT-HARDEN-BOOT-HARD-2.
// §11.4.115 GREEN-polarity regression guards for BOOT2-1. White-box (package
// boot) so they reuse the in-package test doubles (moreMock*) and assert on
// the distributor's Undistribute invocation counter.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the rollback-completeness
// LOGIC of BootManager via the in-package fakes — that a failed/cancelled boot
// tears down the distributor's live remote state (containers/tunnels/volumes),
// not only compose groups. They exercise the orchestration decision, NOT a
// live container runtime — which is what pkg/boot is (a device-independent
// composition layer, single-goroutine, host-testable per the batch brief).
//
// DEFECT BOOT2-1 (rollback-completeness beyond BOOT-2): BOOT-2 added rollback
// of compose groups on a failed/cancelled boot, but rollback tore down ONLY
// compose groups. A boot that (a) successfully distributed remote endpoints in
// Phase 2.5 and then (b) failed for ANY reason (an unrelated required health
// failure, a required-remote shortfall, or a ctx cancel) returned an error
// while leaving every successfully-distributed remote endpoint RUNNING on its
// host — the BOOT-2 partial-boot-leak class extended to the distribution path
// (§11.4.69 sink-side leak). The fix flags that the distributor ran this boot
// and has rollback call distributor.Undistribute (idempotent, best-effort) for
// every rollback site.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/endpoint"
)

// cancelOnDistributeDistributor cancels the boot ctx the moment the
// distributor is invoked, so BootAll's post-Phase-2.5 ctx.Err() check fires
// and rolls back AFTER remote state was distributed. It records Undistribute
// invocations so the guard proves the ctx-cancel rollback path tears down the
// distributed state.
type cancelOnDistributeDistributor struct {
	cancel             context.CancelFunc
	deployed           int
	undistributeCalled int
}

func (m *cancelOnDistributeDistributor) DistributeEndpoints(_ context.Context, _ []string) (int, error) {
	if m.cancel != nil {
		m.cancel()
	}
	return m.deployed, nil
}

func (m *cancelOnDistributeDistributor) Undistribute(_ context.Context) error {
	m.undistributeCalled++
	return nil
}

// TestWave20_BOOT2_DistributedRemoteTornDownWhenUnrelatedRequiredFails proves
// that when Phase 2.5 successfully distributes a remote endpoint and a SEPARATE
// required compose service then fails its health check (sinking the boot), the
// distributed remote endpoint is Undistribute'd during rollback. Pre-fix,
// rollback ran only ComposeDown and the distributed remote state leaked while
// BootAll returned an error.
func TestWave20_BOOT2_DistributedRemoteTornDownWhenUnrelatedRequiredFails(t *testing.T) {
	dist := &moreMockDistributor{deployed: 1}
	orch := &moreMockOrchestrator{}
	hc := &moreMockHealthChecker{results: map[string]bool{"svc": false}}
	eps := map[string]endpoint.ServiceEndpoint{
		// Remote, no compose → distributed in Phase 2.5 (Remote++).
		"r1": {Enabled: true, Remote: true},
		// Local required compose service whose health check fails → sinks boot.
		"svc": {Enabled: true, Required: true, ComposeFile: "c.yml", HealthType: "tcp"},
	}

	bm := NewBootManager(eps,
		WithOrchestrator(orch),
		WithHealthChecker(hc),
		WithDistributor(dist),
	)
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err, "required health failure must fail BootAll")
	require.NotNil(t, summary.Results["r1"])
	assert.Equal(t, "distributed", summary.Results["r1"].Status,
		"r1 was genuinely distributed in Phase 2.5")
	assert.Equal(t, 1, dist.undistributeCalled,
		"a failed boot MUST tear down the distributed remote endpoint, not leak it")
}

// TestWave20_BOOT2_DistributedRemoteTornDownOnRequiredShortfall proves that a
// required-remote shortfall (BOOT-1) which itself sinks the boot ALSO tears
// down the sibling remote endpoint that WAS successfully distributed. With two
// required remotes and deployed==1, exactly one is distributed and one fails
// (deterministic regardless of map order), so BootAll errors and rollback must
// Undistribute the live one. Pre-fix, the distributed sibling leaked.
func TestWave20_BOOT2_DistributedRemoteTornDownOnRequiredShortfall(t *testing.T) {
	dist := &moreMockDistributor{deployed: 1}
	eps := map[string]endpoint.ServiceEndpoint{
		"r1": {Enabled: true, Remote: true, Required: true},
		"r2": {Enabled: true, Remote: true, Required: true},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	summary, err := bm.BootAll(context.Background())

	require.Error(t, err, "1/2 required remotes deployed → BootAll must fail")
	assert.Equal(t, 1, summary.Remote, "exactly one endpoint was distributed")
	assert.Equal(t, 1, summary.Failed, "exactly one endpoint is the shortfall")
	assert.Equal(t, 1, dist.undistributeCalled,
		"the successfully-distributed sibling MUST be torn down on the failed boot")
}

// TestWave20_BOOT2_DistributedRemoteTornDownOnContextCancel proves that a ctx
// cancel occurring AFTER Phase 2.5 distributed a remote endpoint tears that
// state down via the post-Phase-2.5 ctx.Err() rollback path (a distinct
// rollback call site from the HasFailures path). Pre-fix, the cancelled boot
// returned the ctx error while leaving the distributed remote endpoint running.
func TestWave20_BOOT2_DistributedRemoteTornDownOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dist := &cancelOnDistributeDistributor{cancel: cancel, deployed: 1}
	eps := map[string]endpoint.ServiceEndpoint{
		"r1": {Enabled: true, Remote: true},
	}

	bm := NewBootManager(eps, WithDistributor(dist))
	_, err := bm.BootAll(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "cancelled boot returns ctx error")
	assert.Equal(t, 1, dist.undistributeCalled,
		"a distributed remote endpoint MUST be torn down when the boot is cancelled")
}
