//go:build !integration

package distribution

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/network"
	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/runtime"
	"digital.vasic.containers/pkg/scheduler"
	"digital.vasic.containers/pkg/volume"
)

// wave20_di2hard_test.go — CT-HARDEN-DIST2HARD (Wave-20 continuation)
// §11.4.115 GREEN-polarity regression guards. Each guard was proven genuine
// by a SURGICAL REVERT of its fix in distributor.go / failover.go (edit the
// fix OUT -> run the guard -> observe a real `--- FAIL` -> edit the fix back
// IN -> GREEN); the observed FAIL output is recorded in the batch handoff
// report, not here.
//
// HONEST BOUNDARY (§11.4.107): these are UNIT guards with no live container
// runtime / SSH / sshfs. They prove the tunnel/volume/teardown/health-check
// COMMAND is ISSUED to the correct seam (TunnelManager / VolumeManager /
// LocalRuntime / Executor) in the correct order and that tracked STATE
// reflects it — they do NOT prove a real tunnel/mount/container on a real
// host actually exists. That is the §11.4.108 runtime-signature layer, out
// of scope for a package unit test.

// ---------------------------------------------------------------------------
// DI-1: TunnelManager.CreateTunnel / VolumeManager.Mount were never wired.
// ---------------------------------------------------------------------------

// countingTunnelManager is a network.TunnelManager fake that records every
// CreateTunnel/CloseTunnel call so a guard can assert the distributor drove
// the tunnel seam (§11.4.27: no real ssh is touched).
type countingTunnelManager struct {
	createFunc  func(ctx context.Context, hostName string, spec network.TunnelSpec) (*network.TunnelInfo, error)
	createCalls []network.TunnelSpec
	closeCalls  []string
}

func (m *countingTunnelManager) CreateTunnel(
	ctx context.Context, hostName string, spec network.TunnelSpec,
) (*network.TunnelInfo, error) {
	m.createCalls = append(m.createCalls, spec)
	if m.createFunc != nil {
		return m.createFunc(ctx, hostName, spec)
	}
	return &network.TunnelInfo{
		Spec: spec, HostName: hostName, State: network.TunnelActive,
	}, nil
}

func (m *countingTunnelManager) CloseTunnel(localPort string) error {
	m.closeCalls = append(m.closeCalls, localPort)
	return nil
}

func (m *countingTunnelManager) ListTunnels() []network.TunnelInfo     { return nil }
func (m *countingTunnelManager) CloseAllForHost(hostName string) error { return nil }
func (m *countingTunnelManager) CloseAll() error                       { return nil }

// countingVolumeManager is a volume.VolumeManager fake that records every
// Mount/Unmount call so a guard can assert the distributor drove the volume
// seam (§11.4.27: no real sshfs/nfs/rsync is touched).
type countingVolumeManager struct {
	mountFunc    func(ctx context.Context, mount volume.VolumeMount) error
	mountCalls   []volume.VolumeMount
	unmountCalls []string
}

func (m *countingVolumeManager) Mount(
	ctx context.Context, mount volume.VolumeMount,
) error {
	m.mountCalls = append(m.mountCalls, mount)
	if m.mountFunc != nil {
		return m.mountFunc(ctx, mount)
	}
	return nil
}

func (m *countingVolumeManager) Unmount(ctx context.Context, name string) error {
	m.unmountCalls = append(m.unmountCalls, name)
	return nil
}
func (m *countingVolumeManager) Sync(ctx context.Context, name string) error { return nil }
func (m *countingVolumeManager) Status(name string) (*volume.MountInfo, error) {
	return nil, nil
}
func (m *countingVolumeManager) ListMounts() []volume.MountInfo       { return nil }
func (m *countingVolumeManager) UnmountAll(ctx context.Context) error { return nil }

