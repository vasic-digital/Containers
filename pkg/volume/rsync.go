package volume

import (
	"context"
	"fmt"
	"strings"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// RsyncSyncer handles rsync-based volume synchronization.
type RsyncSyncer struct {
	executor remote.RemoteExecutor
	logger   logging.Logger
	opts     MountOptions
}

// NewRsyncSyncer creates a RsyncSyncer.
func NewRsyncSyncer(
	executor remote.RemoteExecutor,
	logger logging.Logger,
	opts MountOptions,
) *RsyncSyncer {
	return &RsyncSyncer{
		executor: executor,
		logger:   logger,
		opts:     opts,
	}
}

// Sync synchronizes a local directory to a remote host using
// rsync over SSH.
func (r *RsyncSyncer) Sync(
	ctx context.Context,
	host remote.RemoteHost,
	mount VolumeMount,
) error {
	// Ensure remote directory exists.
	mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(mount.RemotePath))
	result, err := r.executor.Execute(ctx, host, mkdirCmd)
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

	// Build the rsync command to run locally.
	// rsync pushes from local to remote via SSH.
	flags := r.opts.RsyncFlags
	if mount.ReadOnly {
		// For read-only, we still sync but just flag it.
		flags = append(flags, "--dry-run")
	}

	r.logger.Info("rsync to %s: %s -> %s",
		host.Name, mount.LocalPath, mount.RemotePath,
	)

	// Run rsync via the remote executor. The remote host pulls
	// from the local host using rsync over SSH.
	// NOTE: the source host is host.Address (the remote host itself), so this
	// rsyncs the remote's own LocalPath rather than pushing the local
	// orchestrator's files — a separate tracked defect (needs a local-host
	// identity threaded through config). The path quoting here closes the
	// shell-injection / word-splitting vector regardless of that.
	pullCmd := fmt.Sprintf(
		"rsync %s %s@%s:%s/ %s/",
		strings.Join(flags, " "),
		host.User, host.Address,
		shellQuote(mount.LocalPath),
		shellQuote(mount.RemotePath),
	)

	result, err = r.executor.Execute(ctx, host, pullCmd)
	if err != nil {
		return fmt.Errorf(
			"rsync to %s: %w", host.Name, err,
		)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"rsync to %s: exit %d: %s",
			host.Name, result.ExitCode, result.Stderr,
		)
	}

	return nil
}
