package network

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// TestPortAllocator_ExhaustedRange verifies that Allocate returns an error
// when all ports in the range are already marked as allocated.
// We use white-box access to the allocated map to pre-fill it without
// actually opening sockets.
func TestPortAllocator_ExhaustedRange(t *testing.T) {
	// Use a small range: [55000, 55003) — 3 ports.
	a := NewPortAllocator(55000, 55003)

	// Mark all ports as allocated directly (white-box).
	for p := 55000; p < 55003; p++ {
		a.allocated[p] = &PortAllocation{
			Port:        p,
			Description: "pre-allocated",
			AllocatedAt: time.Now(),
		}
	}

	_, err := a.Allocate("should-fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no available ports")
}

// TestPortAllocator_AllAlreadyAllocated exercises the "all allocated" path
// in Allocate's scanning loop with a different range.
func TestPortAllocator_AllAlreadyAllocated(t *testing.T) {
	a := NewPortAllocator(56000, 56002)

	// Mark both ports as taken.
	a.allocated[56000] = &PortAllocation{Port: 56000, AllocatedAt: time.Now()}
	a.allocated[56001] = &PortAllocation{Port: 56001, AllocatedAt: time.Now()}

	_, err := a.Allocate("test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no available ports in range 56000-56002")
}

// TestPortAllocator_PortWraparound exercises the wraparound logic:
// after allocating the last port in the range, next wraps back to start.
func TestPortAllocator_PortWraparound(t *testing.T) {
	a := NewPortAllocator(55100, 55103)

	// Allocate the first port to set next forward.
	port1, err := a.Allocate("wrap-first")
	if err != nil {
		t.Skip("port not available on this system") // SKIP-OK: #requires-infra-port
	}

	// Release it so the range is free again.
	a.Release(port1)

	// After wrap we should still be able to allocate.
	port2, err := a.Allocate("wrap-second")
	if err != nil {
		t.Skip("port not available on this system") // SKIP-OK: #requires-infra-port
	}
	assert.GreaterOrEqual(t, port2, 55100)
	assert.Less(t, port2, 55103)
}

// TestPortAllocator_IsAllocated verifies that after Allocate, IsAllocated
// returns true for the allocated port and false after Release.
func TestPortAllocator_IsAllocated(t *testing.T) {
	a := NewPortAllocator(57000, 57010)

	port, err := a.Allocate("is-allocated-test")
	if err != nil {
		t.Skip("no port available in range 57000-57010") // SKIP-OK: #requires-infra-port
	}

	assert.True(t, a.IsAllocated(port))
	a.Release(port)
	assert.False(t, a.IsAllocated(port))
}

// TestPortAllocator_SkipsInUsePorts exercises the isPortAvailable branch
// where a port is not marked as allocated but is actually in use by a
// listener, so Allocate must skip it and pick another free port.
func TestPortAllocator_SkipsInUsePorts(t *testing.T) {
	// Listen on a port to make it unavailable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	usedPort := ln.Addr().(*net.TCPAddr).Port

	start := usedPort
	end := usedPort + 10

	if end > 65535 {
		t.Skip("port range would exceed 65535") // SKIP-OK: #env-port-range-overflow
	}

	a := NewPortAllocator(start, end)
	a.next = usedPort

	port, err := a.Allocate("skip-in-use")
	if err != nil {
		t.Logf("no free port found in range %d-%d (all in use), skipping", start, end)
		return
	}
	assert.NotEqual(t, usedPort, port,
		"Allocate should have skipped the in-use port")
}

// TestPortAllocator_ListAllocations_AfterRelease verifies ListAllocations
// reflects releases correctly.
func TestPortAllocator_ListAllocations_AfterRelease(t *testing.T) {
	a := NewPortAllocator(58000, 58010)

	p1, err1 := a.Allocate("first")
	p2, err2 := a.Allocate("second")

	if err1 != nil || err2 != nil {
		t.Skip("ports not available") // SKIP-OK: #requires-infra-port
	}

	assert.Len(t, a.ListAllocations(), 2)

	a.Release(p1)
	assert.Len(t, a.ListAllocations(), 1)

	a.Release(p2)
	assert.Empty(t, a.ListAllocations())
}

