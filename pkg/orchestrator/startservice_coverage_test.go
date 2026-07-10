package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/remote"
)

// TestDefaultOrchestrator_StartService_ServiceFound_LocalOrch exercises the
// StartService local code path when the service exists.
func TestDefaultOrchestrator_StartService_ServiceFound_LocalOrch(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	upCalled := false
	orch := &startTestComposeOrch{
		upFunc: func() error {
			upCalled = true
			return nil
		},
	}

	o := New(
		WithLocalOrchestrator(orch),
		WithProjectDir(tmpDir),
	)
	o.AddService(Service{
		Name:        "test-svc-local",
		ComposeFile: composePath,
	})

	err := o.StartService(context.Background(), "test-svc-local")
	assert.NoError(t, err)
	assert.True(t, upCalled)
}

// TestDefaultOrchestrator_StartService_RelativePath exercises relative
// compose file path joining with projectDir.
func TestDefaultOrchestrator_StartService_RelativePath(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	upCalled := false
	orch := &startTestComposeOrch{upFunc: func() error { upCalled = true; return nil }}

	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{
		Name:        "rel-svc",
		ComposeFile: "docker-compose.yml", // relative path
	})

	err := o.StartService(context.Background(), "rel-svc")
	assert.NoError(t, err)
	assert.True(t, upCalled)
}

// TestDefaultOrchestrator_StartService_RemoteEnabled exercises the remote code path.
func TestDefaultOrchestrator_StartService_RemoteEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("version: '3'\n"), 0644))

	executeCalled := false
	remoteExec := &startTestRemoteExec{
		executeFunc: func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			executeCalled = true
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	hostMgr := &startTestHostMgr{
		hosts: []remote.RemoteHost{{Name: "remote-1", Address: "10.0.0.1", User: "u"}},
	}

	o := New(
		WithRemoteExecutor(remoteExec),
		WithHostManager(hostMgr),
		WithProjectDir(tmpDir),
	)
	// Manually set remoteEnabled since New() sets it based on hostMgr/remoteExec
	o.remoteEnabled = true
	o.AddService(Service{
		Name:        "remote-svc",
		ComposeFile: composePath,
	})

	err := o.StartService(context.Background(), "remote-svc")
	// The stub Execute + CopyDir both succeed, so the remote start path must
	// complete without error AND must actually have invoked the remote
	// executor. The previous `executeCalled || err != nil` assertion was a
	// tautology (the remote path always calls Execute first, so executeCalled
	// is always true) — it passed whether or not the remote start worked,
	// §11.4 test-layer anti-bluff.
	require.NoError(t, err)
	assert.True(t, executeCalled, "the remote start path must invoke the remote executor")
}

// startTestComposeOrch is a minimal ComposeOrchestrator mock.
type startTestComposeOrch struct {
	upFunc   func() error
	downFunc func() error
}

func (m *startTestComposeOrch) Up(ctx context.Context, p compose.ComposeProject) error {
	if m.upFunc != nil {
		return m.upFunc()
	}
	return nil
}

func (m *startTestComposeOrch) Down(ctx context.Context, p compose.ComposeProject) error {
	if m.downFunc != nil {
		return m.downFunc()
	}
	return nil
}

// startTestRemoteExec is a RemoteExecutor mock.
type startTestRemoteExec struct {
	executeFunc func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
}

func (m *startTestRemoteExec) Execute(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, host, cmd)
	}
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (m *startTestRemoteExec) CopyDir(ctx context.Context, host remote.RemoteHost, src, dst string) error {
	return nil
}

// startTestHostMgr provides a list of hosts.
type startTestHostMgr struct {
	hosts []remote.RemoteHost
}

func (m *startTestHostMgr) ListHosts() []remote.RemoteHost {
	return m.hosts
}
