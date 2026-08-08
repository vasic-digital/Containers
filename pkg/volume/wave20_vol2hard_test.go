package volume

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-20 pkg/volume hardening guards (VOL-HIGH-1/2/3/4/5/6, VOL-MED-8).
//
// Polarity (§11.4.115): each guard is a STANDING GREEN assertion of the
// FIXED behavior. The artifact IS the polarity oracle — GREEN on the fixed
// source, RED when the corresponding fix is surgically reverted out of the
// tree. No separate happy-path test; the bug-catcher IS the permanent
// regression guard. Every guard drives ONLY the package's existing
// fake-executor cmd-capture seam (mockExecutor / mockHostManager from
// manager_test.go) — no real NFS/SSHFS/rsync/network (§11.4.27).

// TestWave20_VOLHIGH6_UnmountRsyncFailsLoud is the VOL-HIGH-6 guard.
//
// DEFECT (RED on the pre-fix manager.go): Unmount()'s type switch had a case
// for MountSSHFS and MountNFS but NONE for MountRsync and no default. For a
// MountRsync entry unmountErr stayed nil (Go's zero value), so Unmount
// reported success, deleted the tracking entry, and issued ZERO remote
// commands — the remote directory (populated by rsync) was never touched,
// yet the manager and every caller believed the volume torn down. A §11.4.14
// leak masked as a false success.
func TestWave20_VOLHIGH6_UnmountRsyncFailsLoud(t *testing.T) {
	var callCount int32
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		atomic.AddInt32(&callCount, 1)
		return &remote.CommandResult{ExitCode: 0}, nil
	}}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {
				Name: "host-1", Address: "10.0.0.1",
				User: "deploy", Port: 22,
			},
		},
	}
	mgr := NewVolumeManager(hm, exec, logging.NopLogger{})

	mount := VolumeMount{
		Name: "rsync-vol", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	callsAfterMount := atomic.LoadInt32(&callCount)
	require.Greater(t, callsAfterMount, int32(0),
		"sanity: Mount must issue remote commands (mkdir + rsync)")

	err := mgr.Unmount(context.Background(), "rsync-vol")
	if err == nil {
		t.Fatalf("VOL-HIGH-6: Unmount of a MountRsync entry returned nil " +
			"(silent success) — the pre-fix switch had no case for " +
			"MountRsync and no default, so unmountErr stayed nil, zero " +
			"remote commands ran, the remote directory was never touched, " +
			"and the tracking entry was deleted — a false teardown")
	}
	if !strings.Contains(err.Error(), "not unmountable") {
		t.Fatalf("VOL-HIGH-6: want a clear 'rsync volumes are not "+
			"unmountable via this path' error, got: %v", err)
	}

	callsAfterUnmount := atomic.LoadInt32(&callCount)
	if callsAfterUnmount != callsAfterMount {
		t.Fatalf("VOL-HIGH-6: Unmount issued %d remote command(s) for a "+
			"rsync mount it cannot actually detach; want 0 (fail loud "+
			"BEFORE touching the remote host)",
			callsAfterUnmount-callsAfterMount)
	}

	// The entry is preserved (never silently dropped) and marked failed —
	// callers (and UnmountAll) can see the volume was NOT torn down.
	info, statErr := mgr.Status("rsync-vol")
	require.NoError(t, statErr)
	assert.Equal(t, MountFailed, info.State)
	assert.Contains(t, info.Error, "not unmountable")
}

// TestWave20_VOLHIGH6_UnmountUnknownTypeFailsLoud covers the paired `default`
// case in the same switch: an unrecognized Mount.Type must also fail loud,
// never silently succeed.
func TestWave20_VOLHIGH6_UnmountUnknownTypeFailsLoud(t *testing.T) {
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
		},
	}
	exec := &mockExecutor{}
	mgr := NewVolumeManager(hm, exec, logging.NopLogger{})

	// Insert a mount entry directly with an out-of-band type — Mount()
	// itself already rejects unsupported types, so this reaches the
	// Unmount-side switch's default arm the only way possible: an entry
	// that exists in the map with a type Mount() would never have accepted.
	mgr.mu.Lock()
	mgr.mounts["ceph-vol"] = &MountInfo{
		Mount: VolumeMount{
			Name: "ceph-vol", Type: MountType("ceph"),
			LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
		},
		State: MountMounted,
	}
	mgr.mu.Unlock()

	err := mgr.Unmount(context.Background(), "ceph-vol")
	if err == nil {
		t.Fatalf("VOL-HIGH-6: Unmount of an unrecognized mount type " +
			"returned nil (silent success); want a fail-loud default arm")
	}
	assert.Contains(t, err.Error(), "unsupported mount type")
}

