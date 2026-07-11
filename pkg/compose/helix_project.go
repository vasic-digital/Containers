package compose

import (
	"fmt"
	"os"
	"time"
)

// HelixService defines a single infrastructure service in the Helix stack.
type HelixService struct {
	Name           string
	Image          string
	Ports          []PortMapping
	Env            map[string]string
	Volumes        []string
	DependsOn      []string
	HealthCheck    *HelixHealthCheck
	Labels         map[string]string
	ResourceLimits *HelixResourceLimits
}

// HelixHealthCheck configures a health probe for a service.
type HelixHealthCheck struct {
	Test     []string
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
}

// HelixResourceLimits sets container resource constraints.
type HelixResourceLimits struct {
	CPUs   string
	Memory string
	Pids   int64
}

// PortMapping maps a host port to a container port.
type PortMapping struct {
	HostPort      int
	ContainerPort int
	Protocol      string
}

// HelixComposeProject embeds ComposeProject with Helix-specific services.
type HelixComposeProject struct {
	ProjectName string
	Services    []HelixService
	Networks    []string
	Volumes     []string
}

// NewHelixComposeProject creates a new project with the given services.
func NewHelixComposeProject(projectName string, services []HelixService) *HelixComposeProject {
	return &HelixComposeProject{
		ProjectName: projectName,
		Services:    services,
		Networks:    []string{"helix"},
	}
}

// GetService returns a service by name.
func (p *HelixComposeProject) GetService(name string) (*HelixService, error) {
	if p == nil {
		// A nil receiver (map-lookup miss, ignored-error return, uninitialized
		// field — all legitimate Go zero values) returns the same "not found"
		// error an empty project gives, rather than panicking
		// (CT-HARDEN-COMPOSE-1).
		return nil, fmt.Errorf("service %q not found", name)
	}
	for i := range p.Services {
		if p.Services[i].Name == name {
			return &p.Services[i], nil
		}
	}
	return nil, fmt.Errorf("service %q not found", name)
}

// ServiceNames returns all service names.
func (p *HelixComposeProject) ServiceNames() []string {
	if p == nil {
		// Return the SAME non-nil empty slice an initialized-but-empty
		// project yields (make([]string, 0)), not a bare nil — so a nil
		// receiver is truly indistinguishable from an empty project
		// (reflect.DeepEqual / == nil / JSON null-vs-[] all agree), the
		// contract CT-HARDEN-COMPOSE-1 guarantees.
		return []string{}
	}
	names := make([]string, len(p.Services))
	for i, s := range p.Services {
		names[i] = s.Name
	}
	return names
}

// HasService checks if a service exists.
func (p *HelixComposeProject) HasService(name string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}

