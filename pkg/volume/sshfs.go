package volume

import (
	"context"
	"fmt"
	"strings"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// SSHFSMounter handles SSHFS-based volume mounts.
type SSHFSMounter struct {
	executor remote.RemoteExecutor
	logger   logging.Logger
	opts     MountOptions
}

// NewSSHFSMounter creates an SSHFSMounter.
func NewSSHFSMounter(
	executor remote.RemoteExecutor,
	logger logging.Logger,
	opts MountOptions,
) *SSHFSMounter {
	return &SSHFSMounter{
		executor: executor,
		logger:   logger,
		opts:     opts,
	}
}

// Mount creates an SSHFS mount on the remote host. The remote
// host mounts the local path via reverse SSHFS.
func (m *SSHFSMounter) Mount(
	ctx context.Context,
	host remote.RemoteHost,
	mount VolumeMount,
) error {
	// Create the remote mount point.
	mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(mount.RemotePath))
	result, err := m.executor.Execute(ctx, host, mkdirCmd)
	if err != nil {
		return fmt.Errorf(
			"create remote dir %s: %w", mount.RemotePath, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"create remote dir: exit %d: %s",
			result.ExitCode, result.Stderr,
		)
	}

	// Build sshfs command.
	sshfsArgs := []string{"sshfs"}
	sshfsArgs = append(sshfsArgs, m.opts.SSHFSOptions...)
	if mount.ReadOnly {
		sshfsArgs = append(sshfsArgs, "-o", "ro")
	}

	// The remote host uses sshfs to mount from the local host.
	// This requires the local host to be SSH-accessible from
	// the remote host, or use a reverse tunnel.
	sshfsCmd := fmt.Sprintf("%s %s %s",
		strings.Join(sshfsArgs, " "),
		shellQuote(mount.LocalPath),
		shellQuote(mount.RemotePath),
	)

	m.logger.Info("sshfs mount on %s: %s",
		host.Name, sshfsCmd,
	)

	result, err = m.executor.Execute(ctx, host, sshfsCmd)
	if err != nil {
		return fmt.Errorf(
			"sshfs mount on %s: %w", host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"sshfs mount on %s: exit %d: %s",
			host.Name, result.ExitCode, result.Stderr,
		)
	}

	return nil
}

// Unmount removes an SSHFS mount on the remote host.
func (m *SSHFSMounter) Unmount(
	ctx context.Context,
	host remote.RemoteHost,
	mount VolumeMount,
) error {
	cmd := fmt.Sprintf("fusermount -u %s", shellQuote(mount.RemotePath))
	result, err := m.executor.Execute(ctx, host, cmd)
	if err != nil {
		return fmt.Errorf(
			"sshfs unmount on %s: %w", host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"sshfs unmount on %s: exit %d: %s",
			host.Name, result.ExitCode, result.Stderr,
		)
	}
	return nil
}