// TestWave20_VOLHIGH4_MountRejectsHostRemotePathCollision is the VOL-HIGH-4
// guard.
//
// DEFECT (RED on the pre-fix manager.go): Mount() deduplicated only by Name.
// Two DIFFERENTLY-named mounts to the identical (HostName, RemotePath) both
// succeeded, both issuing their own mkdir/mount against the SAME remote
// directory. Tearing down either name's mount deleted only that name's
// tracking entry, leaving the manager believing the path was fully
// reclaimed while the OTHER name's mount was still live on that exact
// directory.
func TestWave20_VOLHIGH4_MountRejectsHostRemotePathCollision(t *testing.T) {
	exec := &mockExecutor{}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
		},
	}
	mgr := NewVolumeManager(
		hm, exec, logging.NopLogger{}, WithLocalHostAddress("10.0.0.99"),
	)

	first := VolumeMount{
		Name: "vol-a", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/shared", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), first))

	second := VolumeMount{
		// Different Name, SAME (HostName, RemotePath) as `first`.
		Name: "vol-b", Type: MountSSHFS,
		LocalPath: "/c", RemotePath: "/shared", HostName: "host-1",
	}
	err := mgr.Mount(context.Background(), second)
	if err == nil {
		t.Fatalf("VOL-HIGH-4: a second, differently-named Mount(%q) to the "+
			"identical (host=%s, remote=%s) succeeded — two names now "+
			"stack onto ONE remote directory; unmounting either name would "+
			"leave the manager believing the path fully reclaimed while "+
			"the other name's mount is still live on it",
			second.Name, second.HostName, second.RemotePath)
	}
	if !strings.Contains(err.Error(), "already in use by mount") {
		t.Fatalf("VOL-HIGH-4: want a clear (host, remote-path) collision "+
			"error, got: %v", err)
	}

	// vol-b must never have been registered.
	_, statErr := mgr.Status("vol-b")
	assert.Error(t, statErr)

	// vol-a remains healthy and completely unaffected by the rejection.
	info, statErr := mgr.Status("vol-a")
	require.NoError(t, statErr)
	assert.Equal(t, MountMounted, info.State)

	// A genuinely distinct remote path on the same host is still accepted —
	// proves the fix is collision-specific, not a blanket same-host reject.
	distinct := VolumeMount{
		Name: "vol-c", Type: MountSSHFS,
		LocalPath: "/d", RemotePath: "/not-shared", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), distinct))
}

// TestWave20_VOLHIGH5_UnmountConcurrentDoubleUnmount is the VOL-HIGH-5 guard.
//
// DEFECT (RED on the pre-fix manager.go): Unmount() had no reservation
// (unlike Mount()'s EGVOL-1 MountPending barrier). Two concurrent
// Unmount(name) calls both read the SAME *MountInfo past the exists-check
// and both proceeded to issue the REAL remote unmount command — the loser of
// that race finds the volume already torn down (busy / not mounted) and
// reports a false FAIL for what is, from the caller's perspective, an
// already cleanly-unmounted volume.
//
// Mirrors EGVOL-1's barrier pattern: the executor gates the fusermount
// command on a rendezvous of two arrivals; a lone caller falls through after
// a fixed timeout. On the BROKEN artifact both goroutines reach the
// executor (arrivals == 2); on the FIXED artifact the reservation rejects
// the second caller before it issues any command (arrivals == 1).
func TestWave20_VOLHIGH5_UnmountConcurrentDoubleUnmount(t *testing.T) {
	exec := &mockExecutor{}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
		},
	}
	mgr := NewVolumeManager(
		hm, exec, logging.NopLogger{}, WithLocalHostAddress("10.0.0.99"),
	)

	mount := VolumeMount{
		Name: "dup-unmount", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	var fusermountArrivals int32
	bothPassed := make(chan struct{})
	var once sync.Once
	exec.executeFunc = func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		if strings.HasPrefix(strings.TrimSpace(cmd), "fusermount") {
			n := atomic.AddInt32(&fusermountArrivals, 1)
			if n >= 2 {
				once.Do(func() { close(bothPassed) })
			}
			select {
			case <-bothPassed:
			case <-time.After(500 * time.Millisecond):
				// Fixed code: exactly one caller reaches here; it falls
				// through after the timeout since the other was rejected by
				// the reservation before issuing any command.
			}
			// Model the real umount race: whichever arrives first "wins"
			// (n==1); a second real invocation would find nothing left to
			// unmount, exactly like a genuine concurrent fusermount race.
			if n == 1 {
				return &remote.CommandResult{ExitCode: 0}, nil
			}
			return &remote.CommandResult{ExitCode: 1, Stderr: "not mounted"}, nil
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = mgr.Unmount(context.Background(), "dup-unmount")
		}(i)
	}
	wg.Wait()

	arrivals := atomic.LoadInt32(&fusermountArrivals)
	t.Logf("fusermount arrivals: %d; errs: %v, %v", arrivals, errs[0], errs[1])

	if arrivals != 1 {
		t.Fatalf("VOL-HIGH-5: reservation guard defeated — %d concurrent "+
			"Unmount(%q) calls reached the real remote unmount command "+
			"(want exactly 1); the second races the first and can report "+
			"a false FAIL (e.g. 'not mounted') for an already cleanly "+
			"torn-down volume", arrivals, mount.Name)
	}

	successes, rejections := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case strings.Contains(e.Error(), "already being unmounted"):
			rejections++
		}
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("VOL-HIGH-5: want exactly one real-unmount success and "+
			"one reservation rejection; got successes=%d rejections=%d "+
			"(errs: %v, %v)", successes, rejections, errs[0], errs[1])
	}

	// The volume is genuinely gone: Status resolves to not-found.
	_, statErr := mgr.Status("dup-unmount")
	assert.Error(t, statErr)
}

