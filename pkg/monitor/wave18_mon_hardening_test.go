package monitor_test

import (
	"sync"
	"testing"

	"digital.vasic.containers/pkg/monitor"
	"digital.vasic.containers/pkg/remote"
)

// TestHardenMON1_ConcurrentCollect_Race is the §11.4.115 RED→GREEN guard for
// CT-HARDEN-MON-1: concurrent Collect() on ONE DefaultSystemCollector races on
// the unsynchronized prevIdle/prevTotal CPU-delta state (system.go). Run under
// -race: pre-fix it reports WARNING: DATA RACE at collectCPULinux; post-fix the
// sync.Mutex serialises the read-sample→delta→update-prev sequence and it
// passes. Removing the mutex re-arms the race → this test FAILs under -race
// (a real oracle, not a test that agrees with the fix).
func TestHardenMON1_ConcurrentCollect_Race(t *testing.T) {
	c := monitor.NewDefaultSystemCollector()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = c.Collect()
			}
		}()
	}
	wg.Wait()
}

// TestHardenMON2_NilRemoteHost_Panic is the §11.4.115 RED→GREEN guard for
// CT-HARDEN-MON-2: a nil *remote.HostResources value in the remoteHosts map
// (a legitimate "host failed to probe" entry) is dereferenced (hr.CPUCores) in
// NewClusterSnapshot and panics SIGSEGV (types.go). Pre-fix this test panics
// and FAILs; post-fix the `if hr == nil { continue }` guard skips it — the nil
// host contributes nothing and is not counted, the valid host is. Removing the
// guard re-arms the panic → this test FAILs.
func TestHardenMON2_NilRemoteHost_Panic(t *testing.T) {
	local := &monitor.ResourceSnapshot{
		System: monitor.SystemResources{
			MemoryTotal: 1 << 30,
			DiskTotal:   1 << 30,
		},
	}
	remotes := map[string]*remote.HostResources{
		"host-failed-probe": nil,
		"host-ok":           {CPUCores: 4, MemoryTotalMB: 2048, DiskTotalMB: 5000},
	}
	cs := monitor.NewClusterSnapshot(local, remotes)
	if cs == nil {
		t.Fatal("nil cluster snapshot")
	}
	// local (seeded HostCount=1) + the one non-nil remote = 2; the nil host is
	// skipped, so it is neither counted nor dereferenced.
	if cs.HostCount != 2 {
		t.Fatalf("HostCount = %d, want 2 (local + non-nil remote)", cs.HostCount)
	}
	if cs.TotalCPUCores != 4 {
		t.Fatalf("TotalCPUCores = %d, want 4 (nil host contributes nothing)", cs.TotalCPUCores)
	}
}