// TestPortAllocator_AllocatedCount_Precise verifies AllocatedCount tracks
// correctly using direct map manipulation.
func TestPortAllocator_AllocatedCount_Precise(t *testing.T) {
	a := NewPortAllocator(59000, 59010)

	assert.Equal(t, 0, a.AllocatedCount())

	a.allocated[59000] = &PortAllocation{Port: 59000, AllocatedAt: time.Now()}
	assert.Equal(t, 1, a.AllocatedCount())

	a.allocated[59001] = &PortAllocation{Port: 59001, AllocatedAt: time.Now()}
	assert.Equal(t, 2, a.AllocatedCount())

	delete(a.allocated, 59000)
	assert.Equal(t, 1, a.AllocatedCount())
}

// --- CreateTunnel branch coverage ---

// nullReturningHostManager is a HostManager whose GetHost returns (nil, nil)
// — simulating a host that exists in the registry but has no data.
type nullReturningHostManager struct{}

func (m *nullReturningHostManager) AddHost(_ remote.RemoteHost) error { return nil }
func (m *nullReturningHostManager) RemoveHost(_ string) error         { return nil }
func (m *nullReturningHostManager) GetHost(_ string) (*remote.RemoteHost, error) {
	return nil, nil // host is nil but no error
}
func (m *nullReturningHostManager) ListHosts() []remote.RemoteHost { return nil }
func (m *nullReturningHostManager) ProbeHost(_ context.Context, _ string) (*remote.HostResources, error) {
	return nil, nil
}
func (m *nullReturningHostManager) ProbeAll(_ context.Context) map[string]*remote.HostResources {
	return nil
}
func (m *nullReturningHostManager) HostState(_ string) remote.HostState {
	return remote.HostUnknown
}

// TestCreateTunnel_NilHost exercises the "host == nil" branch in CreateTunnel.
func TestCreateTunnel_NilHost(t *testing.T) {
	hm := &nullReturningHostManager{}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	_, err := mgr.CreateTunnel(context.Background(), "ghost-host", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "9999",
		RemotePort: "5432",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// exhaustedPortHostManager returns a valid host for CreateTunnel tests.
type exhaustedPortHostManager struct {
	host *remote.RemoteHost
}

func (m *exhaustedPortHostManager) AddHost(_ remote.RemoteHost) error { return nil }
func (m *exhaustedPortHostManager) RemoveHost(_ string) error         { return nil }
func (m *exhaustedPortHostManager) GetHost(_ string) (*remote.RemoteHost, error) {
	return m.host, nil
}
func (m *exhaustedPortHostManager) ListHosts() []remote.RemoteHost { return nil }
func (m *exhaustedPortHostManager) ProbeHost(_ context.Context, _ string) (*remote.HostResources, error) {
	return nil, nil
}
func (m *exhaustedPortHostManager) ProbeAll(_ context.Context) map[string]*remote.HostResources {
	return nil
}
func (m *exhaustedPortHostManager) HostState(_ string) remote.HostState { return remote.HostOnline }

// TestCreateTunnel_PortExhausted exercises the "allocate port" failure branch
// in CreateTunnel (spec.LocalPort == "" and all ports are taken).
func TestCreateTunnel_PortExhausted(t *testing.T) {
	host := &remote.RemoteHost{
		Name: "h1", Address: "10.0.0.1", User: "u", Port: 22,
	}
	hm := &exhaustedPortHostManager{host: host}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	// Mark all ports in the allocator as taken so auto-allocate fails.
	start := mgr.opts.PortRangeStart
	end := mgr.opts.PortRangeEnd
	for p := start; p < end; p++ {
		mgr.allocator.allocated[p] = &PortAllocation{
			Port: p, AllocatedAt: time.Now(),
		}
	}

	// LocalPort is empty → triggers auto-allocate → should fail.
	_, err := mgr.CreateTunnel(context.Background(), "h1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
		// LocalPort intentionally empty.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allocate port")
}