// TestWave20_VOLHIGH1_NFSMountRequiresLocalHostAddress is the VOL-HIGH-1
// guard.
//
// DEFECT (RED on the pre-fix nfs.go): the remote `mount -t nfs <server>:
// <export> <mountpoint>` command needs a real NFS server address distinct
// from the export path. The pre-fix code built
// `mount -t nfs <LocalPath>:<LocalPath> <RemotePath>` — using the local
// filesystem path as BOTH the server and the export — which no NFS client
// can ever resolve; every NFS mount was categorically broken while still
// reporting whatever exit code the (nonsensical) command line produced.
func TestWave20_VOLHIGH1_NFSMountRequiresLocalHostAddress(t *testing.T) {
	t.Run("UnconfiguredFailsLoudBeforeAnyRemoteCommand", func(t *testing.T) {
		var cmds []string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			cmds = append(cmds, cmd)
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		m := NewNFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
		mount := VolumeMount{
			Name: "nfs-x", Type: MountNFS,
			LocalPath: "/local/nfs", RemotePath: "/mnt", HostName: "test-host",
		}
		err := m.Mount(context.Background(), testHost(), mount)
		if err == nil {
			t.Fatalf("VOL-HIGH-1: NFS Mount with no LocalHostAddress " +
				"configured succeeded — the pre-fix code built `mount -t " +
				"nfs <LocalPath>:<LocalPath> <RemotePath>`, using the " +
				"local filesystem path as BOTH the NFS server address and " +
				"the export, which no NFS client can ever resolve")
		}
		if !strings.Contains(err.Error(), "no local host address configured") {
			t.Fatalf("VOL-HIGH-1: want a clear config error, got: %v", err)
		}
		if len(cmds) != 0 {
			t.Fatalf("VOL-HIGH-1: NFS Mount issued %d remote command(s) "+
				"before failing loud; want 0 (fail BEFORE emitting the "+
				"known-broken command): %v", len(cmds), cmds)
		}
	})

	t.Run("ConfiguredBuildsServerExportNotLocalPathTwice", func(t *testing.T) {
		var gotCmd string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			if strings.HasPrefix(strings.TrimSpace(cmd), "mount ") {
				gotCmd = cmd
			}
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		m := NewNFSMounter(exec, logging.NopLogger{}, ApplyOptions(
			[]Option{WithLocalHostAddress("192.168.50.10")},
		))
		mount := VolumeMount{
			Name: "nfs-x", Type: MountNFS,
			LocalPath: "/local/nfs", RemotePath: "/mnt", HostName: "test-host",
		}
		require.NoError(t, m.Mount(context.Background(), testHost(), mount))
		t.Logf("generated NFS mount command: %s", gotCmd)

		if !strings.Contains(gotCmd, "'192.168.50.10':'/local/nfs'") {
			t.Fatalf("VOL-HIGH-1: NFS mount source must be "+
				"<LocalHostAddress>:<LocalPath>, got: %q", gotCmd)
		}
		if strings.Contains(gotCmd, "'/local/nfs':'/local/nfs'") {
			t.Fatalf("VOL-HIGH-1: NFS mount source must NOT use LocalPath "+
				"as both server and export, got: %q", gotCmd)
		}
	})
}

