package network

import (
	"context"
	"fmt"
	"sync"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// OverlayNetwork manages cross-host container networking using
// Docker overlay networks or SSH tunnel-based alternatives.
type OverlayNetwork interface {
	// Create creates a named overlay network.
	Create(ctx context.Context, name string) error
	// Delete deletes a named overlay network.
	Delete(ctx context.Context, name string) error
	// Connect connects a container to the overlay network.
	Connect(ctx context.Context, network, containerID string) error
	// Disconnect removes a container from the overlay network.
	Disconnect(ctx context.Context, network, containerID string) error
	// List returns all overlay networks.
	List(ctx context.Context) ([]string, error)
}

// TunnelOverlay implements OverlayNetwork. NOTE: it currently maintains only
// an in-memory registry of network -> container-ID membership; it does NOT
// yet perform any host-side networking (no Docker/bridge network is created,
// no SSH transport is wired). The injected tunnelManager/hostManager/executor
// are the seam for a real cross-host implementation, which is a tracked gap —
// until then a nil return from these methods means "recorded in memory", NOT
// "cross-host connectivity established". Callers MUST NOT treat success here
// as proof that containers on different hosts can actually reach each other.
type TunnelOverlay struct {
	tunnelManager TunnelManager
	hostManager   remote.HostManager
	executor      remote.RemoteExecutor
	logger        logging.Logger
	// mu guards networks. Every sibling stateful type in this package
	// (DefaultTunnelManager.tunnels in tunnel.go, PortAllocator.allocated
	// in port_allocator.go) protects its map with a mutex; networks was
	// the sole exception, mutated by Create/Delete/Connect/Disconnect/
	// List with no synchronization — a genuine data race (concurrent
	// map read/write, detectable by `go test -race` and by Go's runtime
	// concurrent-map-write fatal error) the moment two goroutines call
	// any of those methods on the same *TunnelOverlay concurrently, a
	// realistic pattern for a network manager shared across a
	// distribution/orchestration flow.
	mu       sync.Mutex
	networks map[string][]string // network -> container IDs
}

// NewTunnelOverlay creates an overlay backed by SSH tunnels.
func NewTunnelOverlay(
	tunnelManager TunnelManager,
	hostManager remote.HostManager,
	executor remote.RemoteExecutor,
	logger logging.Logger,
) *TunnelOverlay {
	if logger == nil {
		logger = logging.NopLogger{}
	}
	return &TunnelOverlay{
		tunnelManager: tunnelManager,
		hostManager:   hostManager,
		executor:      executor,
		logger:        logger,
		networks:      make(map[string][]string),
	}
}

// Create records a named overlay network in the in-memory registry. NOTE: it
// does NOT (yet) create any host-side Docker/bridge network or SSH transport;
// the real cross-host wiring is a tracked gap (see the TunnelOverlay type doc).
func (o *TunnelOverlay) Create(
	ctx context.Context, name string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, exists := o.networks[name]; exists {
		return fmt.Errorf(
			"overlay network %q already exists", name,
		)
	}

	o.networks[name] = nil
	o.logger.Info("created overlay network %s", name)
	return nil
}

// Delete removes a named overlay network.
func (o *TunnelOverlay) Delete(
	ctx context.Context, name string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, exists := o.networks[name]; !exists {
		return fmt.Errorf(
			"overlay network %q not found", name,
		)
	}

	delete(o.networks, name)
	o.logger.Info("deleted overlay network %s", name)
	return nil
}

// Connect adds a container to the overlay network.
func (o *TunnelOverlay) Connect(
	ctx context.Context, network, containerID string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	containers, exists := o.networks[network]
	if !exists {
		return fmt.Errorf(
			"overlay network %q not found", network,
		)
	}

	o.networks[network] = append(containers, containerID)
	o.logger.Info("connected %s to network %s",
		containerID, network,
	)
	return nil
}

// Disconnect removes a container from the overlay network.
func (o *TunnelOverlay) Disconnect(
	ctx context.Context, network, containerID string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	containers, exists := o.networks[network]
	if !exists {
		return fmt.Errorf(
			"overlay network %q not found", network,
		)
	}

	filtered := make([]string, 0, len(containers))
	for _, c := range containers {
		if c != containerID {
			filtered = append(filtered, c)
		}
	}
	o.networks[network] = filtered
	o.logger.Info("disconnected %s from network %s",
		containerID, network,
	)
	return nil
}

// List returns all overlay networks.
func (o *TunnelOverlay) List(
	ctx context.Context,
) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	names := make([]string, 0, len(o.networks))
	for name := range o.networks {
		names = append(names, name)
	}
	return names, nil
}
