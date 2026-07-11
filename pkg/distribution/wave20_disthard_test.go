//go:build !integration

package distribution

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
)

// wave20_disthard_test.go — CT-HARDEN-DIST-HARD (Wave-20) §11.4.115 GREEN-polarity
// regression guards. Each guard was proven genuine by a SURGICAL REVERT of its
// fix in distributor.go (edit the fix OUT → run the guard → observe a real
// `--- FAIL` → edit the fix back IN → GREEN); the observed FAIL output is
// recorded in the batch handoff, not here.
//
// HONEST BOUNDARY (§11.4.107): these are UNIT guards with no live container
// runtime. They prove the teardown / no-op COMMAND is ISSUED to the correct
// seam (Executor / LocalRuntime) in the correct order — they do NOT prove a real
// container on a real host actually died. That is the §11.4.108 runtime-signature
// layer, out of scope for a package unit test.

// countingLocalRuntime is a runtime.ContainerRuntime fake that records the
// Start/Stop/Remove calls issued to it, so a guard can assert the distributor
// drove the local-runtime seam. §11.4.27: no real docker/podman is touched.
type countingLocalRuntime struct {
	startIDs  []string
	stopIDs   []string
	removeIDs []string
	startErr  error
}

func (r *countingLocalRuntime) Name() string                         { return "counting" }
func (r *countingLocalRuntime) IsAvailable(ctx context.Context) bool { return true }
func (r *countingLocalRuntime) Version(ctx context.Context) (string, error) {
	return "1.0", nil
}
func (r *countingLocalRuntime) Start(ctx context.Context, id string, opts ...runtime.StartOption) error {
	r.startIDs = append(r.startIDs, id)
	return r.startErr
}
func (r *countingLocalRuntime) Stop(ctx context.Context, id string, opts ...runtime.StopOption) error {
	r.stopIDs = append(r.stopIDs, id)
	return nil
}
func (r *countingLocalRuntime) Remove(ctx context.Context, id string, opts ...runtime.RemoveOption) error {
	r.removeIDs = append(r.removeIDs, id)
	return nil
}
func (r *countingLocalRuntime) Status(ctx context.Context, id string) (*runtime.ContainerStatus, error) {
	return &runtime.ContainerStatus{State: runtime.StateRunning}, nil
}
func (r *countingLocalRuntime) List(ctx context.Context, f runtime.ListFilter) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (r *countingLocalRuntime) Stats(ctx context.Context, id string) (*runtime.ContainerStats, error) {
	return nil, nil
}
func (r *countingLocalRuntime) Exec(ctx context.Context, id string, cmd []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (r *countingLocalRuntime) Logs(ctx context.Context, id string, opts ...runtime.LogOption) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// placeOn returns a mockScheduler batchFunc that places every requirement on the
// host named by *host (read at call time, so a test can flip placement between
// two Distribute calls to model a relocation).
func placeOn(host *string) func(context.Context, []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error) {
	return func(ctx context.Context, reqs []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error) {
		decisions := make([]scheduler.PlacementDecision, len(reqs))
		for i, req := range reqs {
			decisions[i] = scheduler.PlacementDecision{
				Requirement: req,
				HostName:    *host,
				Score:       0.9,
				Reason:      "wave20",
			}
		}
		return &scheduler.PlacementPlan{
			Decisions:     decisions,
			HostSnapshots: map[string]*remote.HostResources{},
		}, nil
	}
}

// TestWave20_DIST1_Undistribute_IssuesRemoteRemove is the guard for DIST-1
// (remote half): Undistribute() must ISSUE a `rt rm -f <name>` to the Executor
// for every running remote container before it drops tracking — the prior code
// only flipped State to Stopped in memory (§11.4.69 sink-side bluff: State says
// stopped, the host still runs it). Surgical revert (delete the teardown loop in
// Undistribute) → no rm issued → FAIL.
func TestWave20_DIST1_Undistribute_IssuesRemoteRemove(t *testing.T) {
	host := "remote-1"
	var issued []string
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			issued = append(issued, cmd)
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u", Runtime: "podman"},
		}}),
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "svc-a", Image: "nginx"},
	})
	require.NoError(t, err)

	// Isolate the teardown from the deploy-time pre-deploy rm + run.
	issued = nil
	require.NoError(t, dist.Undistribute(context.Background()))

	found := false
	for _, c := range issued {
		// §11.4.120 reconcile: Wave-20 DI3-SITE4 (ARGSWEEP) now shell-quotes the
		// runtime token, so the issued command is `'podman' rm -f 'svc-a' ...`.
		// Assert the security-hardened quoted form (the remote login shell strips
		// the single quotes → resolves to `podman` identically). This still
		// catches the DIST-1 mutation (Undistribute issuing NO remote rm -f).
		if strings.Contains(c, "'podman' rm -f 'svc-a'") {
			found = true
		}
	}
	require.True(t, found,
		"Undistribute must ISSUE a remote rm -f for the tracked container; issued=%v", issued)
}

// TestWave20_DIST1_Undistribute_IssuesLocalStopRemove is the guard for DIST-1
// (local half): a running LOCAL container must be Stop+Remove'd through the
// LocalRuntime seam on Undistribute. Surgical revert → no Stop/Remove issued →
// FAIL.
func TestWave20_DIST1_Undistribute_IssuesLocalStopRemove(t *testing.T) {
	rt := &countingLocalRuntime{}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}), // default → HostName "local"
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "loc-a", Image: "nginx"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"loc-a"}, rt.startIDs, "container must have been deployed locally")

	require.NoError(t, dist.Undistribute(context.Background()))
	assert.Equal(t, []string{"loc-a"}, rt.stopIDs, "Undistribute must Stop the local container")
	assert.Equal(t, []string{"loc-a"}, rt.removeIDs, "Undistribute must Remove the local container")
}

