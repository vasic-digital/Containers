package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// HealthStatus captures the outcome of a single health check.
type HealthStatus struct {
	Healthy bool
	Message string
}

// HelixServiceHealthChecker implements HealthChecker for a Helix infrastructure service.
type HelixServiceHealthChecker struct {
	ServiceName string
	CheckType   string // tcp, http, grpc
	Host        string
	Port        int
	Path        string // for HTTP checks
	Timeout     time.Duration
	Retries     int
}

// NewHelixServiceHealthChecker creates a health checker for a named Helix service.
func NewHelixServiceHealthChecker(serviceName string) *HelixServiceHealthChecker {
	configs := map[string]*HelixServiceHealthChecker{
		"postgres-primary": {ServiceName: "postgres-primary", CheckType: "tcp", Host: "localhost", Port: 5432, Timeout: 5 * time.Second, Retries: 5},
		"postgres-replica": {ServiceName: "postgres-replica", CheckType: "tcp", Host: "localhost", Port: 5433, Timeout: 5 * time.Second, Retries: 5},
		"redis-master-1":   {ServiceName: "redis-master-1", CheckType: "tcp", Host: "localhost", Port: 6379, Timeout: 3 * time.Second, Retries: 5},
		"redis-master-2":   {ServiceName: "redis-master-2", CheckType: "tcp", Host: "localhost", Port: 6380, Timeout: 3 * time.Second, Retries: 5},
		"redis-master-3":   {ServiceName: "redis-master-3", CheckType: "tcp", Host: "localhost", Port: 6381, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-1":  {ServiceName: "redis-replica-1", CheckType: "tcp", Host: "localhost", Port: 6390, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-2":  {ServiceName: "redis-replica-2", CheckType: "tcp", Host: "localhost", Port: 6391, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-3":  {ServiceName: "redis-replica-3", CheckType: "tcp", Host: "localhost", Port: 6392, Timeout: 3 * time.Second, Retries: 5},
		"etcd-1":           {ServiceName: "etcd-1", CheckType: "http", Host: "localhost", Port: 2379, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"etcd-2":           {ServiceName: "etcd-2", CheckType: "http", Host: "localhost", Port: 2381, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"etcd-3":           {ServiceName: "etcd-3", CheckType: "http", Host: "localhost", Port: 2382, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"nats":             {ServiceName: "nats", CheckType: "http", Host: "localhost", Port: 8222, Path: "/healthz", Timeout: 3 * time.Second, Retries: 5},
		"kafka-1":          {ServiceName: "kafka-1", CheckType: "tcp", Host: "localhost", Port: 9092, Timeout: 5 * time.Second, Retries: 5},
		"kafka-2":          {ServiceName: "kafka-2", CheckType: "tcp", Host: "localhost", Port: 9093, Timeout: 5 * time.Second, Retries: 5},
		"kafka-3":          {ServiceName: "kafka-3", CheckType: "tcp", Host: "localhost", Port: 9094, Timeout: 5 * time.Second, Retries: 5},
		"rabbitmq":         {ServiceName: "rabbitmq", CheckType: "http", Host: "localhost", Port: 15672, Path: "/api/health/checks/virtual-hosts", Timeout: 5 * time.Second, Retries: 5},
		"prometheus":       {ServiceName: "prometheus", CheckType: "http", Host: "localhost", Port: 9090, Path: "/-/healthy", Timeout: 5 * time.Second, Retries: 5},
		"grafana":          {ServiceName: "grafana", CheckType: "http", Host: "localhost", Port: 3000, Path: "/api/health", Timeout: 5 * time.Second, Retries: 5},
		"jaeger":           {ServiceName: "jaeger", CheckType: "http", Host: "localhost", Port: 16686, Path: "/", Timeout: 5 * time.Second, Retries: 5},
		"vault":            {ServiceName: "vault", CheckType: "http", Host: "localhost", Port: 8200, Path: "/v1/sys/health", Timeout: 5 * time.Second, Retries: 5},
	}
	c, ok := configs[serviceName]
	if !ok {
		return nil
	}
	applyHelixServiceEnvOverrides(c)
	return c
}

// helixHealthEnvPrefix namespaces the per-service Host/Port/Path override
// environment variables (Wave-20 HE-4, §6.R). No connection literal is
// hardcoded here; the map above supplies documented FALLBACK DEFAULTS so
// existing callers keep compiling and behaving identically when the
// corresponding env var is unset — the override only takes effect when an
// operator/deployment explicitly sets it.
const helixHealthEnvPrefix = "HELIX_HEALTH_"

// helixServiceEnvKey builds the env-var name for a given service + field
// suffix, e.g. service "postgres-primary" + suffix "HOST" ->
// "HELIX_HEALTH_POSTGRES_PRIMARY_HOST".
func helixServiceEnvKey(serviceName, suffix string) string {
	key := strings.ToUpper(strings.ReplaceAll(serviceName, "-", "_"))
	return helixHealthEnvPrefix + key + "_" + suffix
}

// applyHelixServiceEnvOverrides overlays Host/Port/Path with values from
// the service's namespaced env vars, when set and valid. The struct's
// literal defaults (from the configs map) are left untouched — and thus
// still in full effect — whenever the corresponding env var is absent,
// empty, or (for Port) not a valid positive integer.
func applyHelixServiceEnvOverrides(c *HelixServiceHealthChecker) {
	if v := os.Getenv(helixServiceEnvKey(c.ServiceName, "HOST")); v != "" {
		c.Host = v
	}
	if v := os.Getenv(helixServiceEnvKey(c.ServiceName, "PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			c.Port = p
		}
	}
	if v := os.Getenv(helixServiceEnvKey(c.ServiceName, "PATH")); v != "" {
		c.Path = v
	}
}

// Check performs the health check.
func (h *HelixServiceHealthChecker) Check(ctx context.Context) (HealthStatus, error) {
	if h == nil {
		return HealthStatus{Healthy: false, Message: "nil checker"}, fmt.Errorf("nil checker")
	}

	var lastErr error
	for i := 0; i <= h.Retries; i++ {
		// Bail out promptly if the caller's context is already done,
		// rather than running every remaining attempt + sleep.
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}
		status, err := h.checkOnce(ctx)
		if err == nil && status.Healthy {
			return status, nil
		}
		lastErr = err
		if i < h.Retries {
			// Honor context cancellation during the inter-retry wait
			// instead of an unconditional time.Sleep.
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				return HealthStatus{
					Healthy: false,
					Message: fmt.Sprintf(
						"service %s health check cancelled: %v",
						h.ServiceName, lastErr,
					),
				}, lastErr
			case <-time.After(time.Second):
			}
		}
	}
	return HealthStatus{
		Healthy: false,
		Message: fmt.Sprintf("service %s unhealthy after %d retries: %v", h.ServiceName, h.Retries, lastErr),
	}, lastErr
}

func (h *HelixServiceHealthChecker) checkOnce(ctx context.Context) (HealthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	addr := net.JoinHostPort(h.Host, fmt.Sprintf("%d", h.Port))

	switch h.CheckType {
	case "tcp":
		// Use a Dialer bound to ctx (not net.DialTimeout, which takes no
		// context and cannot observe cancellation) so the ctx above
		// (already timeout-bounded, and cancellable by the caller) can
		// abort the dial promptly instead of always blocking for the
		// full h.Timeout (Wave-20 HE-1).
		dialer := &net.Dialer{Timeout: h.Timeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return HealthStatus{Healthy: false, Message: err.Error()}, err
		}
		conn.Close()
		return HealthStatus{Healthy: true, Message: "tcp ok"}, nil

	case "http":
		url := fmt.Sprintf("http://%s%s", addr, h.Path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return HealthStatus{Healthy: false, Message: err.Error()}, err
		}
		client := &http.Client{
			Timeout: h.Timeout,
			// Do not transparently follow redirects (HE-3): the message
			// below reports the status as if it came from addr/url — a
			// silently-followed redirect could hand back a DIFFERENT
			// server's status code under the original target's name.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			return HealthStatus{Healthy: false, Message: err.Error()}, err
		}
		defer func() {
			// Drain (bounded) before Close so the shared
			// http.DefaultTransport can pool/reuse the underlying
			// connection (HE-2) instead of a fresh dial per check.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
			_ = resp.Body.Close()
		}()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return HealthStatus{Healthy: true, Message: fmt.Sprintf("http %d", resp.StatusCode)}, nil
		}
		return HealthStatus{Healthy: false, Message: fmt.Sprintf("http %d", resp.StatusCode)}, fmt.Errorf("HTTP %d", resp.StatusCode)

	default:
		return HealthStatus{Healthy: false, Message: "unknown check type"}, fmt.Errorf("unknown check type: %s", h.CheckType)
	}
}

// AllHelixHealthCheckers returns health checkers for all 20 services.
func AllHelixHealthCheckers() map[string]*HelixServiceHealthChecker {
	services := []string{
		"postgres-primary", "postgres-replica",
		"redis-master-1", "redis-master-2", "redis-master-3",
		"redis-replica-1", "redis-replica-2", "redis-replica-3",
		"etcd-1", "etcd-2", "etcd-3",
		"nats", "kafka-1", "kafka-2", "kafka-3",
		"rabbitmq", "prometheus", "grafana", "jaeger", "vault",
	}
	checkers := make(map[string]*HelixServiceHealthChecker, len(services))
	for _, name := range services {
		if c := NewHelixServiceHealthChecker(name); c != nil {
			checkers[name] = c
		}
	}
	return checkers
}
