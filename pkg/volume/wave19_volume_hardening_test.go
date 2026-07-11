package volume

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-19 volume-hardening regression guards (CT-HARDEN-EGVOL-1/2/3).
//
// Polarity (§11.4.115): each guard is a STANDING GREEN assertion of the FIXED
// behavior. The artifact IS the polarity oracle — GREEN on the fixed source,
// RED when the corresponding fix is surgically reverted out of the tree (the
// EGVOL-2 guard additionally uses the `-race` detector as its oracle). No
// separate happy-path test; the bug-catcher IS the permanent regression guard.

// TestWave19_EGVOL1_MountDuplicateTOCTOU is the CT-HARDEN-EGVOL-1 guard.
//
// DEFECT (RED on the pre-fix manager.go): Mount() dropped m.mu after the
// "already exists" check and re-took it only at the map insert, with the
// blocking remote mount in between. Two concurrent Mount() calls for the SAME
// name both passed the (still-empty) check and both ran the real remote mount —
// the uniqueness invariant was silently unenforced (a check-then-act TOCTOU:
// the lock released before the protected op completed).
//
// The executor gates each `mkdir` (issued only AFTER the exists-check passes)
// on a barrier that releases once TWO goroutines have arrived; a lone caller
// falls through after a fixed timeout. On the BROKEN artifact both goroutines
// reach the executor (mkdir arrivals == 2) and both Mount() succeed. On the
// FIXED artifact the reservation rejects the second caller with "already
// exists" BEFORE it issues any command (mkdir arrivals == 1), so exactly one
// Mount() succeeds. Deterministic: barrier + timeout, no sleeps racing.
func TestWave19_EGVOL1_MountDuplicateTOCTOU(t *testing.T) {
	exec := &mockExecutor{}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {
				Name: "host-1", Address: "10.0.0.1",
				User: "deploy", Port: 22,
			},
		},
	}
	mgr := NewVolumeManager(hm, exec, logging.NopLogger{})

	var mkdirArrivals int32
	bothPassed := make(chan struct{})
	var once sync.Once
	exec.executeFunc = func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		// Only the remote mount's first command (mkdir) runs past the
		// exists-check; gate it so both callers-past-the-check rendezvous.
		if strings.HasPrefix(strings.TrimSpace(cmd), "mkdir") {
			if atomic.AddInt32(&mkdirArrivals, 1) >= 2 {
				once.Do(func() { close(bothPassed) })
			}
			select {
			case <-bothPassed:
			case <-time.After(2 * time.Second):
				// Fixed code: the lone caller past the check falls
				// through (the other was rejected before issuing mkdir).
			}
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}

	mount := VolumeMount{
		Name: "dup", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = mgr.Mount(context.Background(), mount)
		}(i)
	}
	wg.Wait()

	successes := 0
	rejections := 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case strings.Contains(e.Error(), "already exists"):
			rejections++
		}
	}
	t.Logf("mkdir arrivals (goroutines past the exists-check): %d",
		atomic.LoadInt32(&mkdirArrivals))
	t.Logf("Mount results: err[0]=%v err[1]=%v ; successes=%d",
		errs[0], errs[1], successes)

	if successes != 1 {
		t.Fatalf("EGVOL-1 TOCTOU: duplicate-prevention guard defeated — "+
			"%d concurrent Mount(%q) calls succeeded (both ran the real "+
			"remote mount on the same host+path); want exactly 1",
			successes, mount.Name)
	}
	if rejections != 1 {
		t.Fatalf("EGVOL-1: want exactly one \"already exists\" rejection; "+
			"got %d (errs: %v)", rejections, errs)
	}
}

