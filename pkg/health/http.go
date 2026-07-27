package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultHTTPTimeout = 10 * time.Second

// maxDrainBytes bounds how much of a response body is read (to let
// http.Transport reuse the underlying TCP connection via keep-alive)
// before Close — never an unbounded io.Copy that could stall on a huge
// or slow-writing body (Wave-20 HE-2). Shared by helix_infra.go.
const maxDrainBytes = 64 * 1024

// CheckHTTP performs a health check by issuing an HTTP GET request to
// the target. The check passes only for a 2xx or 3xx response status
// (200-399): a healthy endpoint answers its health path successfully or
// redirects. A 4xx (e.g. 401/403/404/429 — auth failure, wrong path,
// rate-limited) or 5xx (server error) is reported UNHEALTHY. A bare
// "< 500" predicate would greenlight a 404 from a mis-pointed health
// URL, masking a broken service (Wave-18 CT-HARDEN-60); the sibling
// HelixServiceHealthChecker already uses 2xx-only for the same reason.
func CheckHTTP(ctx context.Context, target HealthTarget) *HealthResult {
	start := time.Now()
	url := target.ResolvedAddress()

	// When no full URL is provided, construct one from host:port + path.
	if target.URL == "" {
		scheme := "http"
		path := target.Path
		if path == "" {
			path = "/"
		}
		url = fmt.Sprintf("%s://%s:%s%s", scheme, target.Host, target.Port, path)
	}

	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}

	client := &http.Client{
		Timeout: timeout,
		// Do not transparently follow redirects (HE-3): a health check
		// must certify exactly the NAMED target. Without this, the
		// default client follows up to 10 cross-host redirects while
		// Details["url"] below still reports the ORIGINAL target — a
		// Healthy verdict could come from a completely different
		// server than the one claimed as evidence.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Duration:  time.Since(start),
			Error:     fmt.Sprintf("failed to create request: %v", err),
			Timestamp: start,
		}
	}

	resp, err := client.Do(req)
	duration := time.Since(start)

	if err != nil {
		return &HealthResult{
			Target:    target.Name,
			Healthy:   false,
			Duration:  duration,
			Error:     fmt.Sprintf("http request failed: %v", err),
			Timestamp: start,
		}
	}
	defer func() {
		// Drain (bounded) before Close so the shared http.DefaultTransport
		// can pool/reuse the underlying connection (HE-2) instead of
		// opening a fresh TCP(+TLS) socket on every subsequent check.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		_ = resp.Body.Close()
	}()

	// 2xx-3xx (200-399) is healthy; 4xx and 5xx are not (CT-HARDEN-60).
	healthy := resp.StatusCode >= http.StatusOK &&
		resp.StatusCode < http.StatusBadRequest

	result := &HealthResult{
		Target:    target.Name,
		Healthy:   healthy,
		Duration:  duration,
		Timestamp: start,
		Details: map[string]string{
			"url":         url,
			"status_code": fmt.Sprintf("%d", resp.StatusCode),
		},
	}

	if !healthy {
		result.Error = fmt.Sprintf(
			"unhealthy status code: %d", resp.StatusCode,
		)
	}

	return result
}
