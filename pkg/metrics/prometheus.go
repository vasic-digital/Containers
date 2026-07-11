package metrics

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusCollector implements MetricsCollector using the
// Prometheus client library.
type PrometheusCollector struct {
	containerStarts   *prometheus.CounterVec
	containerStops    *prometheus.CounterVec
	containerFailures *prometheus.CounterVec
	healthCheckDur    *prometheus.HistogramVec
	bootDuration      prometheus.Histogram
	containerUp       *prometheus.GaugeVec
}

// Ensure PrometheusCollector satisfies MetricsCollector at
// compile time.
var _ MetricsCollector = (*PrometheusCollector)(nil)

// NewPrometheusCollector creates a PrometheusCollector and
// registers all metrics with the provided registerer. If reg is
// nil, prometheus.DefaultRegisterer is used.
func NewPrometheusCollector(
	reg prometheus.Registerer,
) *PrometheusCollector {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	starts := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "containers",
			Name:      "starts_total",
			Help:      "Total container start events.",
		},
		[]string{"name"},
	)
	stops := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "containers",
			Name:      "stops_total",
			Help:      "Total container stop events.",
		},
		[]string{"name"},
	)
	failures := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "containers",
			Name:      "failures_total",
			Help:      "Total container failure events.",
		},
		[]string{"name"},
	)
	healthDur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "containers",
			Name:      "health_check_duration_seconds",
			Help:      "Health check duration in seconds.",
			Buckets: prometheus.ExponentialBuckets(
				0.01, 2, 10,
			),
		},
		[]string{"name"},
	)
	boot := prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "containers",
			Name:      "boot_duration_seconds",
			Help:      "Total boot sequence duration.",
			Buckets: prometheus.ExponentialBuckets(
				0.1, 2, 12,
			),
		},
	)
	up := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "containers",
			Name:      "up",
			Help:      "Whether a container is up (1) or down (0).",
		},
		[]string{"name"},
	)

	// Register each collector individually and recover from a benign
	// duplicate-registration collision instead of panicking, so the
	// constructor never takes down its caller (§11.4.122 — public
	// signature unchanged, no error return introduced).
	return &PrometheusCollector{
		containerStarts:   registerOrExisting(reg, starts),
		containerStops:    registerOrExisting(reg, stops),
		containerFailures: registerOrExisting(reg, failures),
		healthCheckDur:    registerOrExisting(reg, healthDur),
		bootDuration:      registerOrExisting(reg, boot),
		containerUp:       registerOrExisting(reg, up),
	}
}

// registerOrExisting registers c with reg and returns the live collector to
// use. On a benign duplicate registration
// (prometheus.AlreadyRegisteredError) it returns the already-registered
// collector of the same type — the idiomatic recover-from-collision path — so
// NewPrometheusCollector never panics on a double-init (two collectors sharing
// one registry, or repeated NewPrometheusCollector(nil) against the default
// registerer) and always yields a usable collector. A non-collision error, or
// a same-name collector of an incompatible type, is treated as non-fatal: the
// freshly-built (unregistered) collector is returned; it still records
// locally, but a registration failure never becomes a caller-fatal panic.
func registerOrExisting[T prometheus.Collector](
	reg prometheus.Registerer,
	c T,
) T {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(T); ok {
				return existing
			}
		}
	}
	return c
}

// sanitizeLabelValue coerces v into a valid-UTF-8 string. prometheus
// *Vec.WithLabelValues PANICS ("label value ... is not valid UTF-8") when a
// label value contains invalid UTF-8 bytes, and *Vec.DeleteLabelValues then
// fails to match a series recorded under the sanitized value. Because this is a
// generic reusable library, the caller-supplied container name flows straight
// into the label with no upstream validation — an ephemeral or externally
// derived name carrying stray bytes would crash the whole process on the
// metrics hot path (the same panic-in-normal-call-path class the constructor
// fix closed, here on the record path). Coercing invalid runs to U+FFFD keeps
// recording panic-free AND keeps the exposition well-formed (an invalid-UTF-8
// label value also corrupts the text/OpenMetrics scrape, which a strict scraper
// drops). The valid-UTF-8 fast path allocates nothing for the common case
// (§11.4.6 — proven by probe, not assumed).
func sanitizeLabelValue(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	return strings.ToValidUTF8(v, "�")
}

// IncContainerStarts increments the start counter for the named
// container.
func (c *PrometheusCollector) IncContainerStarts(name string) {
	name = sanitizeLabelValue(name)
	c.containerStarts.WithLabelValues(name).Inc()
}

// IncContainerStops increments the stop counter for the named
// container.
func (c *PrometheusCollector) IncContainerStops(name string) {
	name = sanitizeLabelValue(name)
	c.containerStops.WithLabelValues(name).Inc()
}

// IncContainerFailures increments the failure counter for the
// named container.
func (c *PrometheusCollector) IncContainerFailures(name string) {
	name = sanitizeLabelValue(name)
	c.containerFailures.WithLabelValues(name).Inc()
}

// ObserveHealthCheckDuration records the duration of a health
// check for the named container.
func (c *PrometheusCollector) ObserveHealthCheckDuration(
	name string,
	d time.Duration,
) {
	// A negative duration (clock skew / reversed time.Since) would count
	// into every bucket AND decrement _sum, silently corrupting the
	// histogram (Prometheus has no un-observe). Clamp to zero. NaN is not
	// reachable here — d.Seconds() = int64(ns)/1e9 is always finite — so
	// only the negative case is guarded (§11.4.6, no over-claim).
	if d < 0 {
		d = 0
	}
	name = sanitizeLabelValue(name)
	c.healthCheckDur.WithLabelValues(name).Observe(d.Seconds())
}

// ObserveBootDuration records the total boot sequence duration.
func (c *PrometheusCollector) ObserveBootDuration(d time.Duration) {
	// Clamp a negative duration to zero — see ObserveHealthCheckDuration.
	if d < 0 {
		d = 0
	}
	c.bootDuration.Observe(d.Seconds())
}

// SetContainerUp sets the up gauge for the named container.
func (c *PrometheusCollector) SetContainerUp(
	name string,
	up bool,
) {
	val := 0.0
	if up {
		val = 1.0
	}
	name = sanitizeLabelValue(name) // MET2-1 anti-tautology anchor: coerce invalid UTF-8 so WithLabelValues never panics
	c.containerUp.WithLabelValues(name).Set(val)
}

// Forget evicts every per-container child series for name across the
// name-labelled vectors (up gauge, start/stop/failure counters, health-check
// duration histogram). Call it on container teardown to bound label
// cardinality (a churning/ephemeral name set otherwise grows RSS unbounded)
// and to stop a stopped container's up{name}=0 series from lingering forever
// and misreporting the fleet. bootDuration carries no name label and is
// unaffected. Forget is a PrometheusCollector-specific method — it is
// deliberately NOT part of the MetricsCollector interface (adding a method
// there would break external implementers), so a caller holding the interface
// type-asserts to *PrometheusCollector or interface{ Forget(string) } to
// invoke it.
func (c *PrometheusCollector) Forget(name string) {
	// Match the sanitized value the record methods stored, otherwise a
	// bad-UTF-8 name's series would never be evicted (MET-2 cardinality bound
	// would silently leak for such names).
	name = sanitizeLabelValue(name)
	c.containerUp.DeleteLabelValues(name)
	c.containerStarts.DeleteLabelValues(name)
	c.containerStops.DeleteLabelValues(name)
	c.containerFailures.DeleteLabelValues(name)
	c.healthCheckDur.DeleteLabelValues(name)
}
