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
	// Copy into a fresh, per-call slice before appending. `flags :=
	// r.opts.RsyncFlags` shares the option slice's backing array; appending
	// into it (when RsyncFlags has spare capacity, cap > len) writes in place
	// into backing[len], mutating memory shared across every concurrent Sync on
	// this one RsyncSyncer — a data race under concurrent read-only Syncs (the
	// -race detector flags the write here and the strings.Join read below).
	flags := append([]string(nil), r.opts.RsyncFlags...)
	if mount.ReadOnly {
		// VOL-HIGH-3: rsync's --dry-run flag transfers NOTHING — by rsync's
		// own documented semantics it is a trial run that reports what
		// would happen without touching the destination. The pre-fix code
		// added --dry-run here (comment: "still sync but just flag it",
		// which contradicts rsync's actual behavior), so every ReadOnly
		// Sync reported success (exit 0) while the remote directory was
		// NEVER populated — a permanent §11.4.108 false-success. Read-only
		// protection is instead enforced by (a) omitting --delete so this
		// sync never prunes files that may exist on the destination through
		// another path, and (b) making the destination directory
		// non-writable at the OS level (chmod) AFTER a REAL transfer below
		// — the remote copy is genuinely populated AND write-protected,
		// which is what a caller requesting a read-only volume expects.
		filtered := make([]string, 0, len(flags))
		for _, f := range flags {
			if f == "--delete" {
				continue
			}
			filtered = append(filtered, f)
		}
		flags = filtered
	}

	r.logger.Info("rsync to %s: %s -> %s",
		host.Name, mount.LocalPath, mount.RemotePath,
	)

	// Run rsync via the remote executor. The remote host pulls
	// from the local host using rsync over SSH.
	// NOTE: the source host is host.Address (the remote host itself), so this
	// rsyncs the remote's own LocalPath rather than pushing the local
	// orchestrator's files — a separate tracked defect (needs a local-host
	// identity threaded through config).
	//
	// VOL2-1: host.User and host.Address are interpolated into the remote
	// `user@host:path` spec and MUST be shell-quoted just like the paths.
	// The prior code quoted only mount.LocalPath / mount.RemotePath and passed
	// host.User / host.Address RAW, so its own claim that path quoting "closes
	// the shell-injection / word-splitting vector" was false: a host User or
	// Address carrying a space (word-split → wrong remote spec) or a shell
	// metacharacter (`;`, `$(...)`, backticks → arbitrary command injection on
	// the remote host, which is exactly where this command runs) still reached
	// the remote shell verbatim. These values flow from host config (§6.R),
	// never a request, but an unescaped interpolation into a shell command is a
	// defect regardless of source — mirror the SSHFSMounter address-quoting
	// (VOL-HIGH-2) so the whole `user@host:path` triple is quoted.
	pullCmd := fmt.Sprintf(
		"rsync %s %s@%s:%s/ %s/",
		strings.Join(flags, " "),
		shellQuote(host.User), shellQuote(host.Address),
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

	if mount.ReadOnly {
		// VOL-HIGH-3 (continued): the transfer above is REAL (no
		// --dry-run), so the destination is now genuinely populated.
		// Enforce the caller's read-only request via OS-level
		// write-protection on the destination directory, rather than by
		// skipping the transfer.
		chmodCmd := fmt.Sprintf("chmod -R a-w %s", shellQuote(mount.RemotePath))
		result, err = r.executor.Execute(ctx, host, chmodCmd)
		if err != nil {
			return fmt.Errorf(
				"rsync read-only chmod on %s: %w", host.Name, err,
			)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf(
				"rsync read-only chmod on %s: exit %d: %s",
				host.Name, result.ExitCode, result.Stderr,
			)
		}
	}

	return nil
}
