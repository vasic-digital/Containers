package network

import (
	"context"
	"fmt"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// mockHostManager implements remote.HostManager for tunnel tests.
type mockHostManager struct {
	hosts map[string]remote.RemoteHost
}

func (m *mockHostManager) AddHost(h remote.RemoteHost) error {
	m.hosts[h.Name] = h
	return nil
}

func (m *mockHostManager) RemoveHost(name string) error {
	delete(m.hosts, name)
	return nil
}

func (m *mockHostManager) GetHost(name string) (*remote.RemoteHost, error) {
	h, ok := m.hosts[name]
	if !ok {
		return nil, fmt.Errorf("host %s not found", name)
	}
	return &h, nil
}

func (m *mockHostManager) ListHosts() []remote.RemoteHost {
	hosts := make([]remote.RemoteHost, 0)
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	return hosts
}

func (m *mockHostManager) ProbeHost(
	ctx context.Context, name string,
) (*remote.HostResources, error) {
	return &remote.HostResources{Host: name}, nil
}

func (m *mockHostManager) ProbeAll(
	ctx context.Context,
) map[string]*remote.HostResources {
	return nil
}

func (m *mockHostManager) HostState(name string) remote.HostState {
	return remote.HostOnline
}

// newDummyCmd returns a non-started exec.Cmd. Its Process is nil,
// which is safe to use because CloseTunnel only calls Process.Kill()
// when Process != nil.
func newDummyCmd() *exec.Cmd {
	return exec.Command("true")
}

// TestNewTunnelManager verifies the constructor initialises fields.
// TestNewTunnelManager_NilLogger verifies nil logger uses NopLogger.
// TestNewTunnelManager_WithOptions verifies option application.
func TestNewTunnelManager_WithOptions(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{},
		WithPortRange(5000, 6000),
	)
	assert.Equal(t, 5000, mgr.opts.PortRangeStart)
	assert.Equal(t, 6000, mgr.opts.PortRangeEnd)
}

// TestDefaultTunnelManager_CreateTunnel_HostNotFound verifies error
// when the requested host does not exist in the manager.
func TestDefaultTunnelManager_CreateTunnel_HostNotFound(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	_, err := mgr.CreateTunnel(context.Background(), "nonexistent", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemotePort: "5432",
	})
	assert.Error(t, err)
}

// TestDefaultTunnelManager_ListTunnels_WithEntries verifies listing
// returns currently tracked tunnels.
func TestDefaultTunnelManager_ListTunnels_WithEntries(t *testing.T) {
	mgr := &DefaultTunnelManager{
		tunnels: make(map[string]*tunnelEntry),
	}
	mgr.tunnels["9001"] = &tunnelEntry{
		info: TunnelInfo{
			Spec:     TunnelSpec{LocalPort: "9001"},
			HostName: "gpu-1",
			State:    TunnelActive,
		},
		cmd: newDummyCmd(),
	}
	mgr.tunnels["9002"] = &tunnelEntry{
		info: TunnelInfo{
			Spec:     TunnelSpec{LocalPort: "9002"},
			HostName: "gpu-1",
			State:    TunnelActive,
		},
		cmd: newDummyCmd(),
	}

	infos := mgr.ListTunnels()
	assert.Len(t, infos, 2)
}

// TestDefaultTunnelManager_CloseAllForHost_MatchingAndNonMatching
// verifies only tunnels for the specified host are closed.
func TestDefaultTunnelManager_CloseAllForHost_MatchingAndNonMatching(t *testing.T) {
	mgr := &DefaultTunnelManager{
		tunnels:   make(map[string]*tunnelEntry),
		allocator: NewPortAllocator(20000, 30000),
		logger:    logging.NopLogger{},
	}

	// Use unstarted cmds: Process is nil so Kill() is skipped.
	mgr.tunnels["9001"] = &tunnelEntry{
		info: TunnelInfo{HostName: "host-a", Spec: TunnelSpec{LocalPort: "9001"}},
		cmd:  newDummyCmd(),
	}
	mgr.tunnels["9002"] = &tunnelEntry{
		info: TunnelInfo{HostName: "host-b", Spec: TunnelSpec{LocalPort: "9002"}},
		cmd:  newDummyCmd(),
	}

	err := mgr.CloseAllForHost("host-a")
	assert.NoError(t, err)
	assert.Len(t, mgr.tunnels, 1)
	_, remaining := mgr.tunnels["9002"]
	assert.True(t, remaining)
}