// TestWave20_DI1_Distribute_WiresTunnelsAndVolumes is the guard for DI-1: a
// remote container with declared Ports, a configured TunnelManager, and a
// configured VolumeManager+VolumeMountsFor must have CreateTunnel and Mount
// ISSUED to those seams after it starts, with TunnelPorts/VolumeMounts
// populated on the tracked DistributedContainer. Surgical revert (delete the
// wireTunnelsAndVolumes call in deployContainer) -> neither seam is ever
// called -> FAIL.
func TestWave20_DI1_Distribute_WiresTunnelsAndVolumes(t *testing.T) {
	host := "remote-1"
	tm := &countingTunnelManager{}
	vm := &countingVolumeManager{}

	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(&mockExecutor{}),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u", Runtime: "docker"},
		}}),
		WithTunnelManager(tm),
		WithVolumeManager(vm),
		WithVolumeMountsFor(func(req scheduler.ContainerRequirements, hostName string) []volume.VolumeMount {
			return []volume.VolumeMount{
				{Name: "data-" + req.Name, Type: volume.MountSSHFS, LocalPath: "/local/" + req.Name, RemotePath: "/remote/" + req.Name},
			}
		}),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{
			Name: "svc-a", Image: "nginx",
			Ports: []scheduler.PortMapping{{ContainerPort: 8080}},
		},
	}
	summary, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)
	require.Equal(t, 1, summary.RemoteContainers,
		"the container must have deployed successfully; DI-1 wiring must not "+
			"itself cause a false failure when both seams succeed")

	require.Len(t, tm.createCalls, 1,
		"DI-1: TunnelManager.CreateTunnel must be ISSUED for a remote "+
			"container's declared port")
	assert.Equal(t, "8080", tm.createCalls[0].RemotePort)

	require.Len(t, vm.mountCalls, 1,
		"DI-1: VolumeManager.Mount must be ISSUED for the container's "+
			"configured VolumeMountsFor result")
	assert.Equal(t, "data-svc-a", vm.mountCalls[0].Name)
	assert.Equal(t, "remote-1", vm.mountCalls[0].HostName,
		"an empty mount.HostName must default to the container's placement host")

	status := dist.Status(context.Background())
	require.Len(t, status, 1)
	assert.NotEmpty(t, status[0].TunnelPorts,
		"DI-1: dc.TunnelPorts must be populated by the wired CreateTunnel call")
	assert.Contains(t, status[0].TunnelPorts, "8080")
	assert.Equal(t, []string{"data-svc-a"}, status[0].VolumeMounts,
		"DI-1: dc.VolumeMounts must be populated by the wired Mount call")
}

// TestWave20_DI1_TunnelFailure_FailsDeployAndTearsDownContainer is the guard
// for DI-1's failure-honesty half: a CreateTunnel error must FAIL the deploy
// (not StateRunning) and the just-started container must be torn down.
// Surgical revert -> the tunnel error is ignored, the container stays
// StateRunning, and no teardown is issued -> FAIL.
func TestWave20_DI1_TunnelFailure_FailsDeployAndTearsDownContainer(t *testing.T) {
	host := "remote-1"
	var removeCalls int
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, h remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			if containsRmDashF(cmd) {
				removeCalls++
			}
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}

	tm := &countingTunnelManager{
		createFunc: func(ctx context.Context, hostName string, spec network.TunnelSpec) (*network.TunnelInfo, error) {
			return nil, assert.AnError
		},
	}

	dist := NewDistributor(
		WithScheduler(&mockScheduler{batchFunc: placeOn(&host)}),
		WithExecutor(exec),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u", Runtime: "podman"},
		}}),
		WithTunnelManager(tm),
		WithLogger(logging.NopLogger{}),
	)

	reqs := []scheduler.ContainerRequirements{
		{
			Name: "svc-b", Image: "nginx",
			Ports: []scheduler.PortMapping{{ContainerPort: 9090}},
		},
	}
	summary, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)
	require.Equal(t, 1, summary.FailedContainers,
		"DI-1: a tunnel-creation failure must FAIL the deploy, not count as "+
			"a successful remote deploy")
	assert.Equal(t, 0, summary.RemoteContainers)
	require.Len(t, summary.Containers, 1)
	assert.Equal(t, StateFailed, summary.Containers[0].State)
	assert.Contains(t, summary.Containers[0].Error, "create tunnel")

	// removeCalls counts the deploy-time pre-deploy rm PLUS the post-tunnel-
	// failure teardown rm: 2 total. A single rm (deploy-time only) means the
	// teardown-on-failure step never ran.
	assert.Equal(t, 2, removeCalls,
		"DI-1: a tunnel failure must tear down the container that was just "+
			"started (deploy-time rm + teardown rm), not leave it running")
}

