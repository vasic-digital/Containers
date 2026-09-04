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

// deployLocalRuntime is a minimal runtime mock for deployLocal.
type deployLocalRuntime struct {
	startErr error
}

func (r *deployLocalRuntime) Name() string                         { return "mock" }
func (r *deployLocalRuntime) IsAvailable(ctx context.Context) bool { return true }
func (r *deployLocalRuntime) Version(ctx context.Context) (string, error) {
	return "1.0", nil
}
func (r *deployLocalRuntime) Start(ctx context.Context, id string, opts ...runtime.StartOption) error {
	return r.startErr
}
func (r *deployLocalRuntime) Stop(ctx context.Context, id string, opts ...runtime.StopOption) error {
	return nil
}
func (r *deployLocalRuntime) Remove(ctx context.Context, id string, opts ...runtime.RemoveOption) error {
	return nil
}
func (r *deployLocalRuntime) Status(ctx context.Context, id string) (*runtime.ContainerStatus, error) {
	return &runtime.ContainerStatus{State: runtime.StateRunning}, nil
}
func (r *deployLocalRuntime) List(ctx context.Context, f runtime.ListFilter) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (r *deployLocalRuntime) Stats(ctx context.Context, id string) (*runtime.ContainerStats, error) {
	return nil, nil
}
func (r *deployLocalRuntime) Exec(ctx context.Context, id string, cmd []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (r *deployLocalRuntime) Logs(ctx context.Context, id string, opts ...runtime.LogOption) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestDeployLocal_NilRuntime verifies deployLocal FAILS (does not silently
// succeed) when no local runtime is configured.
//
// §11.4.120 RECONCILED for CT-HARDEN-DIST-HARD DIST-3: this test previously
// asserted deployLocal returns NoError with a nil LocalRuntime — enshrining the
// false-success bluff (`return nil` while nothing deployed). The fix makes a nil
// LocalRuntime an honest deploy failure (mirroring deployRemote's "no remote
// executor configured"), so the assertion is flipped to require the error. The
// negation is still caught: reverting the fix to `return nil` makes assert.Error
// FAIL.
func TestDeployLocal_NilRuntime(t *testing.T) {
	dist := NewDistributor(WithLogger(logging.NopLogger{}))
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nginx"},
		HostName:    "local",
	}
	err := dist.deployLocal(context.Background(), dc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no local runtime configured")
}

// TestDeployLocal_WithRuntime verifies deployLocal calls runtime.Start.
func TestDeployLocal_WithRuntime(t *testing.T) {
	rt := &deployLocalRuntime{}
	dist := NewDistributor(
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nginx"},
		HostName:    "local",
	}
	err := dist.deployLocal(context.Background(), dc)
	assert.NoError(t, err)
}

// TestDeployLocal_RuntimeError exercises the error branch.
func TestDeployLocal_RuntimeError(t *testing.T) {
	rt := &deployLocalRuntime{startErr: assert.AnError}
	dist := NewDistributor(
		WithLocalRuntime(rt),
		WithLogger(logging.NopLogger{}),
	)
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nginx"},
		HostName:    "local",
	}
	err := dist.deployLocal(context.Background(), dc)
	assert.Error(t, err)
}

// TestDeployRemote_NoExecutor verifies deployRemote fails without executor.
func TestDeployRemote_NoExecutor(t *testing.T) {
	dist := NewDistributor(WithLogger(logging.NopLogger{}))
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app"},
		HostName:    "remote-1",
	}
	err := dist.deployRemote(context.Background(), dc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no remote executor")
}

// TestDeployRemote_HostNotFound verifies deployRemote fails if host is unknown.
func TestDeployRemote_HostNotFound(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	exec := &mockExecutor{}
	dist := NewDistributor(
		WithHostManager(hm),
		WithExecutor(exec),
		WithLogger(logging.NopLogger{}),
	)
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nginx"},
		HostName:    "missing-host",
	}
	err := dist.deployRemote(context.Background(), dc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestDeployRemote_Success verifies deployRemote with a valid host and executor.
func TestDeployRemote_Success(t *testing.T) {
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u"},
		},
	}
	execCalled := false
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			execCalled = true
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	dist := NewDistributor(
		WithHostManager(hm),
		WithExecutor(exec),
		WithLogger(logging.NopLogger{}),
	)
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nginx:latest"},
		HostName:    "remote-1",
	}
	err := dist.deployRemote(context.Background(), dc)
	require.NoError(t, err)
	assert.True(t, execCalled)
}

// TestDeployRemote_ExecFails verifies deployRemote handles non-zero exit code.
func TestDeployRemote_ExecFails(t *testing.T) {
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u"},
		},
	}
	exec := &mockExecutor{
		executeFunc: func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "image not found"}, nil
		},
	}
	dist := NewDistributor(
		WithHostManager(hm),
		WithExecutor(exec),
		WithLogger(logging.NopLogger{}),
	)
	dc := &DistributedContainer{
		Requirement: scheduler.ContainerRequirements{Name: "app", Image: "nonexistent"},
		HostName:    "remote-1",
	}
	err := dist.deployRemote(context.Background(), dc)
	assert.Error(t, err)
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (r *deployLocalRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
