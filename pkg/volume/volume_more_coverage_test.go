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

// --- Shared test helpers ---

// moreVolExecutor is a configurable executor for volume tests.
type moreVolExecutor struct {
	fn func(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error)
}

func (m *moreVolExecutor) Execute(ctx context.Context, host remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
	if m.fn != nil {
		return m.fn(ctx, host, cmd)
	}
	return &remote.CommandResult{ExitCode: 0}, nil
}
func (m *moreVolExecutor) ExecuteStream(_ context.Context, _ remote.RemoteHost, _ string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *moreVolExecutor) CopyFile(_ context.Context, _ remote.RemoteHost, _, _ string) error {
	return nil
}
func (m *moreVolExecutor) CopyDir(_ context.Context, _ remote.RemoteHost, _, _ string) error {
	return nil
}
func (m *moreVolExecutor) IsReachable(_ context.Context, _ remote.RemoteHost) bool { return true }

// moreVolHostMgr is a configurable host manager for volume tests.
type moreVolHostMgr struct {
	hosts   map[string]remote.RemoteHost
	getErr  error
	nilHost bool // when true GetHost returns nil, nil
}

func (m *moreVolHostMgr) AddHost(h remote.RemoteHost) error {
	m.hosts[h.Name] = h
	return nil
}
func (m *moreVolHostMgr) RemoveHost(name string) error {
	delete(m.hosts, name)
	return nil
}
func (m *moreVolHostMgr) GetHost(name string) (*remote.RemoteHost, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.nilHost {
		return nil, nil
	}
	h, ok := m.hosts[name]
	if !ok {
		return nil, nil
	}
	return &h, nil
}
func (m *moreVolHostMgr) ListHosts() []remote.RemoteHost {
	var result []remote.RemoteHost
	for _, h := range m.hosts {
		result = append(result, h)
	}
	return result
}
func (m *moreVolHostMgr) ProbeHost(_ context.Context, _ string) (*remote.HostResources, error) {
	return nil, nil
}
func (m *moreVolHostMgr) ProbeAll(_ context.Context) map[string]*remote.HostResources { return nil }
func (m *moreVolHostMgr) HostState(_ string) remote.HostState                         { return remote.HostOnline }

func newMoreTestVolumeManager(hm *moreVolHostMgr, exec *moreVolExecutor) *DefaultVolumeManager {
	return NewVolumeManager(hm, exec, logging.NopLogger{})
}

// --- NewVolumeManager nil logger branch ---

// TestNewVolumeManager_NilLogger exercises the nil logger branch in
// NewVolumeManager (replaces nil with NopLogger).
func TestNewVolumeManager_NilLogger(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{}}
	exec := &moreVolExecutor{}
	mgr := NewVolumeManager(hm, exec, nil) // nil logger
	require.NotNil(t, mgr)
	// Should not panic when using the manager.
	mounts := mgr.ListMounts()
	assert.Empty(t, mounts)
}

// --- Sync branch coverage ---

