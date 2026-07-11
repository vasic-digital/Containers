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

// withCommandTimeout returns a context bounded by opts.CommandTimeout when
// configured (> 0), so a stalled remote mount/unmount/rsync command is
// cancelled at a local deadline instead of blocking the caller forever —
// mirroring the per-call deadline every sibling package in this module
// (pkg/genymotion, pkg/cuttlefish, pkg/emulator, pkg/orchestrator, pkg/vm)
// already applies to its remote calls (VOL-MED-8). CommandTimeout <= 0 (the
// default) returns ctx unmodified, preserving prior behavior exactly. The
// returned cancel is always non-nil and safe to defer unconditionally.
func (m *DefaultVolumeManager) withCommandTimeout(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	if m.opts.CommandTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.opts.CommandTimeout)
}

// Mount creates a volume mount on a remote host.
func (m *DefaultVolumeManager) Mount(
	ctx context.Context, mount VolumeMount,
) error {
	if mount.Name == "" {
		return fmt.Errorf("mount name cannot be empty")
	}

	cctx, cancel := m.withCommandTimeout(ctx)
	defer cancel()

	// Reserve the name atomically with the existence check. Previously the
	// check and the map insert used two separate m.mu acquisitions with the
	// blocking remote mount in between, so two concurrent Mount() calls for the
	// same name both passed the (still-empty) check and both ran the real
	// remote mount — the uniqueness invariant was silently unenforced under
	// concurrency (a check-then-act TOCTOU, the lock dropped before the
	// protected op completed). Inserting a MountPending placeholder under the
	// SAME lock as the check rejects a concurrent same-name Mount before it can
	// run any remote op.
	info := &MountInfo{Mount: mount, State: MountPending}
	m.mu.Lock()
	if _, exists := m.mounts[mount.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf(
			"mount %q already exists", mount.Name,
		)
	}
	// VOL-HIGH-4: reject a Mount whose (HostName, RemotePath) collides with
	// an already-registered mount under a DIFFERENT name. Deduping only by
	// Name (the check above) let two differently-named mounts stack onto
	// the SAME remote directory on the SAME host — both would issue their
	// own mkdir/mount/sshfs/rsync against that one remote path, and tearing
	// either one down (Unmount deletes its own entry) left the manager
	// believing the path fully reclaimed while the OTHER name's mount was
	// still live on that exact directory. Checked under the SAME lock as
	// the reservation above so this is atomic with the Name uniqueness
	// check, closing the same TOCTOU class EGVOL-1 closed for Name.
	for _, existing := range m.mounts {
		if existing.Mount.HostName == mount.HostName &&
			existing.Mount.RemotePath == mount.RemotePath {
			m.mu.Unlock()
			return fmt.Errorf(
				"mount %q rejected: host %q remote path %q is already "+
					"in use by mount %q",
				mount.Name, mount.HostName, mount.RemotePath,
				existing.Mount.Name,
			)
		}
	}
	m.mounts[mount.Name] = info
	m.mu.Unlock()

	// unreserve releases the reservation on a pre-mount early return, matching
	// the prior "no entry left" behavior of the bad/absent-host and
	// unsupported-type paths. It only deletes OUR reservation (cur == info) so
	// it can never clobber an unrelated later entry under the same name.
	unreserve := func() {
		m.mu.Lock()
		if cur, ok := m.mounts[mount.Name]; ok && cur == info {
			delete(m.mounts, mount.Name)
		}
		m.mu.Unlock()
	}

	host, err := m.hostManager.GetHost(mount.HostName)
	if err != nil {
		unreserve()
		return fmt.Errorf(
			"get host %s: %w", mount.HostName, err,
		)
	}
	if host == nil {
		unreserve()
		return fmt.Errorf(
			"host %s not found", mount.HostName,
		)
	}

	var mountErr error
	switch mount.Type {
	case MountSSHFS:
		mountErr = m.sshfs.Mount(cctx, *host, mount)
	case MountNFS:
		mountErr = m.nfs.Mount(cctx, *host, mount)
	case MountRsync:
		mountErr = m.rsync.Sync(cctx, *host, mount)
	default:
		unreserve()
		return fmt.Errorf(
			"unsupported mount type: %s", mount.Type,
		)
	}

	if mountErr != nil {
		// Keep the reserved entry, flipped to MountFailed (unchanged existing
		// semantics — TestDefaultVolumeManager_Mount_ExecutionError relies on
		// the failed entry remaining queryable via Status).
		m.mu.Lock()
		info.State = MountFailed
		info.Error = mountErr.Error()
		m.mu.Unlock()
		return mountErr
	}

	m.mu.Lock()
	info.State = MountMounted
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
	cctx, cancel := m.withCommandTimeout(ctx)
	defer cancel()

	m.mu.Lock()
	info, exists := m.mounts[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("mount %q not found", name)
	}
	// VOL-HIGH-5: reserve the unmount atomically with the exists-check,
	// under the SAME lock — mirrors Mount()'s MountPending barrier
	// (EGVOL-1). Without this, two concurrent Unmount(name) calls both
	// read the SAME *MountInfo past the exists-check and both proceed to
	// the switch below, issuing the REAL remote unmount command twice
	// concurrently; whichever loses the race finds the volume already torn
	// down (busy / not mounted) and reports a false FAIL for what is, from
	// the caller's perspective, an already cleanly-unmounted volume — a
	// §11.4.108 false-failure mirroring EGVOL-1's false-success. Exactly
	// one caller may drive the real unmount at a time; a second concurrent
	// caller is rejected here, before it issues any remote command.
	if info.State == MountUnmounting {
		m.mu.Unlock()
		return fmt.Errorf("mount %q is already being unmounted", name)
	}
	prevState := info.State
	info.State = MountUnmounting
	m.mu.Unlock()

	// restore reverts the reservation on a pre-remote-op early return
	// (host lookup error/absent), so a legitimate retry sees the mount's
	// real prior state rather than being stuck reporting "already being
	// unmounted" forever. Only touches OUR info (matched by identity), so
	// it can never clobber an unrelated later entry under the same name.
	restore := func() {
		m.mu.Lock()
		if cur, ok := m.mounts[name]; ok && cur == info {
			info.State = prevState
		}
		m.mu.Unlock()
	}

	host, err := m.hostManager.GetHost(info.Mount.HostName)
	if err != nil {
		restore()
		return fmt.Errorf("get host %s: %w", info.Mount.HostName, err)
	}
	if host == nil {
		// The host is gone, so the remote unmount cannot run. Report this
		// honestly rather than marking the volume unmounted (it may still be
		// mounted on a host we lost track of) — a false "unmounted" success is
		// a §11.4 bluff, and UnmountAll must be able to surface it.
		restore()
		return fmt.Errorf("host %s not found", info.Mount.HostName)
	}

	var unmountErr error
	switch info.Mount.Type {
	case MountSSHFS:
		unmountErr = m.sshfs.Unmount(cctx, *host, info.Mount)
	case MountNFS:
		unmountErr = m.nfs.Unmount(cctx, *host, info.Mount)
	case MountRsync:
		// VOL-HIGH-6: rsync mounts have no filesystem attach point to
		// detach — the remote directory is a periodically-synced COPY, not
		// a live mount. The pre-fix switch had no case for MountRsync and
		// no default, so unmountErr silently stayed nil here: Unmount
		// reported success, deleted the tracking entry, and never touched
		// the remote host at all — the remote directory and its full
		// content remained in place while the manager (and every caller)
		// believed the volume gone. Fail loudly instead of silently
		// succeeding, so callers never mistake this for a real teardown.
		unmountErr = fmt.Errorf(
			"rsync volumes are not unmountable via this path: remote "+
				"directory %q on host %q was populated by rsync copy, not "+
				"a filesystem mount, and must be reclaimed independently",
			info.Mount.RemotePath, info.Mount.HostName,
		)
	default:
		unmountErr = fmt.Errorf(
			"unsupported mount type for unmount: %s", info.Mount.Type,
		)
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
	cctx, cancel := m.withCommandTimeout(ctx)
	defer cancel()

	m.mu.RLock()
	info, exists := m.mounts[name]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("mount %q not found", name)
	}

	// VOL2-2: Sync performs an rsync push, which is meaningful ONLY for an
	// rsync-type mount. An SSHFS/NFS mount is a live network filesystem, not a
	// periodically-synced copy — rsyncing into its RemotePath writes THROUGH the
	// live mountpoint (for SSHFS, back onto the orchestrator's own exported
	// LocalPath) while this method flips State to MountSyncing→MountMounted and
	// returns nil, reporting success for an operation that never belonged to
	// that mount type (a §11.4.108 false-success / wrong-operation). The pre-fix
	// Sync called m.rsync.Sync for ANY registered mount type with no type
	// guard. Reject a Sync of a non-rsync mount BEFORE mutating state or issuing
	// any remote command.
	if info.Mount.Type != MountRsync {
		return fmt.Errorf(
			"sync %q: only rsync-type mounts support Sync; mount %q is type %q",
			name, name, info.Mount.Type,
		)
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

	err := m.rsync.Sync(cctx, *host, info.Mount)

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
	// Return a value copy (never the live internal pointer) so a caller
	// polling Status cannot torn-read State/Error while Sync/Unmount mutate
	// the same struct under the write lock, and cannot mutate manager state
	// through the returned pointer. Mirrors ListMounts, which already copies.
	infoCopy := *info
	return &infoCopy, nil
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
