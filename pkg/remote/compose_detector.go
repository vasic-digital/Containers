package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"digital.vasic.containers/pkg/logging"
)

// composeCandidate is one entry in the priority-ordered list of compose
// commands the detector probes.
type composeCandidate struct {
	binary     string
	subcommand string
	name       string
}

// pipxPodmanComposePath resolves the CURRENT process's real home directory
// (via os.UserHomeDir(), which correctly handles Linux `/home/<user>`, macOS
// `/Users/<user>`, and Windows `%USERPROFILE%`) and returns the path a pipx
// install of podman-compose would land at: `~/.local/bin/podman-compose`.
//
// Returns "" when the home directory cannot be determined (e.g. $HOME unset)
// so the caller can skip this candidate entirely rather than probing a
// malformed path -- no guessing, per the project's no-hardcoding mandate.
func pipxPodmanComposePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "bin", "podman-compose")
}

// ComposeCommand represents a detected compose command on a remote host.
type ComposeCommand struct {
	// Name is the command name (e.g., "podman compose", "docker compose")
	Name string
	// Binary is the container runtime binary (e.g., "podman", "docker")
	Binary string
	// Subcommand is the compose subcommand (e.g., "compose")
	Subcommand string
	// Version is the detected version
	Version string
}

// String returns the full command string.
func (c ComposeCommand) String() string {
	if c.Subcommand != "" {
		return fmt.Sprintf("%s %s", c.Binary, c.Subcommand)
	}
	return c.Binary
}

// ComposeDetector detects the best available compose command on a remote host.
// Priority order: podman-compose > docker compose > podman compose > docker-compose
// Note: podman-compose (hyphen) is preferred over "podman compose" (space) because
// "podman compose" often delegates to docker-compose v1 which is incompatible with Podman.
type ComposeDetector struct {
	executor RemoteExecutor
	logger   logging.Logger
	cache    map[string]*ComposeCommand
	mu       sync.RWMutex
}

// NewComposeDetector creates a new compose command detector.
func NewComposeDetector(executor RemoteExecutor, logger logging.Logger) *ComposeDetector {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &ComposeDetector{
		executor: executor,
		logger:   logger,
		cache:    make(map[string]*ComposeCommand),
	}
}

// Detect detects the best available compose command on the remote host.
// Priority: podman-compose > docker compose > podman compose > docker-compose (v1 standalone)
// Note: podman-compose (hyphen) is preferred because "podman compose" often delegates
// to docker-compose v1 which is incompatible with Podman.
func (d *ComposeDetector) Detect(ctx context.Context, host RemoteHost) (*ComposeCommand, error) {
	// Check cache first
	d.mu.RLock()
	if cached, ok := d.cache[host.Name]; ok {
		d.mu.RUnlock()
		return cached, nil
	}
	d.mu.RUnlock()

	// Try each compose command in priority order
	// Priority: ~/.local/bin/podman-compose > podman-compose > docker compose > podman compose > docker-compose
	// ~/.local/bin/podman-compose is checked first for pipx installations,
	// resolved to the CURRENT user's real home directory (never a hardcoded
	// absolute path) so this candidate matches on every machine/user/OS.
	candidates := make([]composeCandidate, 0, 5)
	if pipxPath := pipxPodmanComposePath(); pipxPath != "" {
		candidates = append(candidates, composeCandidate{
			binary: pipxPath, // pipx installed podman-compose (highest priority)
			name:   "podman-compose",
		})
	}
	candidates = append(candidates,
		composeCandidate{"podman-compose", "", "podman-compose"}, // Native podman-compose (best for Podman)
		composeCandidate{"docker", "compose", "docker compose"},  // Docker v2 plugin
		composeCandidate{"podman", "compose", "podman compose"},  // Podman's wrapper (often delegates to docker-compose v1)
		composeCandidate{"docker-compose", "", "docker-compose"}, // Docker-compose v1 standalone
	)

	for _, candidate := range candidates {
		cmd := &ComposeCommand{
			Name:       candidate.name,
			Binary:     candidate.binary,
			Subcommand: candidate.subcommand,
		}

		if d.probeCommand(ctx, host, cmd) {
			d.logger.Info("detected compose command on %s: %s (version: %s)",
				host.Name, cmd.Name, cmd.Version)

			// Cache the result
			d.mu.Lock()
			d.cache[host.Name] = cmd
			d.mu.Unlock()

			return cmd, nil
		}
	}

	return nil, fmt.Errorf("no compose command found on host %s", host.Name)
}

// probeCommand tests if a compose command is available on the remote host.
func (d *ComposeDetector) probeCommand(ctx context.Context, host RemoteHost, cmd *ComposeCommand) bool {
	// Build version command
	var versionCmd string
	if cmd.Subcommand != "" {
		versionCmd = fmt.Sprintf("%s %s version --short", cmd.Binary, cmd.Subcommand)
	} else {
		versionCmd = fmt.Sprintf("%s version --short", cmd.Binary)
	}

	result, err := d.executor.Execute(ctx, host, versionCmd)
	if err != nil || result.ExitCode != 0 {
		d.logger.Debug("compose command %s not available on %s: %v",
			cmd.Name, host.Name, err)
		return false
	}

	cmd.Version = strings.TrimSpace(result.Stdout)
	return true
}

// DetectWithFallback detects the best compose command, falling back to the host's
// configured runtime if auto-detection fails.
func (d *ComposeDetector) DetectWithFallback(ctx context.Context, host RemoteHost) *ComposeCommand {
	// Try auto-detection first
	cmd, err := d.Detect(ctx, host)
	if err == nil {
		return cmd
	}

	// Fall back to host's configured runtime
	d.logger.Warn("compose auto-detection failed on %s, using configured runtime %s",
		host.Name, host.Runtime)

	binary := host.Runtime
	if binary == "" {
		binary = "docker" // Default fallback
	}

	return &ComposeCommand{
		Name:       fmt.Sprintf("%s compose", binary),
		Binary:     binary,
		Subcommand: "compose",
	}
}

// ClearCache clears the cached compose command for a host (or all hosts if name is empty).
func (d *ComposeDetector) ClearCache(hostName string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if hostName == "" {
		d.cache = make(map[string]*ComposeCommand)
	} else {
		delete(d.cache, hostName)
	}
}

// KnownComposeCommands returns the list of compose commands in priority order.
func KnownComposeCommands() []string {
	return []string{
		"podman-compose",
		"docker compose",
		"podman compose",
		"docker-compose",
	}
}

// IsComposeCommand checks if a string is a known compose command.
func IsComposeCommand(s string) bool {
	for _, cmd := range KnownComposeCommands() {
		if s == cmd {
			return true
		}
	}
	return false
}
