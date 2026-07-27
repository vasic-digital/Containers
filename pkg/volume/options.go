package volume

import "time"

// Option configures volume management behavior.
type Option func(*MountOptions)

// ApplyOptions builds MountOptions from defaults and given options.
func ApplyOptions(opts []Option) MountOptions {
	o := DefaultMountOptions()
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithSSHFSOptions sets extra sshfs flags.
func WithSSHFSOptions(flags []string) Option {
	return func(o *MountOptions) { o.SSHFSOptions = flags }
}

// WithNFSExportOptions sets NFS export options.
func WithNFSExportOptions(opts string) Option {
	return func(o *MountOptions) { o.NFSExportOptions = opts }
}

// WithRsyncFlags sets extra rsync flags.
func WithRsyncFlags(flags []string) Option {
	return func(o *MountOptions) { o.RsyncFlags = flags }
}

// WithSyncInterval sets the rsync synchronization period.
func WithSyncInterval(d time.Duration) Option {
	return func(o *MountOptions) { o.SyncInterval = d }
}

// WithLocalHostAddress sets this orchestrator's own address (or
// "[user@]host"), as reachable FROM every remote host that mounts a local
// path via NFS or SSHFS. Required for NFSMounter/SSHFSMounter to build a
// valid mount source; without it those mounters fail loudly rather than
// build a command from LocalPath alone (VOL-HIGH-1 / VOL-HIGH-2).
func WithLocalHostAddress(addr string) Option {
	return func(o *MountOptions) { o.LocalHostAddress = addr }
}

// WithCommandTimeout sets a local per-call deadline the DefaultVolumeManager
// applies to Mount/Unmount/Sync's ctx before issuing remote commands. Zero
// (the default) leaves the caller's ctx unmodified. Mirrors the local
// deadline every sibling package in this module already applies to remote
// calls (VOL-MED-8).
func WithCommandTimeout(d time.Duration) Option {
	return func(o *MountOptions) { o.CommandTimeout = d }
}
