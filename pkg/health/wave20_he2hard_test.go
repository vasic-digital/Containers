package health

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the permanent §11.4.115 GREEN-polarity guard suite for the
// Wave-20 pkg/health hardening batch (HE-1, HE-2, HE-3, HE-4, HE-5). Every
// test here asserts the FIXED behavior; each finding was reproduced first
// (RED, on the pre-fix code, via surgical revert) before the corresponding
// fix landed — see the conductor report for the captured RED evidence.

// countingListener wraps a net.Listener and counts Accept() calls, so a
// test can assert how many distinct TCP connections a server actually
// received — the load-bearing oracle for HE-2 (connection-reuse via
// keep-alive draining).
type countingListener struct {
	net.Listener
	accepts int32
}

func (c *countingListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err == nil {
		atomic.AddInt32(&c.accepts, 1)
	}
	return conn, err
}

// ---------------------------------------------------------------------
// HE-1: CheckTCP / checkOnce("tcp") must use a ctx-aware Dialer, not the
// uncancellable net.DialTimeout.
// ---------------------------------------------------------------------

func TestCheckTCP_CancelWithoutDeadline_AbortsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	target := HealthTarget{
		Name:    "he1-tcp",
		Host:    "192.0.2.1", // TEST-NET-1, non-routable: the dial hangs until it times out
		Port:    "12345",
		Type:    HealthTCP,
		Timeout: 3 * time.Second,
	}

	start := time.Now()
	result := CheckTCP(ctx, target)
	elapsed := time.Since(start)

	assert.False(t, result.Healthy)
	assert.Less(t, elapsed, 1*time.Second,
		"CheckTCP must observe ctx cancellation via DialContext, not block for "+
			"the full un-cancellable net.DialTimeout duration")
}

func TestHelixServiceHealthChecker_TCPCheckOnce_CancelWithoutDeadline_AbortsPromptly(t *testing.T) {
	h := &HelixServiceHealthChecker{
		ServiceName: "he1-infra-tcp",
		CheckType:   "tcp",
		Host:        "192.0.2.1",
		Port:        12345,
		Timeout:     3 * time.Second,
		Retries:     0,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	status, err := h.Check(ctx)
	elapsed := time.Since(start)

	assert.False(t, status.Healthy)
	assert.Error(t, err)
	assert.Less(t, elapsed, 1*time.Second,
		"checkOnce's tcp dial must observe ctx cancellation via DialContext, "+
			"not net.DialTimeout's own uncancellable timeout")
}

// ---------------------------------------------------------------------
// HE-2: response bodies must be drained (bounded) before Close so the
// shared http.DefaultTransport can pool/reuse the connection.
// ---------------------------------------------------------------------

func TestCheckHTTP_DrainsBodyBeforeClose_ReusesConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cl := &countingListener{Listener: ln}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("this response body must be drained for keep-alive reuse to work"))
		},
	))
	_ = srv.Listener.Close()
	srv.Listener = cl
	srv.Start()
	defer srv.Close()

	target := HealthTarget{
		Name:    "he2-http-drain",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	for i := 0; i < 5; i++ {
		result := CheckHTTP(context.Background(), target)
		require.True(t, result.Healthy, "iteration %d", i)
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&cl.accepts),
		"5 sequential CheckHTTP calls to the same target must reuse one pooled "+
			"keep-alive connection when the body is drained before Close")
}

func TestHelixServiceHealthChecker_HTTPCheckOnce_DrainsBodyBeforeClose_ReusesConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cl := &countingListener{Listener: ln}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("body that must be drained for the infra http checkOnce path too"))
		},
	))
	_ = srv.Listener.Close()
	srv.Listener = cl
	srv.Start()
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := &HelixServiceHealthChecker{
		ServiceName: "he2-infra-http",
		CheckType:   "http",
		Host:        host,
		Port:        portNum,
		Path:        "/",
		Timeout:     2 * time.Second,
		Retries:     0,
	}

	for i := 0; i < 5; i++ {
		status, err := h.Check(context.Background())
		require.NoError(t, err, "iteration %d", i)
		require.True(t, status.Healthy, "iteration %d", i)
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&cl.accepts),
		"5 sequential checkOnce http calls must reuse one pooled keep-alive "+
			"connection when the body is drained before Close")
}

// ---------------------------------------------------------------------
// HE-3: a health check must certify exactly the NAMED target — it must
// not silently follow a redirect to a different server and report that
// server's status/evidence as if it belonged to the original target.
// ---------------------------------------------------------------------

