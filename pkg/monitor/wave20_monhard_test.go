package monitor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"digital.vasic.containers/pkg/runtime"
)

// wave20_monhard_test.go — §11.4.115 RED→GREEN guards for batch
// CT-HARDEN-MON-HARD (Wave-20). Committed-default polarity is GUARD (GREEN on
// the fixed tree). Each guard was proven a real oracle by a surgical revert of
// its fix producing a genuine `--- FAIL`, then restore → GREEN.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the parse / counter-guard /
// error-surface logic behaves correctly on INJECTED in-process fixtures
// (fabricated /proc files, a fake runtime). They do NOT observe a live kernel
// counter rollback or a real container-runtime outage — the injected fixture
// IS the device-independent stand-in for those conditions.

// TestHardenMON1_CounterBackwardsSentinel_NoPoison covers MON-1: a read-error /
// malformed-line sentinel (short "cpu 100 200 300" line → readCPUSampleOK ok=
// false, (0,0)) fed to collectCPULinuxFromFile with a LARGE primed prevTotal
// must NOT underflow the uint64 delta into an out-of-[0,100] CPU%, and must NOT
// clobber prevIdle/prevTotal (which would corrupt the next good sample too).
// Pre-fix (guard = only `total == prevTotal`) this returns a large-magnitude
// out-of-range value AND zeroes prev.
func TestHardenMON1_CounterBackwardsSentinel_NoPoison(t *testing.T) {
	dir := t.TempDir()
	// Short cpu line: len(fields)==4 < 5 → the sentinel path (0, 0, ok=false).
	path := filepath.Join(dir, "stat")
	if err := os.WriteFile(path, []byte("cpu 100 200 300\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const (
		primedIdle  = uint64(1_000_000)
		primedTotal = uint64(9_000_000)
	)
	c := &DefaultSystemCollector{prevIdle: primedIdle, prevTotal: primedTotal}

	got := c.collectCPULinuxFromFile(path)

	if got < 0.0 || got > 100.0 {
		t.Fatalf("CPU%% = %v, want within [0,100] (sentinel must not underflow)", got)
	}
	if got != 0.0 {
		t.Fatalf("CPU%% = %v, want 0 on the read-error sentinel", got)
	}
	if c.prevIdle != primedIdle || c.prevTotal != primedTotal {
		t.Fatalf("prev clobbered: prevIdle=%d prevTotal=%d, want %d/%d (last-good baseline preserved)",
			c.prevIdle, c.prevTotal, primedIdle, primedTotal)
	}
}

// TestHardenMON1_CounterRollback_NoPoison covers the counter-BACKWARDS arm of
// MON-1 with a well-formed but SMALLER-than-prev sample (a real counter
// rollback, e.g. CPU hotplug / namespace change): total < prevTotal must return
// 0 without clobbering prev, never a negative %.
func TestHardenMON1_CounterRollback_NoPoison(t *testing.T) {
	dir := t.TempDir()
	// Full 10-field cpu line: idle(index3)=400, total=5500 — both below prev.
	path := filepath.Join(dir, "stat")
	content := "cpu  100 200 300 400 500 600 700 800 900 1000\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	const (
		primedIdle  = uint64(9_000_000)
		primedTotal = uint64(9_000_000)
	)
	c := &DefaultSystemCollector{prevIdle: primedIdle, prevTotal: primedTotal}

	got := c.collectCPULinuxFromFile(path)

	if got < 0.0 || got > 100.0 {
		t.Fatalf("CPU%% = %v, want within [0,100] on counter rollback", got)
	}
	if got != 0.0 {
		t.Fatalf("CPU%% = %v, want 0 on counter rollback", got)
	}
	if c.prevIdle != primedIdle || c.prevTotal != primedTotal {
		t.Fatalf("prev clobbered on rollback: %d/%d, want %d/%d",
			c.prevIdle, c.prevTotal, primedIdle, primedTotal)
	}
}