// TestDefaultTunnelManager_CloseTunnel_NilProcess verifies closing a
// tunnel whose cmd has no process does not panic.
func TestDefaultTunnelManager_CloseTunnel_NilProcess(t *testing.T) {
	mgr := &DefaultTunnelManager{
		tunnels:   make(map[string]*tunnelEntry),
		allocator: NewPortAllocator(20000, 30000),
		logger:    logging.NopLogger{},
	}

	// newDummyCmd() creates a cmd with nil Process (not started yet).
	mgr.tunnels["9001"] = &tunnelEntry{
		info: TunnelInfo{HostName: "h", Spec: TunnelSpec{LocalPort: "9001"}},
		cmd:  newDummyCmd(),
	}

	err := mgr.CloseTunnel("9001")
	require.NoError(t, err)
	assert.Empty(t, mgr.tunnels)
}

// TestDefaultTunnelManager_CloseAll_WithEntries verifies closing all
// tunnels removes them from the map.
func TestDefaultTunnelManager_CloseAll_WithEntries(t *testing.T) {
	mgr := &DefaultTunnelManager{
		tunnels:   make(map[string]*tunnelEntry),
		allocator: NewPortAllocator(20000, 30000),
		logger:    logging.NopLogger{},
	}

	mgr.tunnels["9001"] = &tunnelEntry{
		info: TunnelInfo{HostName: "h", Spec: TunnelSpec{LocalPort: "9001"}},
		cmd:  newDummyCmd(),
	}
	mgr.tunnels["9002"] = &tunnelEntry{
		info: TunnelInfo{HostName: "h", Spec: TunnelSpec{LocalPort: "9002"}},
		cmd:  newDummyCmd(),
	}

	err := mgr.CloseAll()
	assert.NoError(t, err)
	assert.Empty(t, mgr.tunnels)
}

// TestTunnelArgs_LocalDirection verifies the SSH args for local forwarding.
func TestTunnelArgs_LocalDirection(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	host := remote.RemoteHost{
		Name:    "h",
		Address: "10.0.0.1",
		User:    "user",
		Port:    22,
	}
	spec := TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemoteHost: "db-server",
		RemotePort: "5432",
	}

	args := mgr.tunnelArgs(host, spec)
	assert.Contains(t, args, "-N")
	// NET2-1: the -L flag and its forward spec are two independent argv
	// elements (never one combined "-L host:port:port" string).
	assert.Contains(t, args, "-L")
	assert.Contains(t, args, "8080:db-server:5432")
}

// TestTunnelArgs_RemoteDirection verifies the SSH args for remote forwarding.
func TestTunnelArgs_RemoteDirection(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	host := remote.RemoteHost{
		Name:    "h",
		Address: "10.0.0.1",
		User:    "user",
		KeyPath: "/home/user/.ssh/id_rsa",
		Port:    22,
	}
	spec := TunnelSpec{
		Direction:  TunnelRemote,
		LocalPort:  "8080",
		RemotePort: "9090",
	}

	args := mgr.tunnelArgs(host, spec)
	// NET2-1: the -R flag and its forward spec are two independent argv
	// elements (never one combined "-R host:port:port" string).
	assert.Contains(t, args, "-R")
	assert.Contains(t, args, "9090:localhost:8080")
	assert.Contains(t, args, "/home/user/.ssh/id_rsa")
}

// TestTunnelArgs_RemoteHost_DefaultsToLocalhost verifies empty
// RemoteHost in spec defaults to localhost.
func TestTunnelArgs_RemoteHost_DefaultsToLocalhost(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	host := remote.RemoteHost{
		Name: "h", Address: "10.0.0.1", User: "user", Port: 22,
	}
	spec := TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "8080",
		RemoteHost: "", // empty → should default to localhost
		RemotePort: "5432",
	}

	args := mgr.tunnelArgs(host, spec)
	// NET2-1: the -L flag and its forward spec (with the defaulted
	// "localhost" target) are two independent argv elements.
	assert.Contains(t, args, "-L")
	assert.Contains(t, args, "8080:localhost:5432")
}
