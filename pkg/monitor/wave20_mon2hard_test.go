package monitor

import (
	"os"
	"path/filepath"
	"testing"

	"digital.vasic.containers/pkg/remote"
)

// wave20_mon2hard_test.go — §11.4.115 RED→GREEN guards for the SECOND monitor
// hardening pass (Wave-20 DEEPER, batch CT-HARDEN-MON2HARD). These cover
// GENUINE defects the first pass (CT-HARDEN-MON-HARD MON-1/2/3 + CT-HARDEN-MON-1/2)
// MISSED. Committed-default polarity is GUARD (GREEN on the fixed tree); each
// guard was proven a real oracle by a surgical single-line revert producing a
// genuine `--- FAIL`, then restore → GREEN.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the counter-sanity /
// aggregation-honesty logic on INJECTED in-process fixtures (a fabricated
// /proc/stat line, a hand-built cluster). They do NOT observe a live kernel
// counter rollback or a real absent-local-probe — the injected fixture IS the
// device-independent stand-in.

// TestWave20_MON2_IdleFasterThanTotal_NoNegativeCPU covers MON2-1: the MON-1
// counter guard rejects (a) the read-error sentinel, (b) total not advancing,
// and (c) idle running BACKWARDS — but it does NOT reject the case where idle
// advances FASTER than total (idleDelta > totalDelta). idle is a summand of
// total, so on monotonic counters idleDelta<=totalDelta always; but a PARTIAL
// counter rollback (non-idle sub-counters roll back while idle + net-total
// still advance — CPU hotplug / cgroup / namespace-view change, the exact class
// MON-1 defends against) breaks that invariant and yields a NEGATIVE CPU%
// ((1 - idleDelta/totalDelta)*100 < 0), out of the [0,100] range every bounds
// test asserts. Pre-fix this returns a large negative percentage.
func TestWave20_MON2_IdleFasterThanTotal_NoNegativeCPU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	// Full 10-field cpu line: idle(index3)=250, total = 30+0+0+250+21 = 301.
	// Against a primed prev of idle=100/total=200 this passes every MON-1
	// guard (total 301>200, idle 250>=100) yet idleDelta(150) > totalDelta(101),
	// i.e. the non-idle sub-total rolled back by 49.
	content := "cpu  30 0 0 250 21 0 0 0 0 0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const (
		primedIdle  = uint64(100)
		primedTotal = uint64(200)
	)
	c := &DefaultSystemCollector{prevIdle: primedIdle, prevTotal: primedTotal}

	got := c.collectCPULinuxFromFile(path)

	if got < 0.0 || got > 100.0 {
		t.Fatalf("CPU%% = %v, want within [0,100] (idle advancing faster than total must not go negative)", got)
	}
	if got != 0.0 {
		t.Fatalf("CPU%% = %v, want 0 when idleDelta > totalDelta (partial counter rollback)", got)
	}
	// prev baseline must be preserved on the rejected sample (MON-1 philosophy).
	if c.prevIdle != primedIdle || c.prevTotal != primedTotal {
		t.Fatalf("prev clobbered on partial rollback: %d/%d, want %d/%d",
			c.prevIdle, c.prevTotal, primedIdle, primedTotal)
	}
}

// TestWave20_MON2_NilLocalHostCount_NotCounted covers MON2-2: NewClusterSnapshot
// seeds HostCount=1 for the local host UNCONDITIONALLY, even when the local
// snapshot is nil. The `if local != nil` guard around the local field reads
// PROVES nil-local is an anticipated input, and the sibling nil-REMOTE path
// (CT-HARDEN-MON-2: `if hr == nil { continue }` → not counted) established the
// design intent "a host with no resource data is NOT counted". A nil local is
// exactly such a host, yet it was still counted — over-reporting the cluster
// size by one and skewing any per-host average a consumer computes when the
// local probe is absent. Pre-fix HostCount = 2 (phantom local + 1 real remote).
func TestWave20_MON2_NilLocalHostCount_NotCounted(t *testing.T) {
	remotes := map[string]*remote.HostResources{
		"host-ok": {CPUCores: 4, MemoryTotalMB: 2048, DiskTotalMB: 5000},
	}
	cs := NewClusterSnapshot(nil, remotes)
	if cs == nil {
		t.Fatal("nil cluster snapshot")
	}
	// Local snapshot absent (nil) → it must NOT be counted; only the one real
	// remote host contributes.
	if cs.HostCount != 1 {
		t.Fatalf("HostCount = %d, want 1 (nil local host must not be counted, only the 1 remote)", cs.HostCount)
	}
	// A nil local contributes no memory/disk (guarded), the remote does.
	if cs.TotalMemoryMB != 2048 {
		t.Fatalf("TotalMemoryMB = %d, want 2048 (nil local contributes nothing)", cs.TotalMemoryMB)
	}
	if cs.TotalCPUCores != 4 {
		t.Fatalf("TotalCPUCores = %d, want 4", cs.TotalCPUCores)
	}
}

// TestWave20_MON2_NonNilLocalStillCounted is the companion positive control:
// the MON2-2 fix must NOT drop a genuinely-present local host from HostCount.
// A non-nil local + one remote = 2. This guards against an over-correction
// (e.g. seeding HostCount=0 and forgetting to re-add the present local).
func TestWave20_MON2_NonNilLocalStillCounted(t *testing.T) {
	local := &ResourceSnapshot{
		System: SystemResources{MemoryTotal: 4 << 30, DiskTotal: 8 << 30},
	}
	remotes := map[string]*remote.HostResources{
		"host-ok": {CPUCores: 2, MemoryTotalMB: 1024, DiskTotalMB: 3000},
	}
	cs := NewClusterSnapshot(local, remotes)
	if cs.HostCount != 2 {
		t.Fatalf("HostCount = %d, want 2 (present local + 1 remote)", cs.HostCount)
	}
	// Local memory (4 GiB → 4096 MB) + remote (1024 MB) = 5120 MB.
	if cs.TotalMemoryMB != 4096+1024 {
		t.Fatalf("TotalMemoryMB = %d, want %d", cs.TotalMemoryMB, uint64(4096+1024))
	}
}
