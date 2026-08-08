package volume

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-20 DEEPER (2nd-pass) pkg/volume hardening guards (VOL2-1, VOL2-2).
//
// These cover NEW defects the first Wave-20 pass (VOL-HIGH-1..6 / VOL-MED-8 in
// wave20_vol2hard_test.go, EGVOL-1..3 in wave19_volume_hardening_test.go)
// missed. Deep-sibling filename (no clobber of wave20_vol2hard_test.go).
//
// Polarity (§11.4.115): each guard is a STANDING GREEN assertion of the FIXED
// behavior. The artifact IS the polarity oracle — GREEN on the fixed source,
// RED when the corresponding fix is surgically reverted out of the tree. No
// separate happy-path test; the bug-catcher IS the permanent regression guard.
// Every guard drives ONLY the package's existing fake-executor cmd-capture
// seam (mockExecutor / mockHostManager / testHost) — no real
// NFS/SSHFS/rsync/network (§11.4.27), hermetic.

// TestWave20_VOL2_RsyncHostUserAddressShellQuoted is the VOL2-1 guard.
//
// DEFECT (RED on the pre-fix rsync.go): the rsync pull command is assembled as
//
//	rsync <flags> <host.User>@<host.Address>:<LocalPath>/ <RemotePath>/
//
// The first pass quoted ONLY mount.LocalPath and mount.RemotePath (shellQuote)
// and interpolated host.User and host.Address RAW — while the code's own NOTE
// claimed "The path quoting here closes the shell-injection / word-splitting
// vector." That claim was false: a host User/Address carrying a space
// (word-split → a malformed remote spec) or a shell metacharacter (`;`,
// `$(...)`, backticks → arbitrary command execution on the remote host, which
// is exactly where this command runs via the executor) still reached the
// remote shell verbatim. Same class as the Wave-16 path-injection fix
// (DEFECT-3) and the SSHFS address-quoting fix (VOL-HIGH-2), left unclosed for
// rsync's user@host.
//
// The captured argv from the fake executor is the oracle: on the FIXED source
// the whole `user@host:path` triple is single-quoted; on the pre-fix source
// the user@host is raw.
func TestWave20_VOL2_RsyncHostUserAddressShellQuoted(t *testing.T) {
	var rsyncCmd string
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		if strings.HasPrefix(strings.TrimSpace(cmd), "rsync ") {
			rsyncCmd = cmd
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}}
	syncer := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())

	// A User with a space (word-split vector) and an Address carrying a shell
	// metacharacter sequence (command-injection vector). shellQuote wraps each
	// in single quotes with no embedded single-quote to escape.
	host := remote.RemoteHost{
		Name:    "evil-host",
		User:    "de ploy",
		Address: "1.2.3.4; touch /tmp/pwned",
		Port:    22,
	}
	mount := VolumeMount{
		Name: "vol", Type: MountRsync,
		LocalPath: "/local/data", RemotePath: "/remote/data",
		HostName: "evil-host",
	}
	require.NoError(t, syncer.Sync(context.Background(), host, mount))
	require.NotEmpty(t, rsyncCmd, "sanity: an rsync command must have been issued")
	t.Logf("generated rsync command: %s", rsyncCmd)

	// Load-bearing single-line anchor: the remote spec's user@host@:path must
	// carry the fully-quoted user + address, exactly as shellQuote renders them.
	wantSpec := shellQuote(host.User) + "@" + shellQuote(host.Address) + ":"
	if !strings.Contains(rsyncCmd, wantSpec) {
		t.Fatalf("VOL2-1: rsync remote spec did not shell-quote host.User / "+
			"host.Address — want the quoted triple %q in the command, got: %q "+
			"(pre-fix: user@host interpolated RAW, so a space word-splits the "+
			"spec and a shell metacharacter injects a command on the remote "+
			"host where this rsync runs)", wantSpec, rsyncCmd)
	}

	// Supporting anchor: the RAW (unquoted) `@<address>` injection substring
	// must NOT appear — proves the metacharacter address is inside single
	// quotes, not sitting bare in the command line.
	rawInjection := "@" + host.Address
	if strings.Contains(rsyncCmd, rawInjection) {
		t.Fatalf("VOL2-1: rsync command still contains the RAW unquoted "+
			"host spec %q — the shell would treat `; touch /tmp/pwned` as a "+
			"command separator + injected command: %q", rawInjection, rsyncCmd)
	}
}

