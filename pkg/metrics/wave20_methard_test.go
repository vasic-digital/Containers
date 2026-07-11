package metrics

// Wave-20 batch CT-HARDEN-MET-HARD regression guards (§11.4.115 GREEN-polarity:
// committed default asserts the FIXED behavior). Each guard exercises the real
// PrometheusCollector against a real prometheus.NewRegistry() + Gather() — NO
// mock, real Prometheus objects (§11.4.107 honest boundary): the guards prove
// the collision-recovery / eviction / clamp LOGIC end-to-end, not a stub.
//
//   MET-1 pre-fix RED = a PANIC on duplicate MustRegister (asserted via a
//         recover() shield that flips the guard to FAIL when the panic fires).
//   MET-2 pre-fix RED = the up{name} series lingers after teardown.
//   MET-3 pre-fix RED = Observe(negative) drives the histogram _sum below zero.

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// TestWave20_MET1_ConstructorNoPanicOnDuplicateRegister proves the constructor
// no longer panics when two collectors are registered on the SAME registry
// (the duplicate-registration collision). Pre-fix: reg.MustRegister on the
// second construction PANICS; the recover() below catches it and the
// require.True FAILs (that IS the RED). Post-fix: registerOrExisting reuses the
// already-registered collectors, no panic, and the reused collector is live on
// the registry.
func TestWave20_MET1_ConstructorNoPanicOnDuplicateRegister(t *testing.T) {
	// Real registry + real collectors — no mock (§11.4.107).
	reg := prometheus.NewRegistry()

	var second *PrometheusCollector
	noPanic := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("MET-1 RED: constructor panicked on duplicate "+
					"registration: %v", r)
				ok = false
			}
		}()
		_ = NewPrometheusCollector(reg)      // first registration
		second = NewPrometheusCollector(reg) // second on SAME reg (pre-fix PANIC)
		return true
	}()

	require.True(t, noPanic,
		"NewPrometheusCollector must not panic on duplicate registration")
	require.NotNil(t, second)

	// The reused collector must be live on the registry: an increment must
	// surface via Gather().
	second.IncContainerStarts("dup")
	families, err := reg.Gather()
	require.NoError(t, err)
	f := findFamily(families, "containers_starts_total")
	require.NotNil(t, f,
		"reused collector must be registered and gatherable")
}

// TestWave20_MET2_ForgetEvictsUpSeries proves Forget(name) evicts the up{name}
// series (bounded cardinality on teardown). Pre-fix (Forget body gutted to a
// no-op during surgical revert): the series lingers and the final
// require.False FAILs. Post-fix: the series is gone after Forget.
func TestWave20_MET2_ForgetEvictsUpSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	c.SetContainerUp("ephemeral", true)
	require.True(t, gaugeSeriesPresent(t, reg, "containers_up", "ephemeral"),
		"up{name=ephemeral} must exist before Forget")

	c.Forget("ephemeral")

	require.False(t, gaugeSeriesPresent(t, reg, "containers_up", "ephemeral"),
		"up{name=ephemeral} must be evicted after Forget")
}

// TestWave20_MET2_ForgetEvictsCounters proves Forget also evicts the per-name
// counter/histogram child series, not only the up gauge.
func TestWave20_MET2_ForgetEvictsCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	c.IncContainerStarts("gone")
	c.IncContainerStops("gone")
	c.IncContainerFailures("gone")
	c.ObserveHealthCheckDuration("gone", 10*time.Millisecond)
	require.True(t, familyHasLabel(t, reg, "containers_starts_total", "gone"))

	c.Forget("gone")

	require.False(t, familyHasLabel(t, reg, "containers_starts_total", "gone"),
		"starts_total{name=gone} must be evicted after Forget")
	require.False(t, familyHasLabel(t, reg, "containers_stops_total", "gone"),
		"stops_total{name=gone} must be evicted after Forget")
	require.False(t, familyHasLabel(t, reg, "containers_failures_total", "gone"),
		"failures_total{name=gone} must be evicted after Forget")
	require.False(t, familyHasLabel(t, reg,
		"containers_health_check_duration_seconds", "gone"),
		"health_check_duration{name=gone} must be evicted after Forget")
}

// TestWave20_MET3_NegativeHealthDurationClamped proves a negative health-check
// duration is clamped so the histogram _sum never goes negative. Pre-fix:
// Observe(-1s) drives _sum to -1.0 and require.GreaterOrEqual FAILs.
func TestWave20_MET3_NegativeHealthDurationClamped(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	c.ObserveHealthCheckDuration("skew", -time.Second)

	families, err := reg.Gather()
	require.NoError(t, err)
	f := findFamily(families, "containers_health_check_duration_seconds")
	require.NotNil(t, f)
	require.GreaterOrEqual(t,
		f.Metric[0].GetHistogram().GetSampleSum(), 0.0,
		"negative health-check duration must not corrupt histogram _sum")
}

// TestWave20_MET3_NegativeBootDurationClamped proves the same clamp for the
// boot-duration histogram. Pre-fix: Observe(-2s) drives _sum to -2.0 and FAILs.
func TestWave20_MET3_NegativeBootDurationClamped(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	c.ObserveBootDuration(-2 * time.Second)

	families, err := reg.Gather()
	require.NoError(t, err)
	f := findFamily(families, "containers_boot_duration_seconds")
	require.NotNil(t, f)
	require.GreaterOrEqual(t,
		f.Metric[0].GetHistogram().GetSampleSum(), 0.0,
		"negative boot duration must not corrupt histogram _sum")
}

// gaugeSeriesPresent reports whether a gauge family carries a series with the
// given name label, using a real Gather() over the live registry.
func gaugeSeriesPresent(
	t *testing.T,
	reg *prometheus.Registry,
	family, labelValue string,
) bool {
	t.Helper()
	return familyHasLabel(t, reg, family, labelValue)
}

// familyHasLabel reports whether the named metric family carries a child series
// whose "name" label equals labelValue.
func familyHasLabel(
	t *testing.T,
	reg *prometheus.Registry,
	family, labelValue string,
) bool {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	f := findFamily(families, family)
	if f == nil {
		return false
	}
	for _, m := range f.Metric {
		for _, l := range m.GetLabel() {
			if l.GetName() == "name" && l.GetValue() == labelValue {
				return true
			}
		}
	}
	return false
}