func TestCheckHTTP_DoesNotFollowRedirect_HonestTargetEvidence(t *testing.T) {
	var unrelatedHits int32
	unrelatedSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&unrelatedHits, 1)
			w.WriteHeader(http.StatusOK)
		},
	))
	defer unrelatedSrv.Close()

	targetSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, unrelatedSrv.URL, http.StatusFound)
		},
	))
	defer targetSrv.Close()

	target := HealthTarget{
		Name:    "he3-http",
		URL:     targetSrv.URL,
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	result := CheckHTTP(context.Background(), target)

	assert.True(t, result.Healthy, "302 from the named target is itself within the healthy 2xx-3xx range")
	assert.Equal(t, "302", result.Details["status_code"],
		"the result must certify the NAMED target's own status, not a silently-followed redirect target's")
	assert.Equal(t, targetSrv.URL, result.Details["url"],
		"Details[\"url\"] must keep reporting the original target, never the followed redirect target")
	assert.Equal(t, int32(0), atomic.LoadInt32(&unrelatedHits),
		"CheckHTTP must not silently follow the redirect to a different server")
}

func TestHelixServiceHealthChecker_HTTPCheckOnce_DoesNotFollowRedirect(t *testing.T) {
	var unrelatedHits int32
	unrelatedSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&unrelatedHits, 1)
			w.WriteHeader(http.StatusOK)
		},
	))
	defer unrelatedSrv.Close()

	targetSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, unrelatedSrv.URL, http.StatusFound)
		},
	))
	defer targetSrv.Close()

	host, port, err := net.SplitHostPort(targetSrv.Listener.Addr().String())
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	require.NoError(t, err)

	h := &HelixServiceHealthChecker{
		ServiceName: "he3-infra-http",
		CheckType:   "http",
		Host:        host,
		Port:        portNum,
		Path:        "/",
		Timeout:     2 * time.Second,
		Retries:     0,
	}

	status, err := h.Check(context.Background())

	// checkOnce's http branch is 2xx-only, so an (un-followed) 302 is
	// correctly reported unhealthy here — the load-bearing assertion is
	// that the unrelated server was never contacted.
	assert.False(t, status.Healthy)
	assert.Error(t, err)
	assert.Contains(t, status.Message, "302")
	assert.Equal(t, int32(0), atomic.LoadInt32(&unrelatedHits),
		"checkOnce must not silently follow the redirect to a different server "+
			"and attribute that server's response to the named target")
}

// ---------------------------------------------------------------------
// HE-4: NewHelixServiceHealthChecker's Host/Port/Path must be overridable
// via namespaced env vars, with the existing literals preserved as
// documented fallback defaults (§6.R, backward-compatible).
// ---------------------------------------------------------------------

func TestNewHelixServiceHealthChecker_EnvOverride_HostAndPort(t *testing.T) {
	t.Setenv("HELIX_HEALTH_POSTGRES_PRIMARY_HOST", "10.0.0.5")
	t.Setenv("HELIX_HEALTH_POSTGRES_PRIMARY_PORT", "6543")

	h := NewHelixServiceHealthChecker("postgres-primary")
	require.NotNil(t, h)
	assert.Equal(t, "10.0.0.5", h.Host)
	assert.Equal(t, 6543, h.Port)
	assert.Equal(t, "tcp", h.CheckType, "fields with no override must keep their default")
}

func TestNewHelixServiceHealthChecker_EnvOverride_PathOnly(t *testing.T) {
	t.Setenv("HELIX_HEALTH_ETCD_1_PATH", "/custom-health")

	h := NewHelixServiceHealthChecker("etcd-1")
	require.NotNil(t, h)
	assert.Equal(t, "/custom-health", h.Path)
	assert.Equal(t, "localhost", h.Host, "Host must keep its default when only PATH is overridden")
}

func TestNewHelixServiceHealthChecker_NoEnvOverride_DefaultsPreserved(t *testing.T) {
	h := NewHelixServiceHealthChecker("redis-master-1")
	require.NotNil(t, h)
	assert.Equal(t, "localhost", h.Host)
	assert.Equal(t, 6379, h.Port)
}

func TestNewHelixServiceHealthChecker_EnvOverride_InvalidPortIgnored(t *testing.T) {
	t.Setenv("HELIX_HEALTH_KAFKA_1_PORT", "not-a-number")

	h := NewHelixServiceHealthChecker("kafka-1")
	require.NotNil(t, h)
	assert.Equal(t, 9092, h.Port,
		"a non-numeric PORT override must be ignored, keeping the documented default")
}