// TestWave20_DIST1_Undistribute_Idempotent proves the DIST-1 teardown keeps
// Undistribute idempotent: a second Undistribute issues NO further teardown.
func TestWave20_DIST1_Undistribute_Idempotent(t *testing.T) {
	rt := &countingLocalRuntime{}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{{Name: "loc-a"}})
	require.NoError(t, err)
	require.NoError(t, dist.Undistribute(context.Background()))
	require.NoError(t, dist.Undistribute(context.Background())) // second call: no-op
	assert.Len(t, rt.stopIDs, 1, "second Undistribute must not re-issue teardown")
	assert.Len(t, rt.removeIDs, 1)
}

// TestWave20_DIST2_RelocateTearsDownOldHost is the guard for DIST-2: a
// re-Distribute moving a container from host-A to host-B must rm -f it on the
// OLD host-A (deployRemote only rm-f's the NEW host). Surgical revert (delete
// the reconcileRelocations call in Distribute) → no host-A teardown → FAIL.
func TestWave20_DIST2_RelocateTearsDownOldHost(t *testing.T) {
	host := "host-A"
	type call struct{ host, cmd string }
	var issued []call
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			issued = append(issued, call{h.Name, cmd})
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"host-A": {Name: "host-A", Address: "10.0.0.1", User: "u", Runtime: "podman"},
			"host-B": {Name: "host-B", Address: "10.0.0.2", User: "u", Runtime: "docker"},
		}}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{{Name: "movesvc", Image: "nginx"}}
	_, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)

	// Relocate movesvc A → B and isolate the reconciliation from deploy-time cmds.
	host = "host-B"
	issued = nil
	_, err = dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)

	foundOldTeardown := false
	for _, e := range issued {
		if e.host == "host-A" && strings.Contains(e.cmd, "rm -f 'movesvc'") {
			foundOldTeardown = true
		}
	}
	require.True(t, foundOldTeardown,
		"re-Distribute moving movesvc A→B must rm -f it on the OLD host A; issued=%v", issued)
}

// TestWave20_DIST2_SameHostNoStaleTeardown proves reconciliation does NOT tear
// down a container that stayed on the same host (deployRemote already rm-f'd it
// in place — a spurious teardown would be wasteful/incorrect).
func TestWave20_DIST2_SameHostNoStaleTeardown(t *testing.T) {
	host := "host-A"
	var teardownOnA int
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	// Count only the pre-deploy rm issued by deployRemote (expected), and prove
	// reconciliation adds none: track total rm-f commands across the 2nd call.
	_ = teardownOnA
	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"host-A": {Name: "host-A", Address: "10.0.0.1", User: "u", Runtime: "podman"},
		}}),
		WithLogger(logging.NopLogger{}),
	)
	reqs := []scheduler.ContainerRequirements{{Name: "staysvc", Image: "nginx"}}
	_, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)

	var rmCount int
	exec.executeFunc = func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
		if strings.Contains(cmd, "rm -f 'staysvc'") {
			rmCount++
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}
	_, err = dist.Distribute(context.Background(), reqs) // same host → no relocation
	require.NoError(t, err)
	// Exactly ONE rm -f: deployRemote's pre-deploy cleanup. Reconciliation must
	// add no second teardown for a same-host placement.
	assert.Equal(t, 1, rmCount,
		"same-host re-Distribute must issue only the deploy-time rm, no stale reconcile teardown")
}

// TestWave20_DIST3_NilLocalRuntimeIsFailure is the guard for DIST-3: a local
// placement with a nil LocalRuntime must be reported as a FAILED container, not
// a silent LocalContainers++ success. Surgical revert (`return nil` in
// deployLocal) → the container counts as running local → FAIL.
func TestWave20_DIST3_NilLocalRuntimeIsFailure(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}), // default → HostName "local"
		WithLogger(logging.NopLogger{}), // NO LocalRuntime
	)
	summary, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "x", Image: "nginx"},
	})
	require.NoError(t, err) // Distribute itself does not error; the container fails
	assert.Equal(t, 0, summary.LocalContainers,
		"a nil LocalRuntime must NOT count as a deployed local container")
	assert.Equal(t, 1, summary.FailedContainers,
		"a nil-runtime local deploy must FAIL, not silently succeed")
	require.Len(t, summary.Containers, 1)
	assert.Equal(t, StateFailed, summary.Containers[0].State)
	assert.Contains(t, summary.Containers[0].Error, "no local runtime configured")
}

// TestWave20_DIST4_CanceledContextIssuesNoDeploy is the guard for DIST-4: once
// the context is cancelled, the deploy loop must issue NO command to any host
// and return the ctx error with every remaining decision marked failed. Surgical
// revert (delete the ctx.Err() check) → the executor IS called → FAIL.
func TestWave20_DIST4_CanceledContextIssuesNoDeploy(t *testing.T) {
	host := "remote-1"
	var calls int
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			calls++
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u", Runtime: "docker"},
		}}),
		WithLogger(logging.NopLogger{}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Distribute runs

	summary, err := dist.Distribute(ctx, []scheduler.ContainerRequirements{
		{Name: "a", Image: "nginx"},
		{Name: "b", Image: "nginx"},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, calls,
		"no remote command may be issued once the context is cancelled")
	assert.Equal(t, 2, summary.FailedContainers)
	assert.Equal(t, 0, summary.RemoteContainers)
	assert.Equal(t, 0, summary.LocalContainers)
}
