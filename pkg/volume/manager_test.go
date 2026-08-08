package volume

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// mockExecutor for volume tests.
type mockExecutor struct {
	executeFunc func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
}

func (m *mockExecutor) Execute(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (*remote.CommandResult, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, host, cmd)
	}
	return &remote.CommandResult{ExitCode: 0}, nil
}

func (m *mockExecutor) ExecuteStream(
	ctx context.Context, host remote.RemoteHost, cmd string,
) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockExecutor) CopyFile(
	ctx context.Context, host remote.RemoteHost, local, rmt string,
) error {
	return nil
}

func (m *mockExecutor) CopyDir(
	ctx context.Context, host remote.RemoteHost, local, rmt string,
) error {
	return nil
}

func (m *mockExecutor) IsReachable(
	ctx context.Context, host remote.RemoteHost,
) bool {
	return true
}

// mockHostManager for volume tests.
type mockHostManager struct {
	hosts map[string]remote.RemoteHost
}

func (m *mockHostManager) AddHost(h remote.RemoteHost) error {
	m.hosts[h.Name] = h
	return nil
}

func (m *mockHostManager) RemoveHost(name string) error {
	delete(m.hosts, name)
	return nil
}

func (m *mockHostManager) GetHost(
	name string,
) (*remote.RemoteHost, error) {
	h, ok := m.hosts[name]
	if !ok {
		return nil, nil
	}
	return &h, nil
}

func (m *mockHostManager) ListHosts() []remote.RemoteHost {
	hosts := make([]remote.RemoteHost, 0)
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

func (m *mockHostManager) ProbeHost(
	ctx context.Context, name string,
) (*remote.HostResources, error) {
	return nil, nil
}

func (m *mockHostManager) ProbeAll(
	ctx context.Context,
) map[string]*remote.HostResources {
	return nil
}

func (m *mockHostManager) HostState(
	name string,
) remote.HostState {
	return remote.HostOnline
}

func newTestVolumeManager() (*DefaultVolumeManager, *mockExecutor) {
	exec := &mockExecutor{}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {
				Name: "host-1", Address: "10.0.0.1",
				User: "deploy", Port: 22,
			},
		},
	}
	// WithLocalHostAddress is required for NFSMounter/SSHFSMounter (see
	// VOL-HIGH-1 / VOL-HIGH-2): without a configured local-host identity
	// they now fail loudly instead of emitting a command built from
	// LocalPath alone. The tests through this shared helper exercise
	// unrelated behavior (dedup / execution-error / host lookup / status /
	// etc.) using MountSSHFS/MountNFS as filler types, so a placeholder
	// address keeps them exercising what they were written to exercise.
	mgr := NewVolumeManager(
		hm, exec, logging.NopLogger{}, WithLocalHostAddress("10.0.0.99"),
	)
	return mgr, exec
}

// testMountOptionsWithAddress returns MountOptions with a placeholder
// LocalHostAddress configured (see VOL-HIGH-1 / VOL-HIGH-2), for tests that
// directly construct an NFSMounter/SSHFSMounter and exercise its mkdir/
// mount-command paths rather than the unconfigured-address fail-loud
// behavior itself (that path has its own dedicated guard in
// wave20_vol2hard_test.go).
func testMountOptionsWithAddress() MountOptions {
	return ApplyOptions([]Option{WithLocalHostAddress("10.0.0.99")})
}

func TestDefaultVolumeManager_Mount_SSHFS(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name:       "data",
		Type:       MountSSHFS,
		LocalPath:  "/local/data",
		RemotePath: "/remote/data",
		HostName:   "host-1",
	}

	err := mgr.Mount(context.Background(), mount)
	require.NoError(t, err)

	info, err := mgr.Status("data")
	require.NoError(t, err)
	assert.Equal(t, MountMounted, info.State)
}

func TestDefaultVolumeManager_Mount_NFS(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name:       "nfs-data",
		Type:       MountNFS,
		LocalPath:  "/local/nfs",
		RemotePath: "/remote/nfs",
		HostName:   "host-1",
	}

	err := mgr.Mount(context.Background(), mount)
	require.NoError(t, err)
}

