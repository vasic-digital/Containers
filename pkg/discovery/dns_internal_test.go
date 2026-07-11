package discovery

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestDNSDiscoverer_Discover_DistinguishesIndeterminateFromNotFound is the
// §11.4.115 regression guard for DISC-3: a bare found==false conflates a
// genuinely-absent name (NXDOMAIN-class) with an indeterminate probe outcome
// (context cancel/timeout). Discover's doc comment promises this IS
// distinguishable by inspecting the wrapped error's class
// (errors.Is(err, context.Canceled/DeadlineExceeded), a net.Error.Timeout()
// assertion) — this test proves that promise holds for all three canonical
// classes, rather than trusting the doc comment alone.
//
// §11.4.115 surgical-revert evidence: changing the trailing "%w" verb to
// "%v" in DNSDiscoverer.Discover's lookup-error wrap (dns.go) breaks the
// Unwrap chain these assertions depend on. That revert was performed,
// confirmed `--- FAIL` on this test, and reverted back to "%w" — see the
// DISC-3 finding note in the containers pkg/discovery work log for the
// captured command + output.
func TestDNSDiscoverer_Discover_DistinguishesIndeterminateFromNotFound(t *testing.T) {
	target := DiscoveryTarget{
		Name:    "distinguish",
		Host:    "example.invalid",
		Method:  "dns",
		Timeout: time.Second,
	}

	t.Run("cancelled context is indeterminate", func(t *testing.T) {
		d := &DNSDiscoverer{lookup: &mockHostLookup{err: context.Canceled}}
		found, err := d.Discover(context.Background(), target)
		assert.False(t, found)
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled),
			"a cancelled probe MUST be classifiable as indeterminate via errors.Is(err, context.Canceled)")
	})

	t.Run("resolver timeout is indeterminate, not absence", func(t *testing.T) {
		d := &DNSDiscoverer{lookup: &mockHostLookup{
			err: &net.DNSError{Err: "i/o timeout", Name: target.Host, IsTimeout: true},
		}}
		found, err := d.Discover(context.Background(), target)
		assert.False(t, found)
		require.Error(t, err)
		assert.False(t, errors.Is(err, context.Canceled),
			"an indeterminate timeout MUST NOT be misclassified as the cancelled-context case")
		var netErr net.Error
		require.True(t, errors.As(err, &netErr), "wrapped cause must unwrap to a net.Error")
		assert.True(t, netErr.Timeout(),
			"an indeterminate resolver timeout MUST report Timeout()==true")
	})

	t.Run("NXDOMAIN-class not-found is definitive, not indeterminate", func(t *testing.T) {
		d := &DNSDiscoverer{lookup: &mockHostLookup{
			err: &net.DNSError{Err: "no such host", Name: target.Host, IsNotFound: true},
		}}
		found, err := d.Discover(context.Background(), target)
		assert.False(t, found)
		require.Error(t, err)
		assert.False(t, errors.Is(err, context.Canceled),
			"a definitive not-found MUST NOT be misclassified as the cancelled-context case")
		var netErr net.Error
		require.True(t, errors.As(err, &netErr))
		assert.False(t, netErr.Timeout(),
			"a definitive not-found MUST NOT report Timeout()==true (that signature is reserved for the indeterminate case)")
		var dnsErr *net.DNSError
		require.True(t, errors.As(err, &dnsErr))
		assert.True(t, dnsErr.IsNotFound,
			"the definitive-absence detail must survive the wrap so callers can tell it apart from an indeterminate probe failure")
	})
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