// TestHardenMON2_MemAvailableAbsent_NotFull covers MON-2: a meminfo with
// MemTotal but NO MemAvailable (and no MemFree either) must NOT report 100%
// used on an idle host. Pre-fix (`memAvailable <= memTotal` with memAvailable=0)
// yields MemoryUsed=memTotal / MemoryPercent=100.
func TestHardenMON2_MemAvailableAbsent_NotFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	// MemTotal only — MemAvailable AND MemFree both absent → available unknown.
	if err := os.WriteFile(path, []byte("MemTotal:       16000000 kB\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	c := &DefaultSystemCollector{}
	var res SystemResources
	c.collectMemoryLinuxFromFile(&res, path)

	if res.MemoryTotal != 16000000*1024 {
		t.Fatalf("MemoryTotal = %d, want %d", res.MemoryTotal, uint64(16000000*1024))
	}
	if res.MemoryPercent == 100.0 {
		t.Fatalf("MemoryPercent = 100 on a MemAvailable-absent meminfo (false-full); want unknown/0")
	}
	if res.MemoryPercent != 0.0 || res.MemoryUsed != 0 {
		t.Fatalf("MemAvailable-absent should leave used/percent at 0 (unknown); got used=%d percent=%v",
			res.MemoryUsed, res.MemoryPercent)
	}
}

// TestHardenMON2_MemFreeFallback covers the MON-2 fallback branch: MemAvailable
// absent but MemFree+Buffers+Cached present → available approximated (never 0),
// so the result is a real, in-range, non-100 percentage.
func TestHardenMON2_MemFreeFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	// available ≈ MemFree(4e6)+Buffers(1e6)+Cached(3e6) = 8e6 of 16e6 → 50% used.
	content := "MemTotal:       16000000 kB\n" +
		"MemFree:         4000000 kB\n" +
		"Buffers:         1000000 kB\n" +
		"Cached:          3000000 kB\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	c := &DefaultSystemCollector{}
	var res SystemResources
	c.collectMemoryLinuxFromFile(&res, path)

	if res.MemoryPercent == 100.0 {
		t.Fatalf("MemoryPercent = 100 with a MemFree fallback available; want ~50")
	}
	if res.MemoryPercent != 50.0 {
		t.Fatalf("MemoryPercent = %v, want 50 (MemFree+Buffers+Cached fallback)", res.MemoryPercent)
	}
	if res.MemoryUsed != 8000000*1024 {
		t.Fatalf("MemoryUsed = %d, want %d", res.MemoryUsed, uint64(8000000*1024))
	}
}

// mon3FakeRuntime is a minimal ContainerRuntime whose List and Stats can be
// forced to error, to prove the MON-3 error-surface on the snapshot.
type mon3FakeRuntime struct {
	listErr    error
	containers []runtime.ContainerInfo
	statsErr   error
}

func (f *mon3FakeRuntime) Name() string { return "mon3-fake" }
func (f *mon3FakeRuntime) Version(context.Context) (string, error) {
	return "0.0.0", nil
}
func (f *mon3FakeRuntime) IsAvailable(context.Context) bool { return true }
func (f *mon3FakeRuntime) Start(context.Context, string, ...runtime.StartOption) error {
	return nil
}
func (f *mon3FakeRuntime) Stop(context.Context, string, ...runtime.StopOption) error {
	return nil
}
func (f *mon3FakeRuntime) Remove(context.Context, string, ...runtime.RemoveOption) error {
	return nil
}
func (f *mon3FakeRuntime) Status(context.Context, string) (*runtime.ContainerStatus, error) {
	return &runtime.ContainerStatus{}, nil
}
func (f *mon3FakeRuntime) List(
	context.Context, runtime.ListFilter,
) ([]runtime.ContainerInfo, error) {
	return f.containers, f.listErr
}
func (f *mon3FakeRuntime) Stats(
	context.Context, string,
) (*runtime.ContainerStats, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return &runtime.ContainerStats{}, nil
}
func (f *mon3FakeRuntime) Exec(
	context.Context, string, []string,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
func (f *mon3FakeRuntime) Logs(
	context.Context, string, ...runtime.LogOption,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

// TestHardenMON3_ListError_Surfaced covers MON-3: when rt.List errors, the
// published snapshot must carry ListError=true so a consumer can distinguish a
// runtime outage from a genuinely empty host — instead of silently publishing
// an empty, "healthy"-looking snapshot. Pre-fix (`if err == nil` skip) the
// snapshot carried no such signal.
func TestHardenMON3_ListError_Surfaced(t *testing.T) {
	rt := &mon3FakeRuntime{listErr: io.ErrUnexpectedEOF}
	m := NewDefaultMonitor(rt, nil)

	m.collect(context.Background())

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.ListError {
		t.Fatalf("ListError = false on a List() outage; want true (error must not read as empty-healthy)")
	}
	if len(snap.Containers) != 0 {
		t.Fatalf("Containers = %d, want 0 on List error", len(snap.Containers))
	}
}

// TestHardenMON3_StatsFailure_Surfaced covers MON-3: a listed container whose
// Stats probe fails is STILL dropped from Containers (existing intended
// behavior, TestDefaultMonitor_StatsError), but the snapshot now records
// StatsFailures>0 so the drop is visible rather than reading as "0 containers,
// all healthy". Pre-fix the drop was silent.
func TestHardenMON3_StatsFailure_Surfaced(t *testing.T) {
	rt := &mon3FakeRuntime{
		containers: []runtime.ContainerInfo{
			{ID: "c1", Name: "redis"},
			{ID: "c2", Name: "pg"},
		},
		statsErr: io.ErrUnexpectedEOF,
	}
	m := NewDefaultMonitor(rt, nil)

	m.collect(context.Background())

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Drop preserved: both stats-errored containers are absent (unchanged).
	if len(snap.Containers) != 0 {
		t.Fatalf("Containers = %d, want 0 (stats-errored containers dropped)", len(snap.Containers))
	}
	// ...but the failure is now surfaced.
	if snap.StatsFailures != 2 {
		t.Fatalf("StatsFailures = %d, want 2 (both probe failures surfaced)", snap.StatsFailures)
	}
	if snap.ListError {
		t.Fatalf("ListError = true on a Stats-only failure; want false (List succeeded)")
	}
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (f *mon3FakeRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