// TestWave19_EGVOL2_RsyncFlagsAliasRace is the CT-HARDEN-EGVOL-2 guard.
//
// DEFECT (RED under -race on the pre-fix rsync.go): `flags := r.opts.RsyncFlags`
// shares the option slice's backing array; when RsyncFlags has spare capacity
// (cap > len), `append(flags, "--dry-run")` writes in place into backing[len],
// mutating memory shared across every concurrent Sync on the single shared
// RsyncSyncer. Two concurrent read-only Syncs race on that slot (write at the
// append, read at strings.Join).
//
// The `-race` detector IS the polarity oracle (§11.4.107(2)/§11.4.115): RED
// (DATA RACE) on the broken artifact, GREEN on the per-call-copy fix. Honest
// boundary (per the deliverable): the race is armed ONLY by a caller-supplied
// spare-capacity RsyncFlags — the library default []string{"-avz","--delete"}
// has cap==len==2 and reallocates, so it does not race.
func TestWave19_EGVOL2_RsyncFlagsAliasRace(t *testing.T) {
	// --- sub-case A: concurrent read-only Syncs, -race is the oracle -----
	t.Run("ConcurrentReadOnlySyncNoRace", func(t *testing.T) {
		// Spare-capacity flags (cap 8 > len 2) arm the aliasing append.
		rf := make([]string, 2, 8)
		rf[0], rf[1] = "-avz", "--delete"

		exec := &mockExecutor{} // default: exit 0, no capture (no test-side race)
		hm := &mockHostManager{
			hosts: map[string]remote.RemoteHost{
				"host-1": {
					Name: "host-1", Address: "10.0.0.1",
					User: "deploy", Port: 22,
				},
			},
		}
		mgr := NewVolumeManager(
			hm, exec, logging.NopLogger{}, WithRsyncFlags(rf),
		)
		ctx := context.Background()

		// Distinct RemotePath per mount name (VOL-HIGH-4 reconcile): the
		// manager now rejects a Mount whose (HostName, RemotePath) collides
		// with an already-registered mount under a different name — "ro-a"
		// and "ro-b" sharing host-1:/b would be rejected outright. A
		// distinct path per name preserves this test's intent (two
		// independent mounts, synced concurrently, no aliasing race).
		for _, name := range []string{"ro-a", "ro-b"} {
			require.NoError(t, mgr.Mount(ctx, VolumeMount{
				Name: name, Type: MountRsync,
				LocalPath: "/a", RemotePath: "/b-" + name,
				HostName: "host-1", ReadOnly: true,
			}))
		}

		const iters = 200
		var wg sync.WaitGroup
		wg.Add(2)
		for _, name := range []string{"ro-a", "ro-b"} {
			go func(n string) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					_ = mgr.Sync(ctx, n)
				}
			}(name)
		}
		wg.Wait()
	})

	// --- sub-case B: the read-only rsync command is well-formed ----------
	// Single-threaded (no test-side race): capture the generated rsync
	// command and assert the per-call copy still yields the correct flags,
	// and that repeated read-only Syncs do not corrupt the shared option
	// slice (the base flags never gain a stray "--dry-run" of their own).
	t.Run("ReadOnlyArgsWellFormed", func(t *testing.T) {
		rf := make([]string, 2, 8)
		rf[0], rf[1] = "-avz", "--delete"

		var rsyncCmds []string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			if strings.HasPrefix(strings.TrimSpace(cmd), "rsync ") {
				rsyncCmds = append(rsyncCmds, cmd)
			}
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		syncer := NewRsyncSyncer(exec, logging.NopLogger{}, ApplyOptions(
			[]Option{WithRsyncFlags(rf)},
		))

		roMount := VolumeMount{
			Name: "ro", Type: MountRsync,
			LocalPath: "/a", RemotePath: "/b",
			HostName: "test-host", ReadOnly: true,
		}
		// Two sequential read-only syncs: neither may use --dry-run (see
		// VOL-HIGH-3 — --dry-run transfers nothing, so a ReadOnly Sync
		// reported success while never populating the remote directory) nor
		// --delete (read-only protection is destination write-protection,
		// not a pruning transfer); the shared RsyncFlags must never
		// accumulate/corrupt.
		require.NoError(t, syncer.Sync(context.Background(), testHost(), roMount))
		require.NoError(t, syncer.Sync(context.Background(), testHost(), roMount))

		require.Len(t, rsyncCmds, 2)
		for _, cmd := range rsyncCmds {
			t.Logf("read-only rsync command: %s", cmd)
			require.Contains(t, cmd, "-avz")
			require.NotContains(t, cmd, "--delete",
				"VOL-HIGH-3: read-only rsync must omit --delete: %q", cmd)
			require.NotContains(t, cmd, "--dry-run",
				"VOL-HIGH-3: read-only rsync must NOT use --dry-run (it "+
					"transfers nothing): %q", cmd)
		}
		// The option slice itself must be untouched by the per-call copy.
		require.Equal(t, []string{"-avz", "--delete"}, rf,
			"EGVOL-2: RsyncFlags backing slice was mutated by Sync")
	})
}

