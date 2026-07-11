package envconfig

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const prefix = "CONTAINERS_REMOTE_"

// buildFromEnv constructs a DistributionConfig from the current
// environment. It performs NO validation: a SET-but-invalid value
// falls back to its default here (best-effort). Callers that need
// invalid values surfaced as errors use Parse or LoadFromFile, which
// run validateEnv on top of this.
func buildFromEnv() *DistributionConfig {
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

// LoadFromEnv loads the distribution configuration from environment
// variables. It is the UNVALIDATED convenience path: a SET-but-invalid
// value falls back to its default silently. Its signature intentionally
// returns no error to remain backward-compatible with existing direct
// callers (cmd/boot, cmd/distributed-build, cmd/ctop). Callers that need
// SET-but-invalid / out-of-range values surfaced as errors MUST use Parse
// or LoadFromFile.
func LoadFromEnv() *DistributionConfig {
	return buildFromEnv()
}

// Parse loads the distribution configuration from environment
// variables and VALIDATES it, returning a (config, error) pair so
// callers can use the standard Go idiom: cfg, err := Parse(). A
// SET-but-invalid or out-of-range value is surfaced as a non-nil
// error (never silently defaulted).
func Parse() (*DistributionConfig, error) {
	cfg := buildFromEnv()
	if err := validateEnv(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadFromFile loads configuration from a .env file, then overlays
// environment variables on top, and VALIDATES the result. This is
// the path operators use in practice, so SET-but-invalid values in
// the .env file are surfaced as errors rather than silently defaulted.
func LoadFromFile(path string) (*DistributionConfig, error) {
	if err := loadDotEnv(path); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	cfg := buildFromEnv()
	if err := validateEnv(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateEnv inspects the raw environment for SET-but-invalid values
// (tri-state: UNSET → fallback, fine; SET-but-invalid → error) and for
// out-of-range values, returning a single aggregated error naming every
// offending key. §11.4.10: it NEVER reads or echoes credential material
// (PASSWORD / KEY); only non-secret keys (booleans, integers, ports,
// host name/address) are inspected, and only those non-secret raw values
// appear in messages.
func validateEnv(cfg *DistributionConfig) error {
	var errs []string

	// ENV-1: boolean tri-state. A non-empty value that is not a
	// recognised boolean would otherwise silently invert operator
	// intent (e.g. ENABLED=yes→false, SSH_CONTROL_MASTER=off→true).
	for _, k := range []string{
		prefix + "ENABLED",
		prefix + "SSH_CONTROL_MASTER",
	} {
		if raw := os.Getenv(k); raw != "" {
			if _, ok := parseBoolExtended(raw); !ok {
				errs = append(errs, fmt.Sprintf(
					"%s: invalid boolean %q "+
						"(want true/false/yes/no/on/off/1/0)",
					k, raw,
				))
			}
		}
	}

	// ENV-3: integer tri-state (SET-but-malformed → error rather
	// than a silent fallback that masks the typo).
	for _, k := range []string{
		prefix + "PORT_RANGE_START",
		prefix + "PORT_RANGE_END",
		prefix + "CONNECT_TIMEOUT",
		prefix + "COMMAND_TIMEOUT",
		prefix + "SSH_CONTROL_PERSIST",
		prefix + "SSH_MAX_CONNECTIONS",
	} {
		if raw := os.Getenv(k); raw != "" {
			if _, err := strconv.Atoi(raw); err != nil {
				errs = append(errs, fmt.Sprintf(
					"%s: invalid integer %q", k, raw,
				))
			}
		}
	}

	// ENV-3: range checks on the effective values. A malformed value
	// has already fallen back to an in-range default above, so it is
	// reported once (as malformed), not twice.
	if cfg.ConnectTimeout <= 0 {
		errs = append(errs, fmt.Sprintf(
			"%sCONNECT_TIMEOUT: must be > 0, got %d",
			prefix, cfg.ConnectTimeout,
		))
	}
	if cfg.CommandTimeout <= 0 {
		errs = append(errs, fmt.Sprintf(
			"%sCOMMAND_TIMEOUT: must be > 0, got %d",
			prefix, cfg.CommandTimeout,
		))
	}
	if cfg.ControlPersist < 0 {
		errs = append(errs, fmt.Sprintf(
			"%sSSH_CONTROL_PERSIST: must be >= 0, got %d",
			prefix, cfg.ControlPersist,
		))
	}
	if cfg.MaxConnections < 1 {
		errs = append(errs, fmt.Sprintf(
			"%sSSH_MAX_CONNECTIONS: must be >= 1, got %d",
			prefix, cfg.MaxConnections,
		))
	}
	if cfg.PortRangeStart < 1 || cfg.PortRangeStart > 65535 {
		errs = append(errs, fmt.Sprintf(
			"%sPORT_RANGE_START: out of range 1-65535, got %d",
			prefix, cfg.PortRangeStart,
		))
	}
	if cfg.PortRangeEnd < 1 || cfg.PortRangeEnd > 65535 {
		errs = append(errs, fmt.Sprintf(
			"%sPORT_RANGE_END: out of range 1-65535, got %d",
			prefix, cfg.PortRangeEnd,
		))
	}
	if cfg.PortRangeStart > cfg.PortRangeEnd {
		errs = append(errs, fmt.Sprintf(
			"%sPORT_RANGE_START (%d) must be <= "+
				"%sPORT_RANGE_END (%d)",
			prefix, cfg.PortRangeStart,
			prefix, cfg.PortRangeEnd,
		))
	}

	// ENV-2: per-host required-field validation. Iterate with the
	// SAME stop-at-first-absent-NAME semantics as buildFromEnv. A host
	// with a NAME but no ADDRESS, or a malformed/out-of-range PORT,
	// would otherwise be loaded broken (a missing address SSHes
	// nowhere; a malformed port "22a" is coerced 0→22, masking the
	// typo). User / KeyPath / Password / Runtime legitimately inherit
	// from the defaults or the SSH config, so only ADDRESS is required.
	for n := 1; n <= 100; n++ {
		hp := fmt.Sprintf("%sHOST_%d_", prefix, n)
		name := os.Getenv(hp + "NAME")
		if name == "" {
			break
		}
		if strings.TrimSpace(os.Getenv(hp+"ADDRESS")) == "" {
			errs = append(errs, fmt.Sprintf(
				"%sADDRESS: required (host %d %q has no address)",
				hp, n, name,
			))
		}
		if raw := os.Getenv(hp + "PORT"); raw != "" {
			if p, err := strconv.Atoi(raw); err != nil {
				errs = append(errs, fmt.Sprintf(
					"%sPORT: invalid integer %q "+
						"(host %d %q)",
					hp, raw, n, name,
				))
			} else if p < 1 || p > 65535 {
				errs = append(errs, fmt.Sprintf(
					"%sPORT: out of range 1-65535, got %d "+
						"(host %d %q)",
					hp, p, n, name,
				))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf(
			"envconfig: %s", strings.Join(errs, "; "),
		)
	}
	return nil
}

// parseLabels parses a comma-separated "k=v,k2=v2" string into
// a map. Pairs with an empty key (e.g. "=true") are skipped rather
// than minting an empty-key label (ENV-5).
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
			key := strings.TrimSpace(parts[0])
			if key == "" {
				continue
			}
			labels[key] = strings.TrimSpace(parts[1])
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

// envBool returns the boolean for key, accepting the common extended
// vocabulary (true/false, yes/no, on/off, y/n, enabled/disabled, 1/0 —
// case-insensitive) in addition to strconv.ParseBool's set. An UNSET or
// unrecognised value returns fallback; SET-but-unrecognised values are
// surfaced as errors by validateEnv (via Parse / LoadFromFile), so this
// pure helper never itself reports the invalid input.
func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, ok := parseBoolExtended(v); ok {
			return b
		}
	}
	return fallback
}

// parseBoolExtended parses the common truthy/falsey vocabulary,
// case-insensitively. ok=false means the input was non-empty but not a
// recognised boolean (the SET-but-invalid case ENV-1 must surface,
// distinct from a genuinely UNSET value).
func parseBoolExtended(raw string) (val bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "yes", "y", "on", "enabled":
		return true, true
	case "0", "f", "false", "no", "n", "off", "disabled":
		return false, true
	}
	return false, false
}
