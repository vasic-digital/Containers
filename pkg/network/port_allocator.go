package network

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// PortAllocator manages thread-safe allocation of local ports
// within a configured range.
type PortAllocator struct {
	mu        sync.Mutex
	start     int
	end       int
	allocated map[int]*PortAllocation
	next      int
}

// NewPortAllocator creates a PortAllocator for the given range.
func NewPortAllocator(start, end int) *PortAllocator {
	return &PortAllocator{
		start:     start,
		end:       end,
		allocated: make(map[int]*PortAllocation),
		next:      start,
	}
}

// Allocate finds an available port in the range and reserves it.
func (a *PortAllocator) Allocate(
	description string,
) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rangeSize := a.end - a.start
	for i := 0; i < rangeSize; i++ {
		port := a.start + ((a.next - a.start + i) % rangeSize)

		if _, taken := a.allocated[port]; taken {
			continue
		}

		if !isPortAvailable(port) {
			continue
		}

		a.allocated[port] = &PortAllocation{
			Port:        port,
			Description: description,
			AllocatedAt: time.Now(),
		}
		a.next = port + 1
		if a.next >= a.end {
			a.next = a.start
		}
		return port, nil
	}

	return 0, fmt.Errorf(
		"no available ports in range %d-%d", a.start, a.end,
	)
}

// Release frees a previously allocated port.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, port)
}

// MarkAllocated directly records an externally-chosen port as reserved,
// without scanning the range or requiring the port to fall inside
// [start,end). It exists for callers that pick their own port outside the
// normal Allocate() flow — e.g. an explicit TunnelSpec.LocalPort (NET2-4) —
// so IsAllocated / AllocatedCount / ListAllocations reflect that port too,
// instead of only ever seeing ports this allocator itself chose. Idempotent:
// calling it again for an already-recorded port just refreshes the
// description and timestamp. Release(port) undoes it exactly like any
// Allocate()-assigned port.
func (a *PortAllocator) MarkAllocated(port int, description string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allocated[port] = &PortAllocation{
		Port:        port,
		Description: description,
		AllocatedAt: time.Now(),
	}
}

// IsAllocated checks whether a port is currently reserved.
func (a *PortAllocator) IsAllocated(port int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.allocated[port]
	return ok
}

// AllocatedCount returns the number of reserved ports.
func (a *PortAllocator) AllocatedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.allocated)
}

// ListAllocations returns all current allocations.
func (a *PortAllocator) ListAllocations() []PortAllocation {
	a.mu.Lock()
	defer a.mu.Unlock()

	allocs := make([]PortAllocation, 0, len(a.allocated))
	for _, pa := range a.allocated {
		allocs = append(allocs, *pa)
	}
	return allocs
}

// ReleaseAll frees all allocated ports.
func (a *PortAllocator) ReleaseAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allocated = make(map[int]*PortAllocation)
	a.next = a.start
}

// isPortAvailable checks if a TCP port is free by attempting to
// listen on it.
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp",
		fmt.Sprintf("127.0.0.1:%d", port),
	)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
