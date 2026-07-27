package network

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"digital.vasic.containers/pkg/logging"
)

// TestTunnelOverlay_ConcurrentAccess_Race exercises concurrent
// Create/Connect/Disconnect/List calls against a single shared
// TunnelOverlay instance.
//
// LEAD-1 (second-pass concurrency audit): unlike every sibling stateful
// type in this package — DefaultTunnelManager.tunnels (tunnel.go) and
// PortAllocator.allocated (port_allocator.go), both guarded by
// sync.Mutex — TunnelOverlay.networks was mutated by Create, Delete,
// Connect, Disconnect and read by List with no synchronization at all.
// That is a genuine, reachable data race: any two goroutines calling
// Connect/Disconnect/List concurrently on the same *TunnelOverlay race
// on the same map, and Go's runtime can (and reliably does, under
// enough concurrent writers) fatal with "concurrent map writes" even
// without -race.
//
// Run with `go test -race`: fails on the unguarded version, passes
// once TunnelOverlay.mu guards every access to networks.
func TestTunnelOverlay_ConcurrentAccess_Race(t *testing.T) {
	o := NewTunnelOverlay(nil, nil, nil, logging.NopLogger{})
	ctx := context.Background()

	const networkName = "race-net"
	if err := o.Create(ctx, networkName); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const workers = 25
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		id := "container-" + strconv.Itoa(i)

		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = o.Connect(ctx, networkName, id)
		}()
		go func() {
			defer wg.Done()
			_ = o.Disconnect(ctx, networkName, id)
		}()
		go func() {
			defer wg.Done()
			_, _ = o.List(ctx)
		}()
	}
	wg.Wait()

	// Sanity: the overlay must still be in a consistent, readable state
	// after the concurrent storm (not merely "didn't crash").
	names, err := o.List(ctx)
	if err != nil {
		t.Fatalf("List after concurrent access: %v", err)
	}
	found := false
	for _, n := range names {
		if n == networkName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected network %q to still be present, got %v", networkName, names)
	}
}
