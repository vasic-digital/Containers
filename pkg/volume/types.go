package volume

import "time"

// MountType identifies the method used to share volumes between
// local and remote hosts.
type MountType string

const (
	// MountSSHFS uses SSHFS to mount local paths on remote hosts.
	MountSSHFS MountType = "sshfs"
	// MountNFS uses NFS to share local paths with remote hosts.
	MountNFS MountType = "nfs"
	// MountRsync uses rsync for periodic synchronization.
	MountRsync MountType = "rsync"
)

// MountState describes the current state of a volume mount.
type MountState string

const (
	// MountMounted means the volume is actively mounted.
	MountMounted MountState = "mounted"
	// MountUnmounted means the volume has been unmounted.
	MountUnmounted MountState = "unmounted"
	// MountSyncing means an rsync operation is in progress.
	MountSyncing MountState = "syncing"
	// MountFailed means the mount operation failed.
	MountFailed MountState = "failed"
	// MountPending means the name is reserved and the mount is in progress.
	// It is inserted under the same lock as the existence check so a
	// concurrent same-name Mount is rejected before running any remote op.
	MountPending MountState = "pending"
)

// VolumeMount describes a volume to share between local and
// remote hosts.
type VolumeMount struct {
	// Name is a unique identifier for this mount.
	Name string
	// Type is the mount method.
	Type MountType
	// LocalPath is the path on the local host.
	LocalPath string
	// RemotePath is the mount point on the remote host.
	RemotePath string
	// HostName is the remote host name.
	HostName string
	// ReadOnly mounts the volume as read-only.
	ReadOnly bool
}

// MountInfo holds the current state of a volume mount.
type MountInfo struct {
	// Mount is the original mount configuration.
	Mount VolumeMount
	// State is the current mount state.
	State MountState
	// MountedAt is when the volume was mounted.
	MountedAt time.Time
	// LastSyncAt is when the last rsync completed (rsync only).
	LastSyncAt time.Time
	// Error holds any error message.
	Error string
}

// MountOptions configures mount behavior per type.
type MountOptions struct {
	// SSHFSOptions are extra flags for sshfs.
	SSHFSOptions []string
	// NFSExportOptions are options for NFS exports.
	NFSExportOptions string
	// RsyncFlags are extra flags for rsync.
	RsyncFlags []string
	// SyncInterval is the period between rsync syncs.
	SyncInterval time.Duration
}

// DefaultMountOptions returns sensible defaults.
func DefaultMountOptions() MountOptions {
	return MountOptions{
		SSHFSOptions:     []string{"-o", "reconnect,ServerAliveInterval=15"},
		NFSExportOptions: "rw,sync,no_subtree_check",
		RsyncFlags:       []string{"-avz", "--delete"},
		SyncInterval:     30 * time.Second,
	}
}