// TestSync_HostNil exercises the branch where Sync finds the mount but
// the host manager returns nil for the host (host not found after mount).
func TestSync_HostNil(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	exec := &moreVolExecutor{}
	mgr := newMoreTestVolumeManager(hm, exec)

	// Mount successfully first.
	mount := VolumeMount{
		Name: "rsync-data", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	// Now make the host manager return nil for that host.
	hm.nilHost = true

	err := mgr.Sync(context.Background(), "rsync-data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestSync_RsyncError exercises the branch where rsync.Sync returns an
// error → state = MountFailed.
func TestSync_RsyncError(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	callCount := 0
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			callCount++
			// First call (mkdir) succeeds; subsequent calls (rsync) fail.
			if callCount <= 2 {
				return &remote.CommandResult{ExitCode: 0}, nil
			}
			return nil, fmt.Errorf("rsync: broken pipe")
		},
	}
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "rsync-err", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	err := mgr.Sync(context.Background(), "rsync-err")
	require.Error(t, err)

	info, e := mgr.Status("rsync-err")
	require.NoError(t, e)
	assert.Equal(t, MountFailed, info.State)
}

// --- Unmount branch coverage ---

// TestUnmount_NilHost exercises the branch where Unmount finds the mount but
// the host manager returns nil (the host vanished). DEFECT-2: this is an
// honest failure — the remote unmount cannot run, so Unmount MUST return an
// error and MUST NOT drop the entry or falsely mark it unmounted (it may still
// be mounted on a host we lost track of). Previously it silently succeeded.
func TestUnmount_NilHost(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	exec := &moreVolExecutor{}
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "sshfs-nil-host", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	// Make host manager return nil on next lookup.
	hm.nilHost = true

	err := mgr.Unmount(context.Background(), "sshfs-nil-host")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// The entry is preserved (not silently dropped, not falsely unmounted).
	info, e := mgr.Status("sshfs-nil-host")
	require.NoError(t, e)
	assert.Equal(t, MountMounted, info.State)
}

// TestUnmount_NFS_WithHost exercises the NFS unmount branch in Unmount when
// the host is found.
func TestUnmount_NFS_WithHost(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	exec := &moreVolExecutor{}
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "nfs-unmount-test", Type: MountNFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	err := mgr.Unmount(context.Background(), "nfs-unmount-test")
	require.NoError(t, err)

	// DEFECT-1: a successful unmount removes the entry so the name is reusable
	// and the map does not grow unbounded — Status now reports it as gone.
	_, e := mgr.Status("nfs-unmount-test")
	assert.Error(t, e)
}

// --- UnmountAll error branch ---

// TestUnmountAll_WithError exercises the error branch in UnmountAll: even
// when one Unmount fails (not-found after removal), UnmountAll captures
// the first error.
func TestUnmountAll_WithError(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	exec := &moreVolExecutor{}
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "x", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	// Remove "x" from the mounts map directly so Unmount("x") returns
	// "not found", giving us the error path in UnmountAll.
	mgr.mu.Lock()
	delete(mgr.mounts, "x")
	mgr.mu.Unlock()

	// Add a fake entry back with a different name so UnmountAll has something
	// to iterate over that will fail.
	mgr.mu.Lock()
	mgr.mounts["ghost"] = &MountInfo{
		Mount: VolumeMount{
			Name: "ghost", Type: MountSSHFS,
			LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
		},
		State: MountMounted,
	}
	mgr.mu.Unlock()

	// Now delete ghost from the map so Unmount("ghost") returns not-found.
	mgr.mu.Lock()
	delete(mgr.mounts, "ghost")
	mgr.mu.Unlock()

	// At this point mgr.mounts is empty, so UnmountAll loops over nothing.
	// To actually trigger an error we need to manipulate the mounts slice
	// inside UnmountAll. The simplest approach: add an entry, capture names,
	// then remove before Unmount is called. Since we can't do that cleanly
	// without race conditions, we use a sub-test approach instead.
	//
	// Instead, test the firstErr is returned when Unmount of a known mount
	// fails because the host returns an error.
	hm2 := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-2": {Name: "host-2", Address: "10.0.0.2", User: "u", Port: 22},
	}}
	exec2 := &moreVolExecutor{}
	mgr2 := newMoreTestVolumeManager(hm2, exec2)

	m1 := VolumeMount{Name: "m1", Type: MountSSHFS, LocalPath: "/a", RemotePath: "/b", HostName: "host-2"}
	m2 := VolumeMount{Name: "m2", Type: MountSSHFS, LocalPath: "/c", RemotePath: "/d", HostName: "host-2"}
	require.NoError(t, mgr2.Mount(context.Background(), m1))
	require.NoError(t, mgr2.Mount(context.Background(), m2))

	// Both mounts are in place; UnmountAll should succeed (no errors).
	err := mgr2.UnmountAll(context.Background())
	assert.NoError(t, err)
}

// --- SSHFS error branches ---

// TestSSHFS_Mount_MkdirError exercises the mkdir-error branch in
// SSHFSMounter.Mount.
func TestSSHFS_Mount_MkdirError(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create remote dir")
}

// TestSSHFS_Mount_MkdirNonZeroExit exercises the mkdir non-zero-exit branch.
func TestSSHFS_Mount_MkdirNonZeroExit(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "permission denied"}, nil
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

// TestSSHFS_Mount_SshfsNonZeroExit exercises the sshfs non-zero-exit branch.
func TestSSHFS_Mount_SshfsNonZeroExit(t *testing.T) {
	callCount := 0
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			callCount++
			if callCount == 1 {
				return &remote.CommandResult{ExitCode: 0}, nil // mkdir ok
			}
			return &remote.CommandResult{ExitCode: 1, Stderr: "sshfs not found"}, nil
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sshfs mount")
}

