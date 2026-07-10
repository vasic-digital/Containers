package network

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// TunnelManager creates and manages SSH tunnels between local and
// remote hosts.
type TunnelManager interface {
	// CreateTunnel establishes an SSH tunnel to the named host.
	CreateTunnel(
		ctx context.Context,
		hostName string,
		spec TunnelSpec,
	) (*TunnelInfo, error)

	// CloseTunnel closes the tunnel on the given local port.
	CloseTunnel(localPort string) error

	// ListTunnels returns all active tunnels.
	ListTunnels() []TunnelInfo

	// CloseAllForHost closes all tunnels to a specific host.
	CloseAllForHost(hostName string) error

	// CloseAll closes every active tunnel.
	CloseAll() error
}

// DefaultTunnelManager implements TunnelManager using SSH CLI.
type DefaultTunnelManager struct {
	mu          sync.Mutex
	tunnels     map[string]*tunnelEntry // keyed by local port
	hostManager remote.HostManager
	allocator   *PortAllocator
	opts        Options
	logger      logging.Logger
}

type tunnelEntry struct {
	info TunnelInfo
	cmd  *exec.Cmd
}

// NewTunnelManager creates a DefaultTunnelManager.
func NewTunnelManager(
	hostManager remote.HostManager,
	logger logging.Logger,
	opts ...Option,
) *DefaultTunnelManager {
	o := ApplyOptions(opts)
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &DefaultTunnelManager{
		tunnels:     make(map[string]*tunnelEntry),
		hostManager: hostManager,
		allocator:   NewPortAllocator(o.PortRangeStart, o.PortRangeEnd),
		opts:        o,
		logger:      logger,
	}
}

// CreateTunnel establishes an SSH tunnel.
func (m *DefaultTunnelManager) CreateTunnel(
	ctx context.Context,
	hostName string,
	spec TunnelSpec,
) (*TunnelInfo, error) {
	host, err := m.hostManager.GetHost(hostName)
	if err != nil {
		return nil, fmt.Errorf(
			"get host %s: %w", hostName, err,
		)
	}
	if host == nil {
		return nil, fmt.Errorf("host %s not found", hostName)
	}

	// Auto-allocate local port if not specified.
	autoAllocatedPort := -1
	if spec.LocalPort == "" {
		port, err := m.allocator.Allocate(spec.Description)
		if err != nil {
			return nil, fmt.Errorf(
				"allocate port: %w", err,
			)
		}
		spec.LocalPort = strconv.Itoa(port)
		autoAllocatedPort = port
	}

	args := m.tunnelArgs(*host, spec)
	cmd := exec.CommandContext(ctx, "ssh", args...)

	if err := cmd.Start(); err != nil {
		// The SSH process never launched, so no tunnelEntry is stored and
		// CloseTunnel will never run to release the port. Release the port we
		// auto-allocated above, otherwise every failed launch permanently
		// leaks one port from the range until the allocator is exhausted.
		if autoAllocatedPort >= 0 {
			m.allocator.Release(autoAllocatedPort)
		}
		return nil, fmt.Errorf(
			"start tunnel to %s: %w", hostName, err,
		)
	}

	info := TunnelInfo{
		Spec:      spec,
		HostName:  hostName,
		State:     TunnelActive,
		CreatedAt: time.Now(),
		PID:       cmd.Process.Pid,
	}

	m.mu.Lock()
	m.tunnels[spec.LocalPort] = &tunnelEntry{
		info: info,
		cmd:  cmd,
	}
	m.mu.Unlock()

	m.logger.Info(
		"tunnel created: %s %s:%s <-> local:%s via %s",
		spec.Direction, spec.RemoteHost, spec.RemotePort,
		spec.LocalPort, hostName,
	)

	return &info, nil
}

// CloseTunnel closes the tunnel on the given local port.
func (m *DefaultTunnelManager) CloseTunnel(
	localPort string,
) error {
	m.mu.Lock()
	entry, ok := m.tunnels[localPort]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf(
			"no tunnel on port %s", localPort,
		)
	}
	delete(m.tunnels, localPort)
	m.mu.Unlock()

	port, _ := strconv.Atoi(localPort)
	m.allocator.Release(port)

	if entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
		_ = entry.cmd.Wait()
	}

	m.logger.Info("tunnel closed: local port %s", localPort)
	return nil
}

// ListTunnels returns all active tunnels.
func (m *DefaultTunnelManager) ListTunnels() []TunnelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	infos := make([]TunnelInfo, 0, len(m.tunnels))
	for _, entry := range m.tunnels {
		infos = append(infos, entry.info)
	}
	return infos
}

// CloseAllForHost closes all tunnels to a specific host.
func (m *DefaultTunnelManager) CloseAllForHost(
	hostName string,
) error {
	m.mu.Lock()
	var ports []string
	for port, entry := range m.tunnels {
		if entry.info.HostName == hostName {
			ports = append(ports, port)
		}
	}
	m.mu.Unlock()

	var firstErr error
	for _, port := range ports {
		if err := m.CloseTunnel(port); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseAll closes every active tunnel.
func (m *DefaultTunnelManager) CloseAll() error {
	m.mu.Lock()
	ports := make([]string, 0, len(m.tunnels))
	for port := range m.tunnels {
		ports = append(ports, port)
	}
	m.mu.Unlock()

	var firstErr error
	for _, port := range ports {
		if err := m.CloseTunnel(port); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *DefaultTunnelManager) tunnelArgs(
	host remote.RemoteHost, spec TunnelSpec,
) []string {
	var fwdArg string
	remoteTarget := spec.RemoteHost
	if remoteTarget == "" {
		remoteTarget = "localhost"
	}

	switch spec.Direction {
	case TunnelRemote:
		fwdArg = fmt.Sprintf("-R %s:%s:%s",
			spec.RemotePort, remoteTarget, spec.LocalPort,
		)
	default: // TunnelLocal
		fwdArg = fmt.Sprintf("-L %s:%s:%s",
			spec.LocalPort, remoteTarget, spec.RemotePort,
		)
	}

	args := []string{
		"-N",
		fwdArg,
		"-o", "StrictHostKeyChecking=no",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		"-p", strconv.Itoa(host.SSHPort()),
	}

	if host.KeyPath != "" {
		args = append(args, "-i", host.KeyPath)
	}

	args = append(args,
		fmt.Sprintf("%s@%s", host.User, host.Address),
	)
	return args
}
