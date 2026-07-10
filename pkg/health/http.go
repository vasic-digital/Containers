package health

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const defaultHTTPTimeout = 10 * time.Second

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

	client := &http.Client{Timeout: timeout}

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
	defer func() { _ = resp.Body.Close() }()

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