// containsRmDashF is a tiny local helper avoiding a package-level strings
// import collision with other test files in this package.
func containsRmDashF(cmd string) bool {
	return len(cmd) > 0 && (indexOf(cmd, "rm -f") >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// DI-2: reconcileRelocations' same-host guard wrongly skipped teardown for a
// round that short-circuited BEFORE deployContainer.
// ---------------------------------------------------------------------------

// TestWave20_DI2_ShortCircuitedRoundStillTearsDownStaleContainer is the guard
// for DI-2: round 1 deploys "svc-a" to host-A; round 2 re-Distributes with a
// Score=0 decision that STILL names host-A (short-circuited before
// deployContainer ever runs); round 3 moves it to host-B. The stale host-A
// instance must be torn down (issued during round 2's reconcile), and
// Undistribute must not be defeated by the interleaved StateFailed entry.
// Surgical revert (drop the deployedThisRound guard) -> host-A never gets a
// teardown -> FAIL.
func TestWave20_DI2_ShortCircuitedRoundStillTearsDownStaleContainer(t *testing.T) {
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

	reqs := []scheduler.ContainerRequirements{{Name: "svc-a", Image: "nginx"}}

	// Round 1: real deploy to host-A.
	_, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)

	// Round 2: scheduler reports Score=0 for the SAME recorded host-A —
	// deployContainer is never reached (the short-circuit under test).
	zeroScoreSameHost := &mockScheduler{
		batchFunc: func(ctx context.Context, reqs []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error) {
			decisions := make([]scheduler.PlacementDecision, len(reqs))
			for i, req := range reqs {
				decisions[i] = scheduler.PlacementDecision{
					Requirement: req, HostName: "host-A", Score: 0, Reason: "no capacity",
				}
			}
			return &scheduler.PlacementPlan{Decisions: decisions, HostSnapshots: map[string]*remote.HostResources{}}, nil
		},
	}
	dist.opts.Scheduler = zeroScoreSameHost
	issued = nil
	summary2, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)
	require.Equal(t, 1, summary2.FailedContainers)

	foundStaleTeardown := false
	for _, e := range issued {
		if e.host == "host-A" && indexOf(e.cmd, "rm -f 'svc-a'") >= 0 {
			foundStaleTeardown = true
		}
	}
	require.True(t, foundStaleTeardown,
		"DI-2: a round that short-circuits before deployContainer (Score=0) "+
			"but still names the OLD host must still tear down the truly-"+
			"still-running old container; issued=%v", issued)

	// Round 3: move to host-B — proves Undistribute is not permanently
	// defeated by the interleaved StateFailed entry (the container reaches a
	// genuine StateRunning again and Undistribute can still reach it).
	host = "host-B"
	dist.opts.Scheduler = &mockScheduler{batchFunc: placeOn(&host)}
	summary3, err := dist.Distribute(context.Background(), reqs)
	require.NoError(t, err)
	require.Equal(t, 1, summary3.RemoteContainers)

	issued = nil
	require.NoError(t, dist.Undistribute(context.Background()))
	foundFinalTeardown := false
	for _, e := range issued {
		if e.host == "host-B" && indexOf(e.cmd, "rm -f 'svc-a'") >= 0 {
			foundFinalTeardown = true
		}
	}
	assert.True(t, foundFinalTeardown,
		"Undistribute must still be able to tear down the container after "+
			"the round-2/round-3 history; issued=%v", issued)
}

