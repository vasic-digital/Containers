package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckHTTP_Healthy_200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := HealthTarget{
		Name:    "test-http-200",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.True(t, result.Healthy)
	assert.Equal(t, "test-http-200", result.Target)
	assert.Empty(t, result.Error)
	assert.Equal(t, "200", result.Details["status_code"])
}

func TestCheckHTTP_Unhealthy_404(t *testing.T) {
	// CT-HARDEN-60: a 404 means the health path is wrong / the service is
	// mis-pointed — it is UNHEALTHY, not "running so healthy". (Reconciled
	// from the prior TestCheckHTTP_Healthy_404 which asserted 404<500=healthy.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	target := HealthTarget{
		Name:    "test-http-404",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.False(t, result.Healthy)
	assert.Equal(t, "404", result.Details["status_code"])
	assert.Contains(t, result.Error, "unhealthy status code: 404")
}

func TestCheckHTTP_Unhealthy_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	target := HealthTarget{
		Name:    "test-http-500",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.False(t, result.Healthy)
	assert.Contains(t, result.Error, "unhealthy status code: 500")
}

func TestCheckHTTP_Unhealthy_ConnectionRefused(t *testing.T) {
	target := HealthTarget{
		Name:    "test-http-refused",
		URL:     "http://127.0.0.1:1/health",
		Type:    HealthHTTP,
		Timeout: 500 * time.Millisecond,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.False(t, result.Healthy)
	assert.Contains(t, result.Error, "http request failed")
}

func TestCheckHTTP_TableDriven(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		healthy    bool
	}{
		{"200 OK", http.StatusOK, true},
		{"201 Created", http.StatusCreated, true},
		{"204 No Content", http.StatusNoContent, true},
		{"301 Redirect", http.StatusMovedPermanently, true},
		// CT-HARDEN-60: 4xx is UNHEALTHY (was healthy under the old <500).
		{"400 Bad Request", http.StatusBadRequest, false},
		{"403 Forbidden", http.StatusForbidden, false},
		{"500 Internal Server Error", http.StatusInternalServerError, false},
		{"502 Bad Gateway", http.StatusBadGateway, false},
		{"503 Service Unavailable", http.StatusServiceUnavailable, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.statusCode)
				},
			))
			defer srv.Close()

			target := HealthTarget{
				Name:    tt.name,
				URL:     srv.URL,
				Type:    HealthHTTP,
				Timeout: 2 * time.Second,
			}

			ctx := context.Background()
			result := CheckHTTP(ctx, target)
			assert.Equal(t, tt.healthy, result.Healthy)
		})
	}
}

// TestCheckHTTP_Regression_4xx_Unhealthy_CT60 is the permanent §11.4.135
// regression guard for CT-HARDEN-60: an HTTP health check must NOT report a
// 4xx response (auth failure / wrong path / rate-limited) as healthy, while
// 2xx/3xx stay healthy and 5xx stays unhealthy. Mutating the production
// predicate back to `< 500` (or to 2xx-only) flips one of these rows and
// FAILs this guard — a real polarity oracle, not a test that agrees with the
// fix.
func TestCheckHTTP_Regression_4xx_Unhealthy_CT60(t *testing.T) {
	cases := []struct {
		code    int
		healthy bool
	}{
		{http.StatusOK, true},                  // 200
		{http.StatusMovedPermanently, true},    // 301 — reachable/redirect
		{http.StatusUnauthorized, false},       // 401
		{http.StatusForbidden, false},          // 403
		{http.StatusNotFound, false},           // 404
		{http.StatusTooManyRequests, false},    // 429
		{http.StatusServiceUnavailable, false}, // 503
	}
	for _, c := range cases {
		code := c.code
		srv := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) },
		))
		target := HealthTarget{
			Name: "ct60", URL: srv.URL, Type: HealthHTTP, Timeout: 2 * time.Second,
		}
		result := CheckHTTP(context.Background(), target)
		assert.Equal(t, c.healthy, result.Healthy,
			"status %d: expected healthy=%v", code, c.healthy)
		if !c.healthy {
			assert.Contains(t, result.Error, "unhealthy status code",
				"status %d: unhealthy result must carry an honest error", code)
		}
		srv.Close()
	}
}

func TestCheckHTTP_ConstructsURLFromHostPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract host and port from test server.
	require.NotEmpty(t, srv.URL)
	// srv.Listener.Addr() gives us host:port
	addr := srv.Listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	target := HealthTarget{
		Name:    "host-port-test",
		Host:    host,
		Port:    port,
		Path:    "/healthz",
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)
	assert.True(t, result.Healthy)
}

func TestCheckHTTP_InvalidURL(t *testing.T) {
	target := HealthTarget{
		Name:    "bad-url",
		URL:     "://not-a-url",
		Type:    HealthHTTP,
		Timeout: 500 * time.Millisecond,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)
	assert.False(t, result.Healthy)
}

func TestCheckHTTP_DefaultTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	target := HealthTarget{
		Name:    "default-timeout",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: 0, // Should use defaultHTTPTimeout (10s).
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.True(t, result.Healthy)
}

func TestCheckHTTP_DefaultPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// When no Path is specified, should default to "/".
			assert.Equal(t, "/", r.URL.Path)
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	target := HealthTarget{
		Name:    "default-path",
		Host:    host,
		Port:    port,
		Path:    "", // Empty path should default to "/".
		Type:    HealthHTTP,
		Timeout: 2 * time.Second,
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.True(t, result.Healthy)
}

func TestCheckHTTP_NegativeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	))
	defer srv.Close()

	target := HealthTarget{
		Name:    "negative-timeout",
		URL:     srv.URL,
		Type:    HealthHTTP,
		Timeout: -1 * time.Second, // Negative should use default.
	}

	ctx := context.Background()
	result := CheckHTTP(ctx, target)

	assert.True(t, result.Healthy)
}
