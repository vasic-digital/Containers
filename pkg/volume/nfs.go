package volume

import (
	"context"
	"fmt"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// NFSMounter handles NFS-based volume mounts.
type NFSMounter struct {
	executor remote.RemoteExecutor
	logger   logging.Logger
	opts     MountOptions
}

// NewNFSMounter creates an NFSMounter.
func NewNFSMounter(
	executor remote.RemoteExecutor,
	logger logging.Logger,
	opts MountOptions,
) *NFSMounter {
	return &NFSMounter{
		executor: executor,
		logger:   logger,
		opts:     opts,
	}
}

// Mount creates an NFS mount on the remote host. The remote host
// mounts the NFS export from the local host.
func (m *NFSMounter) Mount(
	ctx context.Context,
	host remote.RemoteHost,
	mount VolumeMount,
) error {
	// Create the remote mount point.
	mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(mount.RemotePath))
	result, err := m.executor.Execute(ctx, host, mkdirCmd)
	if err != nil {
		return fmt.Errorf(
			"create remote dir: %w", err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"create remote dir: exit %d: %s",
			result.ExitCode, result.Stderr,
		)
	}

	// Mount NFS on remote host. `ro` is a mount OPTION and must go under -o;
	// welding it onto the -t filesystem type (`-t nfs,ro`) makes util-linux
	// parse "nfs,ro" as a comma-separated type list and reject it with
	// "unknown filesystem type 'ro'", silently breaking every read-only NFS
	// mount. Read-write mounts (ReadOnly:false) emit a valid `mount -t nfs ...`.
	mountFlags := "-t nfs"
	if mount.ReadOnly {
		mountFlags += " -o ro"
	}
	nfsCmd := fmt.Sprintf(
		"mount %s %s:%s %s",
		mountFlags,
		shellQuote(mount.LocalPath), shellQuote(mount.LocalPath),
		shellQuote(mount.RemotePath),
	)

	m.logger.Info("nfs mount on %s: %s",
		host.Name, nfsCmd,
	)

	result, err = m.executor.Execute(ctx, host, nfsCmd)
	if err != nil {
		return fmt.Errorf(
			"nfs mount on %s: %w", host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"nfs mount on %s: exit %d: %s",
			host.Name, result.ExitCode, result.Stderr,
		)
	}

	return nil
}

// Unmount removes an NFS mount on the remote host.
func (m *NFSMounter) Unmount(
	ctx context.Context,
	host remote.RemoteHost,
	mount VolumeMount,
) error {
	cmd := fmt.Sprintf("umount %s", shellQuote(mount.RemotePath))
	result, err := m.executor.Execute(ctx, host, cmd)
	if err != nil {
		return fmt.Errorf(
			"nfs unmount on %s: %w", host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"nfs unmount on %s: exit %d: %s",
			host.Name, result.ExitCode, result.Stderr,
		)
	}
	return nil
}
