package volume

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"digital.vasic.containers/pkg/remote"
)

// TestDefaultVolumeManager_Status_LivePointerDataRace is the CT-HARDEN-33
// (V-1) regression guard.
//
// DEFECT (RED baseline): Status() returned the LIVE internal *MountInfo
// pointer straight out of m.mounts[name]. Its RLock is released the instant
// Status returns, so a caller that then reads info.State / info.Error does so
// with NO synchronization while Sync() / Unmount() concurrently WRITE those
// same string-typed fields on the SAME struct under the write lock — a data
// race the -race detector proves, and (because MountInfo.State/Error are
// plain strings) a torn read of a (ptr,len) string header.
//
// This test drives the exact concurrent access. Under `go test -race`:
//   - unfixed Status (returns live pointer) => WARNING: DATA RACE => FAIL
//   - fixed Status  (returns a value copy)  => no race            => PASS
//
// The -race detector IS the polarity oracle (§11.4.115): one source, RED on
// the broken artifact, GREEN on the fixed one — no separate happy-path test.
func TestDefaultVolumeManager_Status_LivePointerDataRace(t *testing.T) {
	// --- sub-case A: Status poll races Sync mutation --------------------
	t.Run("StatusVsSync", func(t *testing.T) {
		mgr, _ := newTestVolumeManager()
		ctx := context.Background()

		mount := VolumeMount{
			Name: "race-data", Type: MountRsync,
			LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
		}
		if err := mgr.Mount(ctx, mount); err != nil {
			t.Fatalf("mount: %v", err)
		}

		const iters = 500
		var wg sync.WaitGroup
		wg.Add(2)

		// Writer: Sync flips State MountSyncing -> MountMounted and clears
		// Error, mutating the internal *MountInfo under the write lock.
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = mgr.Sync(ctx, "race-data")
			}
		}()

		// Reader: poll Status and read the returned struct's fields. With the
		// unfixed code these reads hit the SAME struct the writer mutates,
		// with no lock held -> data race on State (and Error).
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				info, err := mgr.Status("race-data")
				if err != nil {
					continue
				}
				_ = info.State
				_ = info.Error
			}
		}()

		wg.Wait()
	})

	// --- sub-case B: Status poll races Unmount(failure) mutation --------
	// On a remote unmount failure Unmount sets State=MountFailed + Error
	// under the write lock and KEEPS the entry, so the escaped pointer the
	// reader holds is mutated concurrently — same race class as A.
	t.Run("StatusVsUnmountFailure", func(t *testing.T) {
		mgr, exec := newTestVolumeManager()
		ctx := context.Background()

		mount := VolumeMount{
			Name: "race-unmount", Type: MountSSHFS,
			LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
		}
		if err := mgr.Mount(ctx, mount); err != nil {
			t.Fatalf("mount: %v", err)
		}
		// Force the remote unmount command to fail so Unmount takes the
		// State=MountFailed branch and retains the map entry.
		exec.executeFunc = func(
			ctx context.Context, host remote.RemoteHost, cmd string,
		) (*remote.CommandResult, error) {
			return nil, fmt.Errorf("umount: target is busy")
		}

		const iters = 500
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = mgr.Unmount(ctx, "race-unmount")
			}
		}()

		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				info, err := mgr.Status("race-unmount")
				if err != nil {
					continue
				}
				_ = info.State
				_ = info.Error
			}
		}()

		wg.Wait()
	})
}
