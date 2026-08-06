package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"digital.vasic.containers/internal/netaddr"
)

const defaultHTTPTimeout = 10 * time.Second

// CheckHTTP performs a health check by issuing an HTTP GET request to
// the target. The check passes when the response status code is less
// than 500 (i.e., the server is not experiencing an internal error).
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
		// The host half must be bracketed when it is an IPv6 literal.
		// net/http happens to recover from the unbracketed form whenever a
		// numeric port is present (url.Parse splits at the last colon), but
		// the URL it produces is still malformed: it fails url.Parse outright
		// when the port is absent or non-numeric, and it is emitted verbatim
		// into Details["url"], where any stricter consumer rejects it.
		url = fmt.Sprintf(
			"%s://%s%s", scheme,
			netaddr.JoinHostPort(target.Host, target.Port), path,
		)
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

	healthy := resp.StatusCode < http.StatusInternalServerError

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
