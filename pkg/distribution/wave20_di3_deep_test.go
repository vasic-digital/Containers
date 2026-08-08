//go:build !integration

package distribution

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
)

// wave20_di3_deep_test.go — CT-HARDEN-DIST3-DEEP (Wave-20 THIRD pass,
// §11.4.118 loop-until-dry) §11.4.115 GREEN-polarity regression guards.
//
// Root cause (composition gap the first two passes missed): the DIST-1 /
// DIST-2 teardown predicates recognise ONLY StateRunning as "a genuinely-
// deployed container that must be torn down", but the LATER DI-4 fix
// (failover.go) now flips genuinely-deployed containers from StateRunning to
// StateMigrating when CheckAndFailover proves their host offline. DI-4 is a
// no-op reschedule (its own comment: the container is NOT actually moved), so
// a StateMigrating container is still deployed and may still be alive on its
// — possibly since-recovered — host. Both teardown sites
// (Undistribute + reconcileRelocations) skip it via `!= StateRunning`, so it
// is silently ORPHANED: Undistribute reports success while the container keeps
// running on its host (the exact §11.4.69 DIST-1 sink-side bluff, re-opened
// for the StateMigrating subset — a §11.4.1 "fix-A-creates-B" from composing
// DI-4 with DIST-1/DIST-2).
//
// HONEST BOUNDARY (§11.4.107): these are UNIT guards with no live container
// runtime / SSH. They prove the teardown `rm -f` COMMAND is ISSUED to the
// Executor seam for a StateMigrating container — they do NOT prove a real
// container on a real host actually died (the §11.4.108 runtime-signature
// layer, out of scope for a package unit test).

// distributeWithFailoverToMigrating deploys `name` to remote `host`, then runs
// CheckAndFailover against an OFFLINE host so the tracked entry is flipped to
// StateMigrating. It returns the distributor and the executor's issued-command
// recorder, with the recorder RESET so only post-failover commands are seen.
func distributeWithFailoverToMigrating(
	t *testing.T, name, host string, issued *[]struct{ host, cmd string },
) *DefaultDistributor {
	t.Helper()
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			*issued = append(*issued, struct{ host, cmd string }{h.Name, cmd})
			return &remote.CommandResult{ExitCode: 0}, nil
		},
		// The host is OFFLINE: CheckAndFailover uses IsReachable (not Execute)
		// for detection, so this drives the StateMigrating flip.
		reachableFunc: func(ctx context.Context, h remote.RemoteHost) bool { return false },
	}
	placement := host
	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&placement)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"host-A": {Name: "host-A", Address: "10.0.0.1", User: "u", Runtime: "podman"},
			"host-B": {Name: "host-B", Address: "10.0.0.2", User: "u", Runtime: "docker"},
		}}),
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: name, Image: "nginx"},
	})
	require.NoError(t, err)

	fh := NewFailoverHandler(dist)
	actions, err := fh.CheckAndFailover(context.Background())
	require.NoError(t, err)
	require.Len(t, actions, 1)

	after := dist.Status(context.Background())
	require.Len(t, after, 1)
	require.Equal(t, StateMigrating, after[0].State,
		"precondition: CheckAndFailover must have flipped the container to "+
			"StateMigrating before the teardown assertion")

	*issued = nil // isolate teardown commands from the deploy-time rm/run
	return dist
}

// TestWave20_DI3DEEP_UndistributeTearsDownMigratingContainer is the guard for
// the primary leak: Undistribute() must ISSUE a `rt rm -f <name>` for a
// StateMigrating container (it was genuinely deployed and may still be alive on
// its — possibly recovered — host). Surgical revert (drop StateMigrating from
// stateNeedsTeardown) -> Undistribute skips the container -> no rm issued ->
// orphan leak -> FAIL.
func TestWave20_DI3DEEP_UndistributeTearsDownMigratingContainer(t *testing.T) {
	var issued []struct{ host, cmd string }
	dist := distributeWithFailoverToMigrating(t, "web-1", "host-A", &issued)

	require.NoError(t, dist.Undistribute(context.Background()))

	found := false
	for _, e := range issued {
		if e.host == "host-A" && indexOf(e.cmd, "rm -f 'web-1'") >= 0 {
			found = true
		}
	}
	assert.True(t, found,
		"DI3-DEEP: Undistribute must tear down a StateMigrating container "+
			"(it was deployed and may still be alive on its host) instead of "+
			"silently orphaning it — a §11.4.69 sink-side leak; issued=%v", issued)
}

// TestWave20_DI3DEEP_RelocateTearsDownMigratingOnOldHost is the guard for the
// symmetric sibling site (§11.4.120): a re-Distribute that relocates a
// StateMigrating container from host-A to host-B must still rm -f it on the OLD
// host-A. reconcileRelocations' `old.State != StateRunning` skip left a
// StateMigrating old placement stranded (and, if host-A recovered, DUPLICATED
// across A and B). Surgical revert (drop StateMigrating from
// stateNeedsTeardown) -> host-A never torn down -> FAIL.
func TestWave20_DI3DEEP_RelocateTearsDownMigratingOnOldHost(t *testing.T) {
	var issued []struct{ host, cmd string }
	dist := distributeWithFailoverToMigrating(t, "movesvc", "host-A", &issued)

	// Relocate movesvc A -> B (a fresh scheduler placing on host-B).
	hostB := "host-B"
	dist.opts.Scheduler = &mockScheduler{batchFunc: placeOn(&hostB)}
	summary, err := dist.Distribute(context.Background(),
		[]scheduler.ContainerRequirements{{Name: "movesvc", Image: "nginx"}})
	require.NoError(t, err)
	require.Equal(t, 1, summary.RemoteContainers)

	foundOldTeardown := false
	for _, e := range issued {
		if e.host == "host-A" && indexOf(e.cmd, "rm -f 'movesvc'") >= 0 {
			foundOldTeardown = true
		}
	}
	assert.True(t, foundOldTeardown,
		"DI3-DEEP: relocating a StateMigrating container A->B must rm -f it on "+
			"the OLD host-A, not strand (or duplicate) it; issued=%v", issued)
}