// TestWave20_VOL2_SyncRejectsNonRsyncMountType is the VOL2-2 guard.
//
// DEFECT (RED on the pre-fix manager.go): DefaultVolumeManager.Sync switched on
// NOTHING — it called m.rsync.Sync for ANY registered mount type. Calling
// Sync(name) on an SSHFS or NFS mount therefore ran a real rsync push against
// that mount's RemotePath (a live network mountpoint, not a periodically-synced
// copy), flipped State MountSyncing→MountMounted, and returned nil — reporting
// success for a destructive, wrong-type operation (§11.4.108 false-success).
// For an SSHFS mount RemotePath is a fuse mount of the orchestrator's own
// LocalPath, so the stray rsync writes back through the live mount.
//
// The fix rejects a Sync of a non-rsync mount BEFORE mutating state or issuing
// any remote command. Oracle: on the pre-fix source Sync returns nil and issues
// remote commands; on the fixed source it returns an error and issues none.
func TestWave20_VOL2_SyncRejectsNonRsyncMountType(t *testing.T) {
	// newMgr builds a manager whose executor records every issued command, and
	// mounts one volume of the given type. Returns the manager + a pointer to
	// the command log + the command count right after Mount (the baseline a
	// subsequent Sync must not grow for a non-rsync type).
	newMgr := func(t *testing.T, mt MountType) (*DefaultVolumeManager, *[]string) {
		t.Helper()
		var cmds []string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			cmds = append(cmds, cmd)
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		hm := &mockHostManager{
			hosts: map[string]remote.RemoteHost{
				"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
			},
		}
		// WithLocalHostAddress: required for the SSHFS/NFS Mount to proceed past
		// its VOL-HIGH-1/2 fail-loud guard (harmless for rsync).
		mgr := NewVolumeManager(
			hm, exec, logging.NopLogger{}, WithLocalHostAddress("10.0.0.99"),
		)
		mount := VolumeMount{
			Name: "vol", Type: mt,
			LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
		}
		require.NoError(t, mgr.Mount(context.Background(), mount))
		return mgr, &cmds
	}

	rsyncIssued := func(cmds []string) bool {
		for _, c := range cmds {
			if strings.HasPrefix(strings.TrimSpace(c), "rsync ") {
				return true
			}
		}
		return false
	}

	// SSHFS and NFS mounts MUST be rejected by Sync, and no remote command may
	// be issued for them.
	for _, mt := range []MountType{MountSSHFS, MountNFS} {
		t.Run(string(mt)+"_rejected", func(t *testing.T) {
			mgr, cmds := newMgr(t, mt)
			baseline := len(*cmds)

			err := mgr.Sync(context.Background(), "vol")
			if err == nil {
				t.Fatalf("VOL2-2: Sync of a %s mount returned nil — the pre-fix "+
					"Sync ran a real rsync push against the live %s mountpoint "+
					"and reported success (a §11.4.108 false-success for a "+
					"wrong-type destructive operation)", mt, mt)
			}
			assert.Contains(t, err.Error(), "only rsync-type mounts",
				"VOL2-2: want a clear rsync-only rejection, got: %v", err)

			if len(*cmds) != baseline {
				t.Fatalf("VOL2-2: Sync of a %s mount issued %d remote command(s) "+
					"(want 0 — reject BEFORE touching the remote host): %v",
					mt, len(*cmds)-baseline, (*cmds)[baseline:])
			}
			if rsyncIssued((*cmds)[baseline:]) {
				t.Fatalf("VOL2-2: Sync of a %s mount issued an rsync command "+
					"against a live mountpoint", mt)
			}

			// The mount's own state is untouched by the rejected Sync (it was
			// never flipped through MountSyncing).
			info, statErr := mgr.Status("vol")
			require.NoError(t, statErr)
			assert.Equal(t, MountMounted, info.State)
		})
	}

	// Positive control: an rsync mount STILL syncs successfully — proves the
	// guard is type-specific, not a blanket break of Sync.
	t.Run("rsync_still_syncs", func(t *testing.T) {
		mgr, cmds := newMgr(t, MountRsync)
		baseline := len(*cmds)

		require.NoError(t, mgr.Sync(context.Background(), "vol"))
		if !rsyncIssued((*cmds)[baseline:]) {
			t.Fatalf("VOL2-2 over-reach: Sync of an rsync mount issued no rsync "+
				"command — the type guard must permit rsync mounts: %v",
				(*cmds)[baseline:])
		}
		info, statErr := mgr.Status("vol")
		require.NoError(t, statErr)
		assert.Equal(t, MountMounted, info.State)
	})
}