// ---------------------------------------------------------------------------
// DI-3: HealthCheckAll never checked LOCAL containers.
// ---------------------------------------------------------------------------

// healthCheckLocalRuntime is a runtime.ContainerRuntime fake whose Status
// call is scripted per test, so a guard can assert HealthCheckAll actually
// calls Status() for a local container instead of trusting tracked State.
type healthCheckLocalRuntime struct {
	countingLocalRuntime
	statusFunc func(ctx context.Context, id string) (*runtime.ContainerStatus, error)
	statusIDs  []string
}

func (r *healthCheckLocalRuntime) Status(
	ctx context.Context, id string,
) (*runtime.ContainerStatus, error) {
	r.statusIDs = append(r.statusIDs, id)
	if r.statusFunc != nil {
		return r.statusFunc(ctx, id)
	}
	return &runtime.ContainerStatus{State: runtime.StateRunning}, nil
}

// TestWave20_DI3_HealthCheckAll_ChecksLocalContainer is the guard for DI-3: a
// StateRunning LOCAL container whose real runtime reports it Stopped must
// surface as an error from HealthCheckAll — a crashed local container must
// not be reported healthy forever. Surgical revert (drop the local branch)
// -> LocalRuntime.Status is never called -> errors stays empty -> FAIL.
func TestWave20_DI3_HealthCheckAll_ChecksLocalContainer(t *testing.T) {
	rt := &healthCheckLocalRuntime{
		statusFunc: func(ctx context.Context, id string) (*runtime.ContainerStatus, error) {
			return &runtime.ContainerStatus{State: runtime.StateStopped}, nil
		},
	}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}), // default -> HostName "local"
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "loc-crash", Image: "nginx"},
	})
	require.NoError(t, err)

	errs := dist.HealthCheckAll(context.Background())
	require.Contains(t, errs, "loc-crash",
		"DI-3: a StateRunning local container reported Stopped by the real "+
			"runtime must surface a health-check error")
	require.Contains(t, rt.statusIDs, "loc-crash",
		"DI-3: HealthCheckAll must actually call LocalRuntime.Status for a "+
			"local container")
}

// TestWave20_DI3_HealthCheckAll_LocalHealthy verifies the non-regression
// counterpart: a genuinely-running local container reports no error.
func TestWave20_DI3_HealthCheckAll_LocalHealthy(t *testing.T) {
	rt := &healthCheckLocalRuntime{}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}),
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "loc-ok", Image: "nginx"},
	})
	require.NoError(t, err)

	errs := dist.HealthCheckAll(context.Background())
	assert.Empty(t, errs)
	assert.Contains(t, rt.statusIDs, "loc-ok")
}

// ---------------------------------------------------------------------------
// DI-4: FailoverHandler.CheckAndFailover computed a reschedule plan but never
// applied it — Status() stayed stale (StateMigrating was dead code).
// ---------------------------------------------------------------------------