// TestWave19_EGVOL3_NFSReadOnlyCommandWellFormed is the CT-HARDEN-EGVOL-3 guard.
//
// DEFECT (RED on the pre-fix nfs.go): a read-only NFS mount welded `ro` onto
// the -t filesystem type (`mount -t nfs,ro ...`). util-linux parses "nfs,ro"
// as a comma-separated type list and rejects it with "unknown filesystem type
// 'ro'", so every read-only NFS mount is categorically broken. The captured
// argv from the fake runner is the oracle: the fixed command passes `ro` via
// `-o` and keeps a clean `-t nfs` type.
func TestWave19_EGVOL3_NFSReadOnlyCommandWellFormed(t *testing.T) {
	var gotCmd string
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		if strings.HasPrefix(strings.TrimSpace(cmd), "mount ") {
			gotCmd = cmd
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}}
	// testMountOptionsWithAddress (see VOL-HIGH-1): a configured
	// LocalHostAddress is required for NFSMounter.Mount to proceed past its
	// VOL-HIGH-1 fail-loud check to the mount command this test inspects.
	mounter := NewNFSMounter(exec, logging.NopLogger{}, testMountOptionsWithAddress())

	mount := VolumeMount{
		Name: "nfs-ro", Type: MountNFS,
		LocalPath: "/exp", RemotePath: "/mnt",
		HostName: "test-host", ReadOnly: true,
	}
	require.NoError(t, mounter.Mount(context.Background(), testHost(), mount))
	t.Logf("generated NFS read-only mount command: %s", gotCmd)

	if strings.Contains(gotCmd, "nfs,ro") {
		t.Fatalf("EGVOL-3 malformed read-only NFS command: %q welds 'ro' "+
			"onto the -t type (`-t nfs,ro`); util-linux rejects it as "+
			"`unknown filesystem type 'ro'`. 'ro' must be passed via -o", gotCmd)
	}
	if !strings.Contains(gotCmd, "-o ro") {
		t.Fatalf("EGVOL-3: read-only NFS mount must pass ro via -o; got %q", gotCmd)
	}
	if !strings.Contains(gotCmd, "-t nfs") {
		t.Fatalf("EGVOL-3: read-only NFS mount must keep a clean -t nfs type; got %q", gotCmd)
	}

	// Read-write NFS (ReadOnly:false) is unaffected: valid `mount -t nfs ...`,
	// no -o ro. Proves the fix is path-specific, not a blanket rewrite.
	gotCmd = ""
	rwMount := mount
	rwMount.Name, rwMount.ReadOnly = "nfs-rw", false
	require.NoError(t, mounter.Mount(context.Background(), testHost(), rwMount))
	t.Logf("generated NFS read-write mount command: %s", gotCmd)
	require.Contains(t, gotCmd, "-t nfs")
	require.NotContains(t, gotCmd, "-o ro")
}