// envOrDefault returns the value of the named environment variable, or
// fallback when unset/empty. DefaultHelixServices() uses this so every
// credential embedded in the generated dev-stack compose definition can be
// overridden by the environment (§6.R no-hardcoding) instead of being a
// tracked-source literal; the literal values that remain are DOCUMENTED
// dev-only fallback defaults for local bring-up, never a value this package
// requires a real deployment to use.
func envOrDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// DefaultHelixServices returns the 20 standard Helix infrastructure services.
//
// CO-3 (§6.R no-hardcoding): the credentials below (Postgres user/password/db,
// Grafana admin user/password, the Vault dev-mode root token) are sourced
// from environment variables, falling back to the historical values ONLY
// when the corresponding env var is unset -- those fallbacks are DEV-ONLY
// defaults for local bring-up of the generated compose stack, not production
// secrets, and MUST be overridden via the environment for any non-throwaway
// deployment:
//
//	HELIX_COMPOSE_POSTGRES_USER          (default "helix")
//	HELIX_COMPOSE_POSTGRES_PASSWORD      (default "helix")
//	HELIX_COMPOSE_POSTGRES_DB            (default "helix")
//	HELIX_COMPOSE_GRAFANA_ADMIN_USER     (default "admin")
//	HELIX_COMPOSE_GRAFANA_ADMIN_PASSWORD (default "admin")
//	HELIX_COMPOSE_VAULT_DEV_ROOT_TOKEN   (default "helix-root-token")
//
// CO-3 port-literal assessment: the HostPort/ContainerPort values below are
// NOT sourced from the environment. Unlike the credentials above they are
// mutually load-bearing across this single fixed 20-service topology (the
// etcd cluster's ETCD_INITIAL_CLUSTER peer URLs are hardcoded to
// etcd-{1,2,3}:2380 and the port list itself defines which host ports the 20
// services occupy relative to one another) -- making them independently
// env-overridable here without a coordinated per-deployment port-plan
// mechanism would let a caller silently create colliding or
// internally-inconsistent bindings, a worse outcome than a documented
// literal. They are honest, non-secret, DEV-ONLY default port assignments
// for local bring-up of this generated stack; a deployment needing different
// ports should construct its own []HelixService rather than rely on this
// slice verbatim.
func DefaultHelixServices() []HelixService {
	return []HelixService{
		{
			Name:  "postgres-primary",
			Image: "postgres:16-alpine",
			Ports: []PortMapping{{HostPort: 5432, ContainerPort: 5432, Protocol: "tcp"}},
			Env: map[string]string{
				"POSTGRES_USER":     envOrDefault("HELIX_COMPOSE_POSTGRES_USER", "helix"),
				"POSTGRES_PASSWORD": envOrDefault("HELIX_COMPOSE_POSTGRES_PASSWORD", "helix"),
				"POSTGRES_DB":       envOrDefault("HELIX_COMPOSE_POSTGRES_DB", "helix"),
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "pg_isready -U helix -d helix"},
				Interval: 5 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "postgres-replica",
			Image: "postgres:16-alpine",
			Ports: []PortMapping{{HostPort: 5433, ContainerPort: 5432, Protocol: "tcp"}},
			Env: map[string]string{
				"POSTGRES_USER":     envOrDefault("HELIX_COMPOSE_POSTGRES_USER", "helix"),
				"POSTGRES_PASSWORD": envOrDefault("HELIX_COMPOSE_POSTGRES_PASSWORD", "helix"),
				"POSTGRES_DB":       envOrDefault("HELIX_COMPOSE_POSTGRES_DB", "helix"),
			},
			DependsOn: []string{"postgres-primary"},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "pg_isready -U helix -d helix"},
				Interval: 5 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "redis-master-1",
			Image: "redis:7-alpine",
			Ports: []PortMapping{{HostPort: 6379, ContainerPort: 6379, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "redis-master-2",
			Image: "redis:7-alpine",
			Ports: []PortMapping{{HostPort: 6380, ContainerPort: 6379, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "redis-master-3",
			Image: "redis:7-alpine",
			Ports: []PortMapping{{HostPort: 6381, ContainerPort: 6379, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:      "redis-replica-1",
			Image:     "redis:7-alpine",
			Ports:     []PortMapping{{HostPort: 6390, ContainerPort: 6379, Protocol: "tcp"}},
			DependsOn: []string{"redis-master-1"},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:      "redis-replica-2",
			Image:     "redis:7-alpine",
			Ports:     []PortMapping{{HostPort: 6391, ContainerPort: 6379, Protocol: "tcp"}},
			DependsOn: []string{"redis-master-2"},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:      "redis-replica-3",
			Image:     "redis:7-alpine",
			Ports:     []PortMapping{{HostPort: 6392, ContainerPort: 6379, Protocol: "tcp"}},
			DependsOn: []string{"redis-master-3"},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD", "redis-cli", "ping"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "etcd-1",
			Image: "quay.io/coreos/etcd:v3.5.15",
			Ports: []PortMapping{{HostPort: 2379, ContainerPort: 2379, Protocol: "tcp"}, {HostPort: 2380, ContainerPort: 2380, Protocol: "tcp"}},
			Env: map[string]string{
				"ETCD_NAME":                        "etcd-1",
				"ETCD_INITIAL_ADVERTISE_PEER_URLS": "http://etcd-1:2380",
				"ETCD_LISTEN_PEER_URLS":            "http://0.0.0.0:2380",
				"ETCD_LISTEN_CLIENT_URLS":          "http://0.0.0.0:2379",
				"ETCD_ADVERTISE_CLIENT_URLS":       "http://etcd-1:2379",
				"ETCD_INITIAL_CLUSTER":             "etcd-1=http://etcd-1:2380,etcd-2=http://etcd-2:2380,etcd-3=http://etcd-3:2380",
				"ETCD_INITIAL_CLUSTER_TOKEN":       "helix-etcd-cluster",
				"ETCD_INITIAL_CLUSTER_STATE":       "new",
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "etcdctl endpoint health"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "etcd-2",
			Image: "quay.io/coreos/etcd:v3.5.15",
			Ports: []PortMapping{{HostPort: 2381, ContainerPort: 2380, Protocol: "tcp"}},
			Env: map[string]string{
				"ETCD_NAME":                        "etcd-2",
				"ETCD_INITIAL_ADVERTISE_PEER_URLS": "http://etcd-2:2380",
				"ETCD_LISTEN_PEER_URLS":            "http://0.0.0.0:2380",
				"ETCD_LISTEN_CLIENT_URLS":          "http://0.0.0.0:2379",
				"ETCD_ADVERTISE_CLIENT_URLS":       "http://etcd-2:2379",
				"ETCD_INITIAL_CLUSTER":             "etcd-1=http://etcd-1:2380,etcd-2=http://etcd-2:2380,etcd-3=http://etcd-3:2380",
				"ETCD_INITIAL_CLUSTER_TOKEN":       "helix-etcd-cluster",
				"ETCD_INITIAL_CLUSTER_STATE":       "new",
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "etcdctl endpoint health"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "etcd-3",
			Image: "quay.io/coreos/etcd:v3.5.15",
			Ports: []PortMapping{{HostPort: 2382, ContainerPort: 2380, Protocol: "tcp"}},
			Env: map[string]string{
				"ETCD_NAME":                        "etcd-3",
				"ETCD_INITIAL_ADVERTISE_PEER_URLS": "http://etcd-3:2380",
				"ETCD_LISTEN_PEER_URLS":            "http://0.0.0.0:2380",
				"ETCD_LISTEN_CLIENT_URLS":          "http://0.0.0.0:2379",
				"ETCD_ADVERTISE_CLIENT_URLS":       "http://etcd-3:2379",
				"ETCD_INITIAL_CLUSTER":             "etcd-1=http://etcd-1:2380,etcd-2=http://etcd-2:2380,etcd-3=http://etcd-3:2380",
				"ETCD_INITIAL_CLUSTER_TOKEN":       "helix-etcd-cluster",
				"ETCD_INITIAL_CLUSTER_STATE":       "new",
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "etcdctl endpoint health"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "nats",
			Image: "nats:2.10-alpine",
			Ports: []PortMapping{{HostPort: 4222, ContainerPort: 4222, Protocol: "tcp"}, {HostPort: 8222, ContainerPort: 8222, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "wget -qO- http://localhost:8222/healthz"},
				Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "kafka-1",
			Image: "apache/kafka:4.0.0",
			Ports: []PortMapping{{HostPort: 9092, ContainerPort: 9092, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "kafka-broker-api-versions.sh --bootstrap-server localhost:9092"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "kafka-2",
			Image: "apache/kafka:4.0.0",
			Ports: []PortMapping{{HostPort: 9093, ContainerPort: 9092, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "kafka-broker-api-versions.sh --bootstrap-server localhost:9092"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "kafka-3",
			Image: "apache/kafka:4.0.0",
			Ports: []PortMapping{{HostPort: 9094, ContainerPort: 9092, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "kafka-broker-api-versions.sh --bootstrap-server localhost:9092"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "rabbitmq",
			Image: "rabbitmq:3.13-management-alpine",
			Ports: []PortMapping{{HostPort: 5672, ContainerPort: 5672, Protocol: "tcp"}, {HostPort: 15672, ContainerPort: 15672, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "rabbitmq-diagnostics ping"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "prometheus",
			Image: "prom/prometheus:v2.50.0",
			Ports: []PortMapping{{HostPort: 9090, ContainerPort: 9090, Protocol: "tcp"}},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "wget -qO- http://localhost:9090/-/healthy"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "grafana",
			Image: "grafana/grafana:10.4.0",
			Ports: []PortMapping{{HostPort: 3000, ContainerPort: 3000, Protocol: "tcp"}},
			Env: map[string]string{
				"GF_SECURITY_ADMIN_USER":     envOrDefault("HELIX_COMPOSE_GRAFANA_ADMIN_USER", "admin"),
				"GF_SECURITY_ADMIN_PASSWORD": envOrDefault("HELIX_COMPOSE_GRAFANA_ADMIN_PASSWORD", "admin"),
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "wget -qO- http://localhost:3000/api/health"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "jaeger",
			Image: "jaegertracing/all-in-one:1.55",
			Ports: []PortMapping{{HostPort: 16686, ContainerPort: 16686, Protocol: "tcp"}, {HostPort: 14268, ContainerPort: 14268, Protocol: "tcp"}},
			Env:   map[string]string{"COLLECTOR_OTLP_ENABLED": "true"},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "wget -qO- http://localhost:16686"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
		{
			Name:  "vault",
			Image: "hashicorp/vault:1.16",
			Ports: []PortMapping{{HostPort: 8200, ContainerPort: 8200, Protocol: "tcp"}},
			Env: map[string]string{
				"VAULT_DEV_ROOT_TOKEN_ID": envOrDefault("HELIX_COMPOSE_VAULT_DEV_ROOT_TOKEN", "helix-root-token"),
				// Bind address, not a secret -- no env override needed.
				"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
			},
			HealthCheck: &HelixHealthCheck{
				Test:     []string{"CMD-SHELL", "vault status"},
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			},
		},
	}
}
