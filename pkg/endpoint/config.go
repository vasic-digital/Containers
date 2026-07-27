package endpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EndpointConfig holds a named collection of service endpoints
// loaded from a configuration file.
type EndpointConfig struct {
	// Endpoints maps a logical service name to its endpoint
	// configuration.
	Endpoints map[string]EndpointEntry `yaml:"endpoints" json:"endpoints"`
}

// EndpointEntry is the serialisable form of a ServiceEndpoint.
type EndpointEntry struct {
	Host             string `yaml:"host" json:"host"`
	Port             string `yaml:"port" json:"port"`
	URL              string `yaml:"url,omitempty" json:"url,omitempty"`
	Enabled          *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Required         bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Remote           bool   `yaml:"remote,omitempty" json:"remote,omitempty"`
	HealthPath       string `yaml:"health_path,omitempty" json:"health_path,omitempty"`
	HealthType       string `yaml:"health_type,omitempty" json:"health_type,omitempty"`
	TimeoutSeconds   int    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	RetryCount       int    `yaml:"retry_count,omitempty" json:"retry_count,omitempty"`
	ComposeFile      string `yaml:"compose_file,omitempty" json:"compose_file,omitempty"`
	ServiceName      string `yaml:"service_name,omitempty" json:"service_name,omitempty"`
	Profile          string `yaml:"profile,omitempty" json:"profile,omitempty"`
	DiscoveryEnabled bool   `yaml:"discovery_enabled,omitempty" json:"discovery_enabled,omitempty"`
	DiscoveryMethod  string `yaml:"discovery_method,omitempty" json:"discovery_method,omitempty"`
	DiscoveryTimeout int    `yaml:"discovery_timeout,omitempty" json:"discovery_timeout,omitempty"`
}

// LoadConfig reads an EndpointConfig from the file at the given
// path. The format is determined by the file extension: .yaml,
// .yml for YAML and .json for JSON.
func LoadConfig(path string) (*EndpointConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	cfg := &EndpointConfig{}
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse yaml %s: %w", path, err)
		}
	case ".json":
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse json %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf(
			"unsupported config format %q", ext,
		)
	}
	return cfg, nil
}

// ToServiceEndpoints converts the loaded configuration entries
// into a map of ServiceEndpoint values ready for use.
func (c *EndpointConfig) ToServiceEndpoints() map[string]ServiceEndpoint {
	out := make(map[string]ServiceEndpoint, len(c.Endpoints))
	for name, entry := range c.Endpoints {
		out[name] = entry.toServiceEndpoint()
	}
	return out
}

// toServiceEndpoint converts an EndpointEntry into a
// ServiceEndpoint, applying default values where appropriate.
func (e *EndpointEntry) toServiceEndpoint() ServiceEndpoint {
	enabled := true
	if e.Enabled != nil {
		enabled = *e.Enabled
	}
	timeout := defaultTimeout
	if e.TimeoutSeconds > 0 {
		timeout = time.Duration(e.TimeoutSeconds) * time.Second
	}
	retryCount := defaultRetryCount
	if e.RetryCount > 0 {
		retryCount = e.RetryCount
	}
	healthType := defaultHealthType
	if e.HealthType != "" {
		healthType = e.HealthType
	}
	var discoveryTimeout time.Duration
	if e.DiscoveryTimeout > 0 {
		discoveryTimeout = time.Duration(
			e.DiscoveryTimeout,
		) * time.Second
	}
	return ServiceEndpoint{
		Host:             e.Host,
		Port:             e.Port,
		URL:              e.URL,
		Enabled:          enabled,
		Required:         e.Required,
		Remote:           e.Remote,
		HealthPath:       e.HealthPath,
		HealthType:       healthType,
		Timeout:          timeout,
		RetryCount:       retryCount,
		ComposeFile:      e.ComposeFile,
		ServiceName:      e.ServiceName,
		Profile:          e.Profile,
		DiscoveryEnabled: e.DiscoveryEnabled,
		DiscoveryMethod:  e.DiscoveryMethod,
		DiscoveryTimeout: discoveryTimeout,
	}
}
