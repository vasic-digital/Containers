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

// Wave-21 pkg/volume hardening guard (SW3-1).
//
// Polarity (§11.4.115): this guard is a STANDING GREEN assertion of the FIXED
// behavior. The artifact IS the polarity oracle — GREEN on the fixed source,
// RED when the SW3-1 reservation is surgically reverted (restore the
// unconditional `m.mu.Lock(); info.State = MountSyncing; m.mu.Unlock()`). No
// separate happy-path test; the bug-catcher IS the permanent regression guard.
// It drives ONLY the package's existing fake-executor cmd-capture seam
// (mockExecutor / mockHostManager from manager_test.go) — no real rsync/network
// (§11.4.27).

// TestWave21_SW31_SyncConcurrentDoubleSync is the SW3-1 guard.
//
// DEFECT (RED on the pre-fix manager.go): DefaultVolumeManager.Sync had NO
// reservation/barrier (unlike Mount()'s EGVOL-1 MountPending guard and
// Unmount()'s VOL-HIGH-5 MountUnmounting guard). It read info under RLock,
// released, checked info.Mount.Type unlocked, then re-acquired the write lock
// and set info.State = MountSyncing UNCONDITIONALLY — with no check for an
// in-flight sync. Two concurrent Sync(name) calls for the SAME name both passed
// the checks, both set MountSyncing, and both called m.rsync.Sync concurrently
// against the identical (host, LocalPath, RemotePath): two real rsync pushes,
// whose LAST-writer overwrites info.State/info.Error, so the reported outcome
// can reflect the WRONG transfer (a §11.4.108 duplicate-execution /
// wrong-caller-wins gap). This is a LOGICAL double-execution gap, not a raw
// memory race — every field access is mutex-guarded — so `-race` is NOT the
// oracle; the in-flight-transfer counter below is.
//
// Mirrors the VOL-HIGH-5 barrier pattern: the executor gates the rsync command
// on a rendezvous of two arrivals; a lone caller falls through after a fixed
// timeout (holding MountSyncing long enough for the concurrent second caller to
// hit the reservation and be rejected). On the BROKEN artifact both goroutines
// reach the real rsync transfer simultaneously (max in-flight == 2); on the
// FIXED artifact the reservation rejects the second caller before it issues any
// command (max in-flight == 1, one "already being synced" rejection).
func TestWave21_SW31_SyncConcurrentDoubleSync(t *testing.T) {
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
		Name: "dup-sync", Type: MountRsync,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))
	// Mount() of a MountRsync entry runs one rsync push and settles the mount
	// to MountMounted; assert that clean baseline before racing two Syncs.
	base, err := mgr.Status("dup-sync")
	require.NoError(t, err)
	require.Equal(t, MountMounted, base.State)

	var (
		inFlight    int32 // rsync transfers currently executing for this name
		maxInFlight int32 // max ever simultaneously in flight — the oracle
		arrivals    int32 // total rsync commands that reached the executor
	)
	bothArrived := make(chan struct{})
	var once sync.Once
	exec.executeFunc = func(
		ctx context.Context, host remote.RemoteHost, cmd string,
	) (*remote.CommandResult, error) {
		if strings.HasPrefix(strings.TrimSpace(cmd), "rsync ") {
			// A real rsync transfer for this mount is now in flight. Record the
			// max number EVER simultaneously in flight for the same name — the
			// double-execution oracle.
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old ||
					atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			if atomic.AddInt32(&arrivals, 1) >= 2 {
				once.Do(func() { close(bothArrived) })
			}
			// Hold the transfer so a concurrent SECOND transfer (the bug) would
			// overlap this one. On the BROKEN artifact both callers reach here
			// and unblock each other via bothArrived. On the FIXED artifact
			// only one caller reaches here; it falls through after the timeout,
			// having held MountSyncing long enough for the rejected second
			// caller to hit the reservation.
			select {
			case <-bothArrived:
			case <-time.After(500 * time.Millisecond):
			}
			atomic.AddInt32(&inFlight, -1)
		}
		return &remote.CommandResult{ExitCode: 0}, nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = mgr.Sync(context.Background(), "dup-sync")
		}(i)
	}
	wg.Wait()

	maxObserved := atomic.LoadInt32(&maxInFlight)
	t.Logf("max simultaneous rsync transfers in flight for %q: %d; errs: %v, %v",
		mount.Name, maxObserved, errs[0], errs[1])

	if maxObserved != 1 {
		t.Fatalf("SW3-1: reservation guard defeated — up to %d concurrent "+
			"Sync(%q) calls drove the REAL rsync transfer SIMULTANEOUSLY "+
			"against the identical (host, LocalPath, RemotePath) (want max 1); "+
			"two racing rsync pushes mean whichever finishes LAST overwrites "+
			"info.State/info.Error, so the reported outcome can reflect the "+
			"WRONG transfer", maxObserved, mount.Name)
	}

	successes, rejections := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case strings.Contains(e.Error(), "already being synced"):
			rejections++
		}
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("SW3-1: want exactly one real-sync success and one "+
			"reservation rejection; got successes=%d rejections=%d "+
			"(errs: %v, %v)", successes, rejections, errs[0], errs[1])
	}

	// A completed Sync releases the reservation: the mount is back to
	// MountMounted and a fresh Sync succeeds — proving the guard RESERVES then
	// RELEASES (it is not a permanent lock-out).
	after, err := mgr.Status("dup-sync")
	require.NoError(t, err)
	assert.Equal(t, MountMounted, after.State)
	require.NoError(t, mgr.Sync(context.Background(), "dup-sync"))
}
