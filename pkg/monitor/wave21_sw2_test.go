package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

// wave21_sw2_test.go — §11.4.115 RED→GREEN guard for SW2-1 (Wave-21).
//
// Defect (HIGH): when a host-metric probe FAILS, the corresponding
// SystemResources.*Percent field was silently left at the Go zero (0.0),
// indistinguishable from a genuinely-idle host. ThresholdEvaluator.resolveMetric
// then returned (0, ok=true) — "genuine reading" — so an operator rule such as
// `system.disk > 90 → page` NEVER fired when the statfs probe was broken while
// the disk was actually full: compare(0, ">", 90) == false, silently, forever.
// This is the SAME silent-zero class the package already guards one layer up for
// containers via ResourceSnapshot.ListError / StatsFailures (MON-3) and ctop's
// StatsUnavailable — the system collector was the one place it was missing.
//
// Fix: SystemResources carries per-metric CPUError / MemoryError / DiskError
// flags, set on each collector's read-failure path; resolveMetric returns
// ok=false for a metric whose flag is set, so Evaluate's `if !ok { continue }`
// treats a broken probe as "cannot resolve this rule" rather than a resolved 0.
//
// Anti-tautology lever: each test drives the REAL collector at a NONEXISTENT
// path to force a genuine probe failure (no hand-set flags), asserts the flag is
// raised, then asserts a ThresholdEvaluator rule that fires on ANY resolvable
// reading (`>= 0`) does NOT fire for the broken metric — AND a genuine reading
// through the SAME rule STILL fires (the fix must not suppress real zeros/
// readings). Reverting resolveMetric to its unconditional `return v, true` makes
// the broken-probe rule fire on the leftover 0 → the "not fired" assertions FAIL
// (RED). Restore → GREEN.
//
// HONEST BOUNDARY (§11.4.107): the injected nonexistent path is the
// device-independent stand-in for an unreadable /proc file or a failing statfs;
// this proves the failure-detection + alert-suppression logic, not a live kernel
// probe outage.

// evalFires registers a single rule on a fresh evaluator and reports whether it
// fired for the given system snapshot.
func evalFires(rule ThresholdRule, sys SystemResources) bool {
	fired := false
	rule.Action = func(_ *ResourceSnapshot) { fired = true }
	e := NewThresholdEvaluator()
	e.AddRule(rule)
	e.Evaluate(&ResourceSnapshot{System: sys})
	return fired
}

// TestSW2_1_DiskProbeFailure_FlaggedAndAlertSkipped is the headline end-to-end
// guard: a failing statfs (nonexistent path) raises DiskError and the broken
// metric is skipped by the evaluator, while a genuine reading still fires.
func TestSW2_1_DiskProbeFailure_FlaggedAndAlertSkipped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-mount-point")

	// Real collector, real failure path.
	var broken SystemResources
	c := &DefaultSystemCollector{}
	c.collectDiskLinuxFromPath(&broken, missing)

	if !broken.DiskError {
		t.Fatalf("DiskError = false after a failed disk probe; want true (silent-zero not distinguished)")
	}
	if broken.DiskPercent != 0 {
		t.Fatalf("DiskPercent = %v on a failed probe; expected the leftover 0 the defect hinges on", broken.DiskPercent)
	}

	// A rule that fires on ANY resolvable reading (0% included). On the broken
	// probe it MUST be skipped (resolveMetric ok=false); pre-fix it fired on the
	// leftover 0.
	rule := ThresholdRule{Metric: "system.disk", Operator: ">=", Threshold: 0}
	if evalFires(rule, broken) {
		t.Fatalf("system.disk >= 0 FIRED on a broken disk probe; want skipped (a 0 from a broken statfs must not read as a genuine reading)")
	}

	// Negative control: a genuine reading (DiskError=false) MUST still resolve
	// and fire — the fix distinguishes probe-failure from genuine-zero, it does
	// not blanket-suppress system.disk.
	genuine := SystemResources{DiskPercent: 42}
	if !evalFires(rule, genuine) {
		t.Fatalf("system.disk >= 0 did NOT fire on a genuine 42%% reading; the fix must not suppress real readings")
	}
	// A genuine ZERO reading must also still resolve (0 >= 0 fires) — proving we
	// key off the error flag, not the value.
	if !evalFires(rule, SystemResources{DiskPercent: 0}) {
		t.Fatalf("system.disk >= 0 did NOT fire on a genuine 0%% reading; genuine zeros must remain resolvable")
	}
}

