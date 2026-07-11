package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCollector(t *testing.T) (
	*PrometheusCollector,
	*prometheus.Registry,
) {
	t.Helper()
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)
	return c, reg
}

func TestPrometheusCollector_ImplementsInterface(t *testing.T) {
	reg := prometheus.NewRegistry()
	var c MetricsCollector = NewPrometheusCollector(reg)
	require.NotNil(t, c)
}

func TestPrometheusCollector_IncContainerStarts(t *testing.T) {
	c, reg := newTestCollector(t)

	c.IncContainerStarts("pg")
	c.IncContainerStarts("pg")
	c.IncContainerStarts("redis")

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(families, "containers_starts_total")
	require.NotNil(t, f)
	assert.Len(t, f.Metric, 2)
}

func TestPrometheusCollector_IncContainerStops(t *testing.T) {
	c, reg := newTestCollector(t)

	c.IncContainerStops("pg")

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(families, "containers_stops_total")
	require.NotNil(t, f)
	assert.Len(t, f.Metric, 1)
}

func TestPrometheusCollector_IncContainerFailures(t *testing.T) {
	c, reg := newTestCollector(t)

	c.IncContainerFailures("redis")
	c.IncContainerFailures("redis")
	c.IncContainerFailures("redis")

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(families, "containers_failures_total")
	require.NotNil(t, f)
	require.Len(t, f.Metric, 1)
	assert.Equal(
		t,
		float64(3),
		f.Metric[0].GetCounter().GetValue(),
	)
}

func TestPrometheusCollector_ObserveHealthCheckDuration(
	t *testing.T,
) {
	c, reg := newTestCollector(t)

	c.ObserveHealthCheckDuration("pg", 50*time.Millisecond)
	c.ObserveHealthCheckDuration("pg", 120*time.Millisecond)

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(
		families,
		"containers_health_check_duration_seconds",
	)
	require.NotNil(t, f)
	require.Len(t, f.Metric, 1)
	assert.Equal(
		t,
		uint64(2),
		f.Metric[0].GetHistogram().GetSampleCount(),
	)
}

func TestPrometheusCollector_ObserveBootDuration(t *testing.T) {
	c, reg := newTestCollector(t)

	c.ObserveBootDuration(3 * time.Second)

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(
		families,
		"containers_boot_duration_seconds",
	)
	require.NotNil(t, f)
	require.Len(t, f.Metric, 1)
	assert.InDelta(
		t,
		3.0,
		f.Metric[0].GetHistogram().GetSampleSum(),
		0.01,
	)
}

func TestPrometheusCollector_SetContainerUp(t *testing.T) {
	c, reg := newTestCollector(t)

	c.SetContainerUp("pg", true)
	assertGaugeValue(t, reg, "containers_up", "pg", 1.0)

	c.SetContainerUp("pg", false)
	assertGaugeValue(t, reg, "containers_up", "pg", 0.0)
}

func TestPrometheusCollector_MultipleContainers(t *testing.T) {
	c, reg := newTestCollector(t)

	c.IncContainerStarts("pg")
	c.IncContainerStarts("redis")
	c.SetContainerUp("pg", true)
	c.SetContainerUp("redis", true)

	families, err := reg.Gather()
	require.NoError(t, err)

	f := findFamily(families, "containers_up")
	require.NotNil(t, f)
	assert.Len(t, f.Metric, 2)
}

func TestPrometheusCollector_NilRegisterer(t *testing.T) {
	// This test covers the branch where reg is nil and uses the default
	// registerer. Reconciled per §11.4.120 for the Wave-20 MET-1 fix: the
	// constructor used to PANIC on a duplicate registration (the old body
	// wrapped this in recover() and logged "expected panic"). MET-1 made
	// duplicate registration non-fatal via registerOrExisting, so this test
	// now asserts the NEW guarantee — repeated NewPrometheusCollector(nil)
	// against the default registerer must NOT panic and must return usable
	// collectors. Real prometheus.DefaultRegisterer, no mock (§11.4.107).
	assert.NotPanics(t, func() {
		c1 := NewPrometheusCollector(nil)
		require.NotNil(t, c1)
		// Second construction on the same (default) registerer would panic
		// pre-MET-1; post-fix it reuses the existing collectors.
		c2 := NewPrometheusCollector(nil)
		require.NotNil(t, c2)
		// Both collectors must remain usable.
		c1.IncContainerStarts("nil-reg-test")
		c2.IncContainerStarts("nil-reg-test")
	})
}

// findFamily searches gathered metric families by name.
func findFamily(
	families []*dto.MetricFamily,
	name string,
) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// assertGaugeValue checks that a gauge with the given name and
// label value matches the expected value.
func assertGaugeValue(
	t *testing.T,
	reg *prometheus.Registry,
	metricName, labelValue string,
	expected float64,
) {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)

	for _, f := range families {
		if f.GetName() == metricName {
			for _, m := range f.Metric {
				for _, l := range m.GetLabel() {
					if l.GetValue() == labelValue {
						assert.Equal(
							t,
							expected,
							m.GetGauge().GetValue(),
						)
						return
					}
				}
			}
		}
	}
	t.Errorf(
		"metric %s{name=%q} not found",
		metricName, labelValue,
	)
}