func TestDefaultVolumeManager_Mount_Rsync(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name:       "sync-data",
		Type:       MountRsync,
		LocalPath:  "/local/sync",
		RemotePath: "/remote/sync",
		HostName:   "host-1",
	}

	err := mgr.Mount(context.Background(), mount)
	require.NoError(t, err)
}

func TestDefaultVolumeManager_Mount_EmptyName(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	err := mgr.Mount(context.Background(), VolumeMount{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name cannot be empty")
}

func TestDefaultVolumeManager_Mount_Duplicate(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	err := mgr.Mount(context.Background(), mount)
	require.NoError(t, err)

	err = mgr.Mount(context.Background(), mount)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestDefaultVolumeManager_Mount_HostNotFound(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b",
		HostName: "nonexistent",
	}
	err := mgr.Mount(context.Background(), mount)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDefaultVolumeManager_Mount_UnsupportedType(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name: "data", Type: MountType("ceph"),
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	err := mgr.Mount(context.Background(), mount)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestDefaultVolumeManager_Mount_ExecutionError(t *testing.T) {
	mgr, exec := newTestVolumeManager()
	exec.executeFunc = func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		return nil, fmt.Errorf("connection refused")
	}

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	err := mgr.Mount(context.Background(), mount)
	assert.Error(t, err)

	info, _ := mgr.Status("data")
	assert.Equal(t, MountFailed, info.State)
}

func TestDefaultVolumeManager_Unmount(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	_ = mgr.Mount(context.Background(), mount)

	err := mgr.Unmount(context.Background(), "data")
	require.NoError(t, err)

	// After a successful unmount the entry is removed so the name is reusable
	// and the mounts map does not grow unbounded (DEFECT-1: it previously
	// lingered as MountUnmounted, blocking any re-mount of the same name).
	_, statErr := mgr.Status("data")
	assert.Error(t, statErr)
}

func TestDefaultVolumeManager_Unmount_NotFound(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	err := mgr.Unmount(context.Background(), "nonexistent")
	assert.Error(t, err)
}

func TestDefaultVolumeManager_Sync(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	mount := VolumeMount{
		Name: "data", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	_ = mgr.Mount(context.Background(), mount)

	err := mgr.Sync(context.Background(), "data")
	assert.NoError(t, err)
}

func TestDefaultVolumeManager_Sync_NotFound(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	err := mgr.Sync(context.Background(), "nonexistent")
	assert.Error(t, err)
}

func TestDefaultVolumeManager_ListMounts(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	// Distinct RemotePath per mount (VOL-HIGH-4 reconcile): the manager now
	// rejects a Mount whose (HostName, RemotePath) collides with an
	// already-registered mount under a different name (three different
	// names all sharing host-1:/b would previously silently stack onto one
	// remote directory — exactly the defect VOL-HIGH-4 closes). Using a
	// distinct RemotePath per iteration preserves this test's original
	// intent: three independent, successful mounts, all listed.
	for i := 0; i < 3; i++ {
		mount := VolumeMount{
			Name: fmt.Sprintf("data-%d", i), Type: MountSSHFS,
			LocalPath: "/a", RemotePath: fmt.Sprintf("/b-%d", i),
			HostName: "host-1",
		}
		require.NoError(t, mgr.Mount(context.Background(), mount))
	}

	mounts := mgr.ListMounts()
	assert.Len(t, mounts, 3)
}

func TestDefaultVolumeManager_UnmountAll(t *testing.T) {
	mgr, _ := newTestVolumeManager()

	// Distinct RemotePath per mount (VOL-HIGH-4 reconcile, see
	// TestDefaultVolumeManager_ListMounts above) so all three mounts
	// genuinely register and UnmountAll exercises tearing down three
	// independent mounts, not one.
	for i := 0; i < 3; i++ {
		mount := VolumeMount{
			Name: fmt.Sprintf("data-%d", i), Type: MountSSHFS,
			LocalPath: "/a", RemotePath: fmt.Sprintf("/b-%d", i),
			HostName: "host-1",
		}
		require.NoError(t, mgr.Mount(context.Background(), mount))
	}
	require.Len(t, mgr.ListMounts(), 3)

	err := mgr.UnmountAll(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, mgr.ListMounts())
}