// TestSW2_1_MemoryProbeFailure_FlaggedAndAlertSkipped covers the memory
// collector's two failure modes (unreadable file AND MemTotal-only meminfo with
// neither MemAvailable nor MemFree) and the matching alert-skip.
func TestSW2_1_MemoryProbeFailure_FlaggedAndAlertSkipped(t *testing.T) {
	dir := t.TempDir()
	c := &DefaultSystemCollector{}

	// (a) unreadable /proc/meminfo.
	var brokenOpen SystemResources
	c.collectMemoryLinuxFromFile(&brokenOpen, filepath.Join(dir, "no-such-meminfo"))
	if !brokenOpen.MemoryError {
		t.Fatalf("MemoryError = false after an unreadable meminfo; want true")
	}

	// (b) MemTotal present but NEITHER MemAvailable NOR MemFree — available is
	// genuinely unknown, so a 0% MemoryPercent must be flagged, not trusted.
	memInfoNoAvail := filepath.Join(dir, "meminfo_no_avail")
	if err := os.WriteFile(memInfoNoAvail, []byte("MemTotal:       16000000 kB\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var brokenUnknown SystemResources
	c.collectMemoryLinuxFromFile(&brokenUnknown, memInfoNoAvail)
	if !brokenUnknown.MemoryError {
		t.Fatalf("MemoryError = false on a MemAvailable/MemFree-absent meminfo; want true (unknown, not genuine 0)")
	}
	if brokenUnknown.MemoryPercent != 0 {
		t.Fatalf("MemoryPercent = %v; expected leftover 0 (unknown-available)", brokenUnknown.MemoryPercent)
	}
	if brokenUnknown.MemoryTotal != 16000000*1024 {
		t.Fatalf("MemoryTotal = %d; want the read total preserved", brokenUnknown.MemoryTotal)
	}

	rule := ThresholdRule{Metric: "system.memory", Operator: ">=", Threshold: 0}
	if evalFires(rule, brokenOpen) {
		t.Fatalf("system.memory >= 0 FIRED on an unreadable-meminfo probe; want skipped")
	}
	if evalFires(rule, brokenUnknown) {
		t.Fatalf("system.memory >= 0 FIRED on an unknown-available probe; want skipped")
	}
	if !evalFires(rule, SystemResources{MemoryPercent: 55}) {
		t.Fatalf("system.memory >= 0 did NOT fire on a genuine 55%% reading; fix must not suppress real readings")
	}
}

// TestSW2_1_CPUProbeFailure_FlaggedAndAlertSkipped reproduces exactly what
// Collect() does for the CPU probe (cpu, ok := collectCPULinuxFromFileOK(...);
// CPUError = !ok) against a nonexistent /proc/stat, then asserts the alert-skip.
func TestSW2_1_CPUProbeFailure_FlaggedAndAlertSkipped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-stat")
	c := &DefaultSystemCollector{}

	// Real collector: an unreadable stat file must report ok=false (probe
	// failed) — the signal Collect() turns into CPUError.
	cpu, ok := c.collectCPULinuxFromFileOK(missing)
	if ok {
		t.Fatalf("collectCPULinuxFromFileOK ok = true on an unreadable /proc/stat; want false")
	}
	if cpu != 0 {
		t.Fatalf("CPU%% = %v on a failed probe; want the leftover 0", cpu)
	}

	// Mirror Collect()'s wiring: CPUError = !ok.
	broken := SystemResources{CPUPercent: cpu, CPUError: !ok}
	if !broken.CPUError {
		t.Fatalf("CPUError = false after a failed CPU probe; want true")
	}

	rule := ThresholdRule{Metric: "system.cpu", Operator: ">=", Threshold: 0}
	if evalFires(rule, broken) {
		t.Fatalf("system.cpu >= 0 FIRED on a broken CPU probe; want skipped (leftover 0 must not read as genuine)")
	}
	if !evalFires(rule, SystemResources{CPUPercent: 7}) {
		t.Fatalf("system.cpu >= 0 did NOT fire on a genuine 7%% reading; fix must not suppress real readings")
	}
}

// TestSW2_1_CPUReadSucceeds_NotFlagged is the counter-control proving the CPU
// error signal is keyed to a READ failure, not to a rejected (non-monotonic)
// sample: a well-formed stat whose counter is below the primed baseline returns
// (0, ok=true) — the sample is skipped WITHOUT latching CPUError.
func TestSW2_1_CPUReadSucceeds_NotFlagged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	// Full 10-field cpu line whose total is below the primed prevTotal → the
	// counter-guard rejects the sample, but the read itself SUCCEEDED.
	if err := os.WriteFile(path, []byte("cpu  100 200 300 400 500 600 700 800 900 1000\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	c := &DefaultSystemCollector{prevIdle: 9_000_000, prevTotal: 9_000_000}

	cpu, ok := c.collectCPULinuxFromFileOK(path)
	if !ok {
		t.Fatalf("ok = false on a readable-but-rejected sample; want true (read succeeded, sample skipped ≠ probe failure)")
	}
	if cpu != 0 {
		t.Fatalf("CPU%% = %v on a rejected non-monotonic sample; want 0", cpu)
	}
	if c.prevIdle != 9_000_000 || c.prevTotal != 9_000_000 {
		t.Fatalf("prev clobbered: %d/%d; last-good baseline must be preserved", c.prevIdle, c.prevTotal)
	}
}
