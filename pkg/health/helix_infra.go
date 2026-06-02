package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
		"postgres-primary":     {ServiceName: "postgres-primary", CheckType: "tcp", Host: "localhost", Port: 5432, Timeout: 5 * time.Second, Retries: 5},
		"postgres-replica":     {ServiceName: "postgres-replica", CheckType: "tcp", Host: "localhost", Port: 5433, Timeout: 5 * time.Second, Retries: 5},
		"redis-master-1":       {ServiceName: "redis-master-1", CheckType: "tcp", Host: "localhost", Port: 6379, Timeout: 3 * time.Second, Retries: 5},
		"redis-master-2":       {ServiceName: "redis-master-2", CheckType: "tcp", Host: "localhost", Port: 6380, Timeout: 3 * time.Second, Retries: 5},
		"redis-master-3":       {ServiceName: "redis-master-3", CheckType: "tcp", Host: "localhost", Port: 6381, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-1":      {ServiceName: "redis-replica-1", CheckType: "tcp", Host: "localhost", Port: 6390, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-2":      {ServiceName: "redis-replica-2", CheckType: "tcp", Host: "localhost", Port: 6391, Timeout: 3 * time.Second, Retries: 5},
		"redis-replica-3":      {ServiceName: "redis-replica-3", CheckType: "tcp", Host: "localhost", Port: 6392, Timeout: 3 * time.Second, Retries: 5},
		"etcd-1":               {ServiceName: "etcd-1", CheckType: "http", Host: "localhost", Port: 2379, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"etcd-2":               {ServiceName: "etcd-2", CheckType: "http", Host: "localhost", Port: 2381, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"etcd-3":               {ServiceName: "etcd-3", CheckType: "http", Host: "localhost", Port: 2382, Path: "/health", Timeout: 3 * time.Second, Retries: 5},
		"nats":                 {ServiceName: "nats", CheckType: "http", Host: "localhost", Port: 8222, Path: "/healthz", Timeout: 3 * time.Second, Retries: 5},
		"kafka-1":              {ServiceName: "kafka-1", CheckType: "tcp", Host: "localhost", Port: 9092, Timeout: 5 * time.Second, Retries: 5},
		"kafka-2":              {ServiceName: "kafka-2", CheckType: "tcp", Host: "localhost", Port: 9093, Timeout: 5 * time.Second, Retries: 5},
		"kafka-3":              {ServiceName: "kafka-3", CheckType: "tcp", Host: "localhost", Port: 9094, Timeout: 5 * time.Second, Retries: 5},
		"rabbitmq":             {ServiceName: "rabbitmq", CheckType: "http", Host: "localhost", Port: 15672, Path: "/api/health/checks/virtual-hosts", Timeout: 5 * time.Second, Retries: 5},
		"prometheus":           {ServiceName: "prometheus", CheckType: "http", Host: "localhost", Port: 9090, Path: "/-/healthy", Timeout: 5 * time.Second, Retries: 5},
		"grafana":              {ServiceName: "grafana", CheckType: "http", Host: "localhost", Port: 3000, Path: "/api/health", Timeout: 5 * time.Second, Retries: 5},
		"jaeger":               {ServiceName: "jaeger", CheckType: "http", Host: "localhost", Port: 16686, Path: "/", Timeout: 5 * time.Second, Retries: 5},
		"vault":                {ServiceName: "vault", CheckType: "http", Host: "localhost", Port: 8200, Path: "/v1/sys/health", Timeout: 5 * time.Second, Retries: 5},
	}
	if c, ok := configs[serviceName]; ok {
		return c
	}
	return nil
}

// Check performs the health check.
func (h *HelixServiceHealthChecker) Check(ctx context.Context) (HealthStatus, error) {
	if h == nil {
		return HealthStatus{Healthy: false, Message: "nil checker"}, fmt.Errorf("nil checker")
	}

	var lastErr error
	for i := 0; i <= h.Retries; i++ {
		status, err := h.checkOnce(ctx)
		if err == nil && status.Healthy {
			return status, nil
		}
		lastErr = err
		if i < h.Retries {
			time.Sleep(time.Second)
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
		conn, err := net.DialTimeout("tcp", addr, h.Timeout)
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
		client := &http.Client{Timeout: h.Timeout}
		resp, err := client.Do(req)
		if err != nil {
			return HealthStatus{Healthy: false, Message: err.Error()}, err
		}
		defer resp.Body.Close()
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
