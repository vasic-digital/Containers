package network

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// TestWave16_Network_CreateTunnel_ReleasesPortOnStartFailure covers the tunnel
// port leak: when the ssh process fails to start, the port auto-allocated for
// the tunnel must be released. Otherwise no tunnelEntry is stored, CloseTunnel
// never runs to release it, and every failed launch permanently consumes one
// port from the range until the allocator is exhausted.
func TestWave16_Network_CreateTunnel_ReleasesPortOnStartFailure(t *testing.T) {
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"h1": {Name: "h1", Address: "10.0.0.1", User: "deploy"},
	}}
	mgr := NewTunnelManager(hm, logging.NopLogger{})

	require.Equal(t, 0, mgr.allocator.AllocatedCount(), "precondition: no ports allocated")

	// An already-cancelled context makes exec.CommandContext(...).Start()
	// return the context error deterministically, before ssh is launched
	// (verified: Start returns "context canceled", no process started).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Empty LocalPort forces an auto-allocation before the (failing) Start.
	_, err := mgr.CreateTunnel(ctx, "h1", TunnelSpec{
		Direction:  TunnelLocal,
		RemotePort: "5432",
	})
	require.Error(t, err)

	// The auto-allocated port must have been released on the failure path.
	assert.Equal(t, 0, mgr.allocator.AllocatedCount(),
		"auto-allocated port leaked when tunnel Start failed")
}