func TestNewHelixServiceHealthChecker_EnvOverride_NonPositivePortIgnored(t *testing.T) {
	t.Setenv("HELIX_HEALTH_KAFKA_2_PORT", "0")

	h := NewHelixServiceHealthChecker("kafka-2")
	require.NotNil(t, h)
	assert.Equal(t, 9093, h.Port,
		"a non-positive PORT override must be ignored, keeping the documented default")
}

// ---------------------------------------------------------------------
// HE-5: exponential backoff must clamp to a sane ceiling and never trust
// a negative/non-finite result of the float64->time.Duration conversion.
// ---------------------------------------------------------------------

func TestNextRetryDelay_Int64Overflow_ClampsToMaxCeiling(t *testing.T) {
	// Empirically confirmed (scratch probe): float64(math.MaxInt64)*2.0
	// converts back to time.Duration as math.MinInt64 (a huge negative
	// value) on this platform — the exact overflow this guards against.
	d := nextRetryDelay(time.Duration(math.MaxInt64), 2.0)
	assert.Equal(t, maxRetryDelay, d)
	assert.GreaterOrEqual(t, d, time.Duration(0))
}

func TestNextRetryDelay_HugeBackoffFactor_ClampsToMaxCeiling(t *testing.T) {
	// delay=1ms * backoffFactor=1e300 overflows on the very FIRST
	// multiplication (empirically confirmed: yields math.MinInt64).
	d := nextRetryDelay(1*time.Millisecond, 1e300)
	assert.Equal(t, maxRetryDelay, d)
}

func TestNextRetryDelay_NormalGrowth_Unaffected(t *testing.T) {
	assert.Equal(t, 100*time.Millisecond, nextRetryDelay(50*time.Millisecond, 2.0))
}

func TestNextRetryDelay_ZeroOrNegativeBackoffFactor_KeepsDelayUnchanged(t *testing.T) {
	assert.Equal(t, 50*time.Millisecond, nextRetryDelay(50*time.Millisecond, 0))
	assert.Equal(t, 50*time.Millisecond, nextRetryDelay(50*time.Millisecond, -3))
}

func TestNextRetryDelay_ExceedsCeiling_ClampsDown(t *testing.T) {
	d := nextRetryDelay(9*time.Minute, 3.0) // 27 minutes, well past the 10-minute ceiling
	assert.Equal(t, maxRetryDelay, d)
}

// TestCheckWithRetry_OverflowingBackoff_ClampedNotBusyLoop is the
// end-to-end discriminator: pre-fix, the overflowed (negative) delay makes
// time.After fire near-instantly every iteration, so all MaxRetries burn
// through in a busy-loop well before the 300ms ctx timeout ever fires
// (calls == MaxRetries+1, no "context cancelled" in the error). Post-fix,
// the clamp makes the (bounded, positive) delay long enough that ctx
// cancellation — not the loop exhausting its retries — governs when
// CheckWithRetry returns (calls < MaxRetries+1, "context cancelled" present).
func TestCheckWithRetry_OverflowingBackoff_ClampedNotBusyLoop(t *testing.T) {
	c := NewDefaultChecker()
	var calls int32
	c.Register(HealthCustom, func(
		_ context.Context, target HealthTarget,
	) *HealthResult {
		atomic.AddInt32(&calls, 1)
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Error:     "still failing",
			Timestamp: time.Now(),
		}
	})

	policy := RetryPolicy{
		MaxRetries:    5,
		Delay:         1 * time.Millisecond,
		BackoffFactor: 1e300,
	}
	target := HealthTarget{Name: "he5-overflow", Type: HealthCustom}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := CheckWithRetry(ctx, c, target, policy)
	elapsed := time.Since(start)

	assert.False(t, result.Healthy)
	assert.Less(t, elapsed, 1*time.Second,
		"the clamp must keep the backoff bounded enough that ctx cancellation "+
			"(300ms) governs when CheckWithRetry returns")
	assert.Contains(t, result.Error, "context cancelled during retry",
		"with the ceiling clamp in effect, ctx must win the select against the "+
			"(large but bounded) clamped delay before all retries burn through instantly")
	assert.Less(t, atomic.LoadInt32(&calls), int32(policy.MaxRetries+1),
		"the clamp must make the retry loop wait long enough for ctx to cut it "+
			"off before exhausting every retry in a busy-loop")
}