// TestWave20_DI4_CheckAndFailover_MarksAffectedContainersMigrating is the
// guard for DI-4 (assessed, minimal honest fix): after CheckAndFailover
// proves a host offline, the tracked entries for containers on that host
// must flip from StateRunning to StateMigrating so Status() is no longer
// stale. Surgical revert (drop the state-fixup block) -> Status() keeps
// reporting StateRunning on a proven-offline host -> FAIL.
func TestWave20_DI4_CheckAndFailover_MarksAffectedContainersMigrating(t *testing.T) {
	dist := NewDistributor(
		WithScheduler(&mockScheduler{
			batchFunc: func(ctx context.Context, reqs []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error) {
				decisions := make([]scheduler.PlacementDecision, len(reqs))
				for i, req := range reqs {
					decisions[i] = scheduler.PlacementDecision{
						Requirement: req, HostName: "node-offline", Score: 0.7, Reason: "test",
					}
				}
				return &scheduler.PlacementPlan{Decisions: decisions, HostSnapshots: map[string]*remote.HostResources{}}, nil
			},
		}),
		WithExecutor(&mockExecutor{
			reachableFunc: func(ctx context.Context, host remote.RemoteHost) bool { return false },
		}),
		WithHostManager(&mockHostManager{hosts: map[string]remote.RemoteHost{
			"node-offline": {Name: "node-offline", Address: "10.0.0.9", User: "u", Runtime: "docker"},
		}}),
		WithLogger(logging.NopLogger{}),
	)

	_, err := dist.Distribute(context.Background(), []scheduler.ContainerRequirements{
		{Name: "web-1", Image: "nginx"},
	})
	require.NoError(t, err)

	before := dist.Status(context.Background())
	require.Len(t, before, 1)
	require.Equal(t, StateRunning, before[0].State,
		"precondition: the container must be tracked StateRunning before failover")

	fh := NewFailoverHandler(dist)
	actions, err := fh.CheckAndFailover(context.Background())
	require.NoError(t, err)
	require.Len(t, actions, 1)

	after := dist.Status(context.Background())
	require.Len(t, after, 1)
	assert.Equal(t, StateMigrating, after[0].State,
		"DI-4: CheckAndFailover must flip the affected container's tracked "+
			"state so Status() is not stale on a host it just proved offline")
}

// ---------------------------------------------------------------------------
// DI-5: DistributeEndpoints discarded the genuinely-deployed count on a
// ctx-cancel error from Distribute (DIST-4).
// ---------------------------------------------------------------------------

// cancelAfterFirstStartRuntime cancels a REAL context.CancelFunc after its
// first Start call succeeds, modelling "ctx cancelled mid-batch" using a real
// runtime.ContainerRuntime.Start seam rather than a fabricated ctx.Err().
type cancelAfterFirstStartRuntime struct {
	*countingLocalRuntime
	cancel context.CancelFunc
	calls  int
}

func (r *cancelAfterFirstStartRuntime) Start(
	ctx context.Context, id string, opts ...runtime.StartOption,
) error {
	r.calls++
	err := r.countingLocalRuntime.Start(ctx, id, opts...)
	if r.calls == 1 {
		r.cancel()
	}
	return err
}

// TestWave20_DI5_DistributeEndpoints_CountsDeployedBeforeCancel is the guard
// for DI-5: DistributeEndpoints must return the genuinely-deployed count
// (not 0) alongside a ctx-cancel error, since DIST-4 guarantees containers
// deployed before cancellation are real StateRunning successes. Surgical
// revert (restore the unconditional `if err != nil { return 0, err }`) ->
// count reports 0 despite one real deploy -> FAIL.
func TestWave20_DI5_DistributeEndpoints_CountsDeployedBeforeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &cancelAfterFirstStartRuntime{
		countingLocalRuntime: &countingLocalRuntime{}, cancel: cancel,
	}
	dist := NewDistributor(
		WithScheduler(&mockScheduler{}), // default -> HostName "local" for every req
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)

	count, err := dist.DistributeEndpoints(ctx, []string{"svc-a", "svc-b"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, count,
		"DI-5: DistributeEndpoints must count the container that genuinely "+
			"deployed before ctx was cancelled, not undercount a live "+
			"container to the boot manager as 0")
}

// TestWave20_DI5_DistributeEndpoints_NilSummaryStillZero verifies the
// non-regression counterpart: a summary-less error (no scheduler configured)
// still returns 0, never a spurious non-zero count.
func TestWave20_DI5_DistributeEndpoints_NilSummaryStillZero(t *testing.T) {
	dist := NewDistributor(WithLogger(logging.NopLogger{}))
	count, err := dist.DistributeEndpoints(context.Background(), []string{"svc-a"})
	require.Error(t, err)
	assert.Equal(t, 0, count)
}
