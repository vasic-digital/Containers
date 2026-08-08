package metrics

// Wave-20 DEEPER batch CT-HARDEN-MET2-HARD regression guards (§11.4.115
// GREEN-polarity: the committed default asserts the FIXED behavior; reverting
// the single-line source anchor reproduces the pre-fix RED). Each guard drives
// the real PrometheusCollector against a real prometheus.NewRegistry()+Gather()
// — NO mock (§11.4.107 honest boundary): the guards prove the label-value
// UTF-8 sanitization end-to-end, not a stub.
//
//   MET2-1 defect (NEW; missed by MET-1/2/3): every recording method funnels a
//         caller-supplied name straight into *Vec.WithLabelValues, which PANICS
//         in prometheus/client_golang when the value is not valid UTF-8 —
//         crashing the whole process on the metrics hot path. Root cause: no
//         label-value validation at the library boundary. Fix: sanitizeLabelValue
//         coerces invalid UTF-8 to U+FFFD before every WithLabelValues /
//         DeleteLabelValues. Pre-fix RED = a PANIC (caught by assert.NotPanics,
//         which then FAILs the guard).

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// badUTF8Name is a deliberately invalid-UTF-8 label value: "A\xff\xfeB". A real
// ephemeral/derived container name could carry such stray bytes.
const badUTF8Name = "A\xff\xfeB"

// TestWave20_MET2_SetContainerUpInvalidUTF8NoPanic is the primary anti-tautology
// guard for MET2-1. It targets the SetContainerUp source anchor
// ("name = sanitizeLabelValue(name) // MET2-1 anti-tautology anchor ..."):
// pre-fix (anchor reverted so the raw name reaches WithLabelValues) SetContainerUp
// PANICS ("label value ... is not valid UTF-8"); assert.NotPanics catches the
// panic and the guard FAILs (that IS the RED). Post-fix: no panic, and the
// recorded series carries a valid-UTF-8 (sanitized) name that Gather() surfaces.
func TestWave20_MET2_SetContainerUpInvalidUTF8NoPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	assert.NotPanics(t, func() {
		c.SetContainerUp(badUTF8Name, true)
	}, "SetContainerUp must not panic on an invalid-UTF-8 name")

	families, err := reg.Gather()
	require.NoError(t, err)
	f := findFamily(families, "containers_up")
	require.NotNil(t, f, "up gauge must be gatherable after recording")
	require.Len(t, f.Metric, 1)

	// The stored label value must be valid UTF-8 (proves sanitization, not just
	// no-panic) and must equal the U+FFFD-coerced form.
	wantName := strings.ToValidUTF8(badUTF8Name, "�")
	require.True(t, familyHasLabel(t, reg, "containers_up", wantName),
		"up series must carry the sanitized (valid-UTF-8) name")
	for _, m := range f.Metric {
		for _, l := range m.GetLabel() {
			if l.GetName() == "name" {
				assert.True(t, utf8.ValidString(l.GetValue()),
					"recorded label value must be valid UTF-8")
			}
		}
	}
}

// TestWave20_MET2_AllRecordMethodsInvalidUTF8NoPanic proves the fix is complete:
// every counter/histogram recording method is panic-safe on a bad-UTF-8 name,
// not only the gauge path. (Each of these methods carries its own
// sanitizeLabelValue guard line; this guard proves the whole record surface.)
func TestWave20_MET2_AllRecordMethodsInvalidUTF8NoPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	assert.NotPanics(t, func() {
		c.IncContainerStarts(badUTF8Name)
		c.IncContainerStops(badUTF8Name)
		c.IncContainerFailures(badUTF8Name)
		c.ObserveHealthCheckDuration(badUTF8Name, 5*time.Millisecond)
	}, "no recording method may panic on an invalid-UTF-8 name")

	_, err := reg.Gather()
	require.NoError(t, err)
	wantName := strings.ToValidUTF8(badUTF8Name, "�")
	require.True(t, familyHasLabel(t, reg, "containers_starts_total", wantName))
	require.True(t, familyHasLabel(t, reg, "containers_stops_total", wantName))
	require.True(t, familyHasLabel(t, reg, "containers_failures_total", wantName))
	require.True(t, familyHasLabel(t, reg,
		"containers_health_check_duration_seconds", wantName))
}

// TestWave20_MET2_ForgetEvictsSanitizedSeries proves Forget evicts the series
// that a bad-UTF-8 name was recorded under. Because record sanitizes the name,
// Forget must sanitize identically or the MET-2 cardinality bound silently
// leaks for such names. Post-fix: the sanitized up{name} series is gone after
// Forget(badUTF8Name).
func TestWave20_MET2_ForgetEvictsSanitizedSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPrometheusCollector(reg)

	c.SetContainerUp(badUTF8Name, true)
	wantName := strings.ToValidUTF8(badUTF8Name, "�")
	require.True(t, familyHasLabel(t, reg, "containers_up", wantName),
		"sanitized up series must exist before Forget")

	c.Forget(badUTF8Name)

	require.False(t, familyHasLabel(t, reg, "containers_up", wantName),
		"Forget must evict the sanitized series (no cardinality leak)")
}
