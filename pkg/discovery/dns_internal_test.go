package discovery

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestDefaultDiscoveryTimeout_Value locks the single hoisted default probe
// timeout shared by the DNS and TCP discoverers (§6.R de-duplication). This
// is a HYGIENE test pinning the const's value — NOT a §11.4.115 RED->GREEN
// behavioural guard: hoisting the two duplicate literals into this const
// preserves the identical 5s value, there is no defect to reproduce.
func TestDefaultDiscoveryTimeout_Value(t *testing.T) {
	assert.Equal(t, 5*time.Second, defaultDiscoveryTimeout)
}

// mockHostLookup is a test double for DNS lookups.
type mockHostLookup struct {
	addrs []string
	err   error
}

func (m *mockHostLookup) LookupHost(
	_ context.Context, _ string,
) ([]string, error) {
	return m.addrs, m.err
}

// TestDNSDiscoverer_Discover_EmptyAddresses tests the case where DNS
// lookup returns no addresses (empty slice) without an error.
func TestDNSDiscoverer_Discover_EmptyAddresses(t *testing.T) {
	d := &DNSDiscoverer{
		lookup: &mockHostLookup{
			addrs: []string{}, // Empty but no error
			err:   nil,
		},
	}

	ctx := context.Background()
	target := DiscoveryTarget{
		Name:    "empty-result",
		Host:    "example.com",
		Method:  "dns",
		Timeout: time.Second,
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no addresses found")
}

// TestDNSDiscoverer_Discover_WithMockSuccess tests successful lookup with mock.
func TestDNSDiscoverer_Discover_WithMockSuccess(t *testing.T) {
	d := &DNSDiscoverer{
		lookup: &mockHostLookup{
			addrs: []string{"192.168.1.1", "192.168.1.2"},
			err:   nil,
		},
	}

	ctx := context.Background()
	target := DiscoveryTarget{
		Name:    "mock-success",
		Host:    "example.com",
		Method:  "dns",
		Timeout: time.Second,
	}

	found, err := d.Discover(ctx, target)
	assert.True(t, found)
	assert.NoError(t, err)
}

// TestDNSDiscoverer_Discover_WithMockError tests lookup error with mock.
func TestDNSDiscoverer_Discover_WithMockError(t *testing.T) {
	d := &DNSDiscoverer{
		lookup: &mockHostLookup{
			addrs: nil,
			err:   errors.New("network unreachable"),
		},
	}

	ctx := context.Background()
	target := DiscoveryTarget{
		Name:    "mock-error",
		Host:    "example.com",
		Method:  "dns",
		Timeout: time.Second,
	}

	found, err := d.Discover(ctx, target)
	assert.False(t, found)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "network unreachable")
}

// TestDNSDiscoverer_Discover_NilLookup tests that nil lookup uses default.
func TestDNSDiscoverer_Discover_NilLookup(t *testing.T) {
	d := &DNSDiscoverer{
		lookup: nil, // Explicitly nil
	}

	ctx := context.Background()
	target := DiscoveryTarget{
		Name:    "nil-lookup",
		Host:    "localhost",
		Method:  "dns",
		Timeout: time.Second,
	}

	// Should use default lookup (real DNS)
	found, err := d.Discover(ctx, target)
	assert.True(t, found)
	assert.NoError(t, err)
}

// TestDefaultHostLookup tests the default host lookup implementation.
func TestDefaultHostLookup(t *testing.T) {
	lookup := &defaultHostLookup{resolver: nil}
	// Creating with nil resolver will use default resolver
	lookup = &defaultHostLookup{resolver: new(net.Resolver)}

	ctx := context.Background()
	addrs, err := lookup.LookupHost(ctx, "localhost")

	// Should successfully resolve localhost
	assert.NoError(t, err)
	assert.NotEmpty(t, addrs)
}
