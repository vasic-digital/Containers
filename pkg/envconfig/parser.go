package envconfig

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const prefix = "CONTAINERS_REMOTE_"

// LoadFromEnv loads the distribution configuration from
// environment variables.
func LoadFromEnv() *DistributionConfig {
	cfg := &DistributionConfig{
		Enabled:         envBool(prefix+"ENABLED", false),
		DefaultUser:     envString(prefix+"DEFAULT_SSH_USER", ""),
		DefaultKeyPath:  envString(prefix+"DEFAULT_SSH_KEY", ""),
		DefaultPassword: envString(prefix+"DEFAULT_SSH_PASSWORD", ""),
		DefaultRuntime:  envString(prefix+"DEFAULT_RUNTIME", "docker"),
		Scheduler:       envString(prefix+"SCHEDULER", "resource_aware"),
		PortRangeStart:  envInt(prefix+"PORT_RANGE_START", 20000),
		PortRangeEnd:    envInt(prefix+"PORT_RANGE_END", 30000),
		VolumeType:      envString(prefix+"VOLUME_TYPE", "sshfs"),
		ConnectTimeout:  envInt(prefix+"CONNECT_TIMEOUT", 10),
		// 30 minutes — large enough for image-build `compose up`
		// operations (multi-GB pulls + multi-minute layer builds)
		// without relying on operators to tune this manually.
		// SSH keep-alive (30s * 10 = 5 min silence tolerance) is
		// the REAL detector of dead connections; this cap catches
		// genuinely hung remote commands. Pre-fix default of 120s
		// routinely killed compose builds on cold hosts.
		CommandTimeout:       envInt(prefix+"COMMAND_TIMEOUT", 1800),
		ControlMasterEnabled: envBool(prefix+"SSH_CONTROL_MASTER", true),
		ControlPersist:       envInt(prefix+"SSH_CONTROL_PERSIST", 300),
		MaxConnections:       envInt(prefix+"SSH_MAX_CONNECTIONS", 10),
	}

	// Load numbered hosts: CONTAINERS_REMOTE_HOST_N_*
	for n := 1; n <= 100; n++ {
		hostPrefix := fmt.Sprintf(
			"%sHOST_%d_", prefix, n,
		)
		name := envString(hostPrefix+"NAME", "")
		if name == "" {
			break
		}
		host := RemoteEndpointConfig{
			Name:     name,
			Address:  envString(hostPrefix+"ADDRESS", ""),
			Port:     envInt(hostPrefix+"PORT", 0),
			User:     envString(hostPrefix+"USER", ""),
			KeyPath:  envString(hostPrefix+"KEY", ""),
			Password: envString(hostPrefix+"PASSWORD", ""),
			Runtime:  envString(hostPrefix+"RUNTIME", ""),
			Labels:   parseLabels(envString(hostPrefix+"LABELS", "")),
		}
		if v := os.Getenv(fmt.Sprintf(
			"%sHOST_%d_GPU_AUTOPROBE", prefix, n,
		)); v != "" {
			if host.Labels == nil {
				host.Labels = map[string]string{}
			}
			host.Labels["gpu_autoprobe"] = v
		}
		cfg.Hosts = append(cfg.Hosts, host)
	}

	return cfg
}

// Parse loads the distribution configuration from environment
// variables. It is a convenience wrapper around LoadFromEnv that
// returns a (config, error) pair so callers can use the standard
// Go idiom: cfg, err := Parse().
func Parse() (*DistributionConfig, error) {
	return LoadFromEnv(), nil
}

// LoadFromFile loads configuration from a .env file, then
// overlays environment variables on top.
func LoadFromFile(path string) (*DistributionConfig, error) {
	if err := loadDotEnv(path); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return LoadFromEnv(), nil
}

// parseLabels parses a comma-separated "k=v,k2=v2" string into
// a map.
func parseLabels(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			labels[strings.TrimSpace(parts[0])] =
				strings.TrimSpace(parts[1])
		}
	}
	return labels
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