// TestWave20_VOLHIGH2_SSHFSMountRequiresLocalHostAddress is the VOL-HIGH-2
// guard.
//
// DEFECT (RED on the pre-fix sshfs.go): sshfs's source argument MUST carry a
// "[user@]host:path" prefix — the remote host uses it to open its OWN ssh
// connection back to the source. The pre-fix code interpolated only
// mount.LocalPath with NO host prefix at all, which sshfs cannot use to
// locate a remote source.
func TestWave20_VOLHIGH2_SSHFSMountRequiresLocalHostAddress(t *testing.T) {
	t.Run("UnconfiguredFailsLoudBeforeAnyRemoteCommand", func(t *testing.T) {
		var cmds []string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			cmds = append(cmds, cmd)
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
		mount := VolumeMount{
			Name: "sshfs-x", Type: MountSSHFS,
			LocalPath: "/local/data", RemotePath: "/mnt", HostName: "test-host",
		}
		err := m.Mount(context.Background(), testHost(), mount)
		if err == nil {
			t.Fatalf("VOL-HIGH-2: SSHFS Mount with no LocalHostAddress " +
				"configured succeeded — the pre-fix code interpolated only " +
				"LocalPath with no \"[user@]host:\" source prefix at all, " +
				"which sshfs cannot use to open a remote source")
		}
		if !strings.Contains(err.Error(), "no local host address configured") {
			t.Fatalf("VOL-HIGH-2: want a clear config error, got: %v", err)
		}
		if len(cmds) != 0 {
			t.Fatalf("VOL-HIGH-2: SSHFS Mount issued %d remote command(s) "+
				"before failing loud; want 0: %v", len(cmds), cmds)
		}
	})

	t.Run("ConfiguredBuildsHostColonPathSource", func(t *testing.T) {
		var gotCmd string
		exec := &mockExecutor{executeFunc: func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			if strings.HasPrefix(strings.TrimSpace(cmd), "sshfs") {
				gotCmd = cmd
			}
			return &remote.CommandResult{ExitCode: 0}, nil
		}}
		m := NewSSHFSMounter(exec, logging.NopLogger{}, ApplyOptions(
			[]Option{WithLocalHostAddress("192.168.50.10")},
		))
		mount := VolumeMount{
			Name: "sshfs-x", Type: MountSSHFS,
			LocalPath: "/local/data", RemotePath: "/mnt", HostName: "test-host",
		}
		require.NoError(t, m.Mount(context.Background(), testHost(), mount))
		t.Logf("generated sshfs mount command: %s", gotCmd)

		if !strings.Contains(gotCmd, "'192.168.50.10':'/local/data'") {
			t.Fatalf("VOL-HIGH-2: sshfs source must carry a "+
				"<LocalHostAddress>:<LocalPath> prefix, got: %q", gotCmd)
		}
	})
}

// TestWave20_VOLHIGH3_ReadOnlySyncActuallyPopulatesDestination is the
// VOL-HIGH-3 guard.
//
// DEFECT (RED on the pre-fix rsync.go): a ReadOnly Sync added --dry-run.
// rsync's own documented semantics: --dry-run performs a trial run that
// transfers NOTHING. So every ReadOnly Sync reported success (exit 0) while
// the remote directory was NEVER populated — a permanent false-success.
//
// Reproduced via the fake-executor seam by simulating the remote
// filesystem: a "marker" is considered present at RemotePath the moment a
// REAL (non --dry-run) rsync command runs. A dry-run rsync command must
// never flip the marker.
func TestWave20_VOLHIGH3_ReadOnlySyncActuallyPopulatesDestination(t *testing.T) {
	var (
		markerPresent bool
		chmodIssued   bool
		rsyncCmd      string
	)
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		trimmed := strings.TrimSpace(cmd)
		switch {
		case strings.HasPrefix(trimmed, "rsync "):
			rsyncCmd = cmd
			if !strings.Contains(cmd, "--dry-run") {
				// A real (non-dry-run) rsync transfer: the marker file now
				// exists at RemotePath on the simulated remote filesystem.
				markerPresent = true
			}
		case strings.HasPrefix(trimmed, "chmod "):
			chmodIssued = true
			if !markerPresent {
				// Honest simulation: chmod-ing a directory that was never
				// actually populated (dry-run) would fail on a real remote.
				return &remote.CommandResult{
					ExitCode: 1,
					Stderr:   "chmod: cannot access '/remote/ro': No such file or directory",
				}, nil
			}
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}}
	syncer := NewRsyncSyncer(exec, logging.NopLogger{}, DefaultMountOptions())

	mount := VolumeMount{
		Name: "ro-vol", Type: MountRsync,
		LocalPath: "/local/data", RemotePath: "/remote/ro",
		HostName: "test-host", ReadOnly: true,
	}
	err := syncer.Sync(context.Background(), testHost(), mount)
	require.NoError(t, err)

	if strings.Contains(rsyncCmd, "--dry-run") {
		t.Fatalf("VOL-HIGH-3: ReadOnly Sync used --dry-run (%q) — rsync's "+
			"own documented semantics transfer NOTHING under --dry-run, so "+
			"a ReadOnly Sync reports success while the remote directory is "+
			"never populated", rsyncCmd)
	}
	if !markerPresent {
		t.Fatalf("VOL-HIGH-3: marker file NOT present at RemotePath after " +
			"a ReadOnly Sync reported success — the transfer did not " +
			"actually happen")
	}
	if !chmodIssued {
		t.Fatalf("VOL-HIGH-3: no destination write-protection (chmod) was " +
			"applied after a ReadOnly Sync; read-only must be enforced via " +
			"OS-level write-protection on a REAL transfer, not by skipping " +
			"the transfer")
	}
}

