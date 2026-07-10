package volume

import (
	"context"
	"fmt"
	"sync"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// VolumeManager defines the interface for managing remote volume
// mounts.
type VolumeManager interface {
	// Mount creates a volume mount on a remote host.
	Mount(ctx context.Context, mount VolumeMount) error
	// Unmount removes a volume mount.
	Unmount(ctx context.Context, name string) error
	// Sync triggers an immediate rsync for the named mount.
	Sync(ctx context.Context, name string) error
	// Status returns the current state of a mount.
	Status(name string) (*MountInfo, error)
	// ListMounts returns all mounts.
	ListMounts() []MountInfo
	// UnmountAll removes all mounts.
	UnmountAll(ctx context.Context) error
}

// DefaultVolumeManager implements VolumeManager using sshfs, nfs,
// and rsync.
type DefaultVolumeManager struct {
	mu          sync.RWMutex
	mounts      map[string]*MountInfo
	hostManager remote.HostManager
	executor    remote.RemoteExecutor
	opts        MountOptions
	logger      logging.Logger
	sshfs       *SSHFSMounter
	nfs         *NFSMounter
	rsync       *RsyncSyncer
}

// NewVolumeManager creates a DefaultVolumeManager.
func NewVolumeManager(
	hostManager remote.HostManager,
	executor remote.RemoteExecutor,
	logger logging.Logger,
	opts ...Option,
) *DefaultVolumeManager {
	o := ApplyOptions(opts)
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultVolumeManager{
		mounts:      make(map[string]*MountInfo),
		hostManager: hostManager,
		executor:    executor,
		opts:        o,
		logger:      logger,
		sshfs:       NewSSHFSMounter(executor, logger, o),
		nfs:         NewNFSMounter(executor, logger, o),
		rsync:       NewRsyncSyncer(executor, logger, o),
	}
}

// Mount creates a volume mount on a remote host.
func (m *DefaultVolumeManager) Mount(
	ctx context.Context, mount VolumeMount,
) error {
	if mount.Name == "" {
		return fmt.Errorf("mount name cannot be empty")
	}

	m.mu.Lock()
	if _, exists := m.mounts[mount.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf(
			"mount %q already exists", mount.Name,
		)
	}
	m.mu.Unlock()

	host, err := m.hostManager.GetHost(mount.HostName)
	if err != nil {
		return fmt.Errorf(
			"get host %s: %w", mount.HostName, err,
		)
	}
	if host == nil {
		return fmt.Errorf(
			"host %s not found", mount.HostName,
		)
	}

	var mountErr error
	switch mount.Type {
	case MountSSHFS:
		mountErr = m.sshfs.Mount(ctx, *host, mount)
	case MountNFS:
		mountErr = m.nfs.Mount(ctx, *host, mount)
	case MountRsync:
		mountErr = m.rsync.Sync(ctx, *host, mount)
	default:
		return fmt.Errorf(
			"unsupported mount type: %s", mount.Type,
		)
	}

	info := &MountInfo{Mount: mount}
	if mountErr != nil {
		info.State = MountFailed
		info.Error = mountErr.Error()
		m.mu.Lock()
		m.mounts[mount.Name] = info
		m.mu.Unlock()
		return mountErr
	}

	info.State = MountMounted
	m.mu.Lock()
	m.mounts[mount.Name] = info
	m.mu.Unlock()

	m.logger.Info("mounted %s (%s) on %s: %s -> %s",
		mount.Name, mount.Type, mount.HostName,
		mount.LocalPath, mount.RemotePath,
	)
	return nil
}

// Unmount removes a volume mount.
func (m *DefaultVolumeManager) Unmount(
	ctx context.Context, name string,
) error {
	m.mu.Lock()
	info, exists := m.mounts[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("mount %q not found", name)
	}
	m.mu.Unlock()

	host, err := m.hostManager.GetHost(info.Mount.HostName)
	if err != nil {
		return fmt.Errorf("get host %s: %w", info.Mount.HostName, err)
	}
	if host == nil {
		// The host is gone, so the remote unmount cannot run. Report this
		// honestly rather than marking the volume unmounted (it may still be
		// mounted on a host we lost track of) — a false "unmounted" success is
		// a §11.4 bluff, and UnmountAll must be able to surface it.
		return fmt.Errorf("host %s not found", info.Mount.HostName)
	}

	var unmountErr error
	switch info.Mount.Type {
	case MountSSHFS:
		unmountErr = m.sshfs.Unmount(ctx, *host, info.Mount)
	case MountNFS:
		unmountErr = m.nfs.Unmount(ctx, *host, info.Mount)
	}
	if unmountErr != nil {
		// The remote unmount failed (busy, unreachable). Do NOT drop the entry
		// or claim success — record the failure and propagate so UnmountAll and
		// the caller see it.
		m.mu.Lock()
		info.State = MountFailed
		info.Error = unmountErr.Error()
		m.mu.Unlock()
		return fmt.Errorf("unmount %q: %w", name, unmountErr)
	}

	// Success: remove the entry so the name is reusable and the map does not
	// grow unbounded across mount/unmount churn.
	m.mu.Lock()
	delete(m.mounts, name)
	m.mu.Unlock()

	m.logger.Info("unmounted %s", name)
	return nil
}

// Sync triggers an immediate rsync for the named mount.
func (m *DefaultVolumeManager) Sync(
	ctx context.Context, name string,
) error {
	m.mu.RLock()
	info, exists := m.mounts[name]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("mount %q not found", name)
	}

	host, _ := m.hostManager.GetHost(info.Mount.HostName)
	if host == nil {
		return fmt.Errorf(
			"host %s not found", info.Mount.HostName,
		)
	}

	m.mu.Lock()
	info.State = MountSyncing
	m.mu.Unlock()

	err := m.rsync.Sync(ctx, *host, info.Mount)

	m.mu.Lock()
	if err != nil {
		info.State = MountFailed
		info.Error = err.Error()
	} else {
		info.State = MountMounted
		info.Error = ""
	}
	m.mu.Unlock()

	return err
}

// Status returns the current state of a mount.
func (m *DefaultVolumeManager) Status(
	name string,
) (*MountInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info, ok := m.mounts[name]
	if !ok {
		return nil, fmt.Errorf("mount %q not found", name)
	}
	return info, nil
}

// ListMounts returns all mounts.
func (m *DefaultVolumeManager) ListMounts() []MountInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mounts := make([]MountInfo, 0, len(m.mounts))
	for _, info := range m.mounts {
		mounts = append(mounts, *info)
	}
	return mounts
}

// UnmountAll removes all mounts.
func (m *DefaultVolumeManager) UnmountAll(
	ctx context.Context,
) error {
	m.mu.RLock()
	names := make([]string, 0, len(m.mounts))
	for name := range m.mounts {
		names = append(names, name)
	}
	m.mu.RUnlock()

	var firstErr error
	for _, name := range names {
		if err := m.Unmount(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