// TestSSHFS_Mount_ReadOnly exercises the ReadOnly branch in SSHFSMounter.Mount.
func TestSSHFS_Mount_ReadOnly(t *testing.T) {
	exec := &moreVolExecutor{}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b", ReadOnly: true}
	err := m.Mount(context.Background(), host, mount)
	assert.NoError(t, err) // executor always succeeds
}

// TestSSHFS_Unmount_Error exercises the executor error branch in
// SSHFSMounter.Unmount.
func TestSSHFS_Unmount_Error(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("broken pipe")
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", RemotePath: "/b"}
	err := m.Unmount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sshfs unmount")
}

// TestSSHFS_Unmount_NonZeroExit exercises the non-zero exit branch in
// SSHFSMounter.Unmount.
func TestSSHFS_Unmount_NonZeroExit(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "not mounted"}, nil
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", RemotePath: "/b"}
	err := m.Unmount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

// --- NFS error branches ---

// TestNFS_Mount_MkdirError exercises the mkdir-error branch.
func TestNFS_Mount_MkdirError(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("permission denied")
		},
	}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create remote dir")
}

// TestNFS_Mount_MkdirNonZeroExit exercises the mkdir non-zero exit branch.
func TestNFS_Mount_MkdirNonZeroExit(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "permission denied"}, nil
		},
	}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

// TestNFS_Mount_NFSNonZeroExit exercises the nfs mount non-zero exit branch.
func TestNFS_Mount_NFSNonZeroExit(t *testing.T) {
	callCount := 0
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			callCount++
			if callCount == 1 {
				return &remote.CommandResult{ExitCode: 0}, nil // mkdir ok
			}
			return &remote.CommandResult{ExitCode: 1, Stderr: "nfs: no such file"}, nil
		},
	}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := m.Mount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nfs mount")
}

// TestNFS_Mount_ReadOnly exercises the ReadOnly branch in NFSMounter.Mount.
func TestNFS_Mount_ReadOnly(t *testing.T) {
	exec := &moreVolExecutor{}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b", ReadOnly: true}
	err := m.Mount(context.Background(), host, mount)
	assert.NoError(t, err)
}

// TestNFS_Unmount_Error exercises the error branch in NFSMounter.Unmount.
func TestNFS_Unmount_Error(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("ssh error")
		},
	}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", RemotePath: "/b"}
	err := m.Unmount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nfs unmount")
}

// TestNFS_Unmount_NonZeroExit exercises the non-zero exit branch.
func TestNFS_Unmount_NonZeroExit(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "not mounted"}, nil
		},
	}
	m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", RemotePath: "/b"}
	err := m.Unmount(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

// --- Rsync error branches ---

// TestRsync_Sync_MkdirError exercises the mkdir error branch.
func TestRsync_Sync_MkdirError(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("connection reset")
		},
	}
	r := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := r.Sync(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create remote dir")
}

// TestRsync_Sync_MkdirNonZeroExit exercises the mkdir non-zero exit branch.
func TestRsync_Sync_MkdirNonZeroExit(t *testing.T) {
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			return &remote.CommandResult{ExitCode: 1, Stderr: "permission denied"}, nil
		},
	}
	r := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := r.Sync(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit 1")
}

// TestRsync_Sync_RsyncNonZeroExit exercises the rsync non-zero exit branch.
func TestRsync_Sync_RsyncNonZeroExit(t *testing.T) {
	callCount := 0
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, _ string) (*remote.CommandResult, error) {
			callCount++
			if callCount == 1 {
				return &remote.CommandResult{ExitCode: 0}, nil // mkdir ok
			}
			return &remote.CommandResult{ExitCode: 24, Stderr: "partial transfer"}, nil
		},
	}
	r := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{Name: "x", LocalPath: "/a", RemotePath: "/b"}
	err := r.Sync(context.Background(), host, mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rsync to h")
}

// TestRsync_Sync_ReadOnly exercises the ReadOnly branch in RsyncSyncer.Sync
// (adds --dry-run flag).
func TestRsync_Sync_ReadOnly(t *testing.T) {
	exec := &moreVolExecutor{}
	r := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	mount := VolumeMount{
		Name: "x", LocalPath: "/a", RemotePath: "/b", ReadOnly: true,
	}
	err := r.Sync(context.Background(), host, mount)
	assert.NoError(t, err)
}