// TestWave20_VOLMED8_CommandTimeoutBoundsRemoteCall is the VOL-MED-8 guard.
//
// DEFECT (RED on the pre-fix manager.go/types.go/options.go): pkg/volume had
// ZERO context.WithTimeout/WithDeadline anywhere, unlike every sibling
// package in this module (pkg/genymotion, pkg/cuttlefish, pkg/emulator,
// pkg/orchestrator, pkg/vm), which all wrap remote calls in a local
// deadline. With no local timeout, a stalled remote mount/unmount/rsync
// command hangs the caller for as long as the caller's OWN ctx allows —
// context.Background() never cancels, so the call can hang forever.
func TestWave20_VOLMED8_CommandTimeoutBoundsRemoteCall(t *testing.T) {
	// The fake "remote command" never returns on its own — it only unblocks
	// when its ctx is cancelled/expires, modeling a genuinely wedged remote
	// mount call.
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
		},
	}
	mgr := NewVolumeManager(
		hm, exec, logging.NopLogger{},
		WithLocalHostAddress("10.0.0.99"),
		WithCommandTimeout(50*time.Millisecond),
	)

	mount := VolumeMount{
		Name: "timeout-vol", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}

	start := time.Now()
	err := mgr.Mount(context.Background(), mount)
	elapsed := time.Since(start)
	t.Logf("Mount against a permanently-stalled remote call returned after "+
		"%v: %v", elapsed, err)

	if err == nil {
		t.Fatalf("VOL-MED-8: Mount against a permanently-stalled remote " +
			"command returned nil; want a context-deadline error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("VOL-MED-8: Mount took %v to return against a configured "+
			"50ms CommandTimeout — the local deadline did not bound the "+
			"remote call (with CommandTimeout unconfigured/0 this call "+
			"would block forever on context.Background())", elapsed)
	}
}

// TestWave20_VOLMED8_ZeroCommandTimeoutPreservesPriorBehavior confirms the
// additive/backward-compatible contract: CommandTimeout's zero value (the
// default — no option supplied) leaves the caller's ctx completely
// unmodified, so every existing caller (none of which sets
// WithCommandTimeout) keeps its exact prior behavior.
func TestWave20_VOLMED8_ZeroCommandTimeoutPreservesPriorBehavior(t *testing.T) {
	var sawCtx context.Context
	exec := &mockExecutor{executeFunc: func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		sawCtx = ctx
		return &remote.CommandResult{ExitCode: 0}, nil
	}}
	hm := &mockHostManager{
		hosts: map[string]remote.RemoteHost{
			"host-1": {Name: "host-1", Address: "10.0.0.1", User: "deploy", Port: 22},
		},
	}
	mgr := NewVolumeManager(
		hm, exec, logging.NopLogger{}, WithLocalHostAddress("10.0.0.99"),
	)

	type ctxKey struct{}
	callerCtx := context.WithValue(context.Background(), ctxKey{}, "marker")
	mount := VolumeMount{
		Name: "no-timeout-vol", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(callerCtx, mount))

	if sawCtx.Value(ctxKey{}) != "marker" {
		t.Fatalf("VOL-MED-8: with CommandTimeout unconfigured (0), the " +
			"executor must see the caller's ORIGINAL ctx unmodified; a " +
			"derived context.WithTimeout wrapper (even a no-op one) would " +
			"not carry the caller's WithValue marker forward implicitly " +
			"the way passing ctx straight through does")
	}
	if _, hasDeadline := sawCtx.Deadline(); hasDeadline {
		t.Fatalf("VOL-MED-8: with CommandTimeout unconfigured (0), the " +
			"executor's ctx must NOT carry a deadline")
	}
}
