package boot_test

import (
	"context"
	"testing"

	"digital.vasic.containers/pkg/boot"
	"digital.vasic.containers/pkg/endpoint"

	"github.com/stretchr/testify/assert"
)

// TestBootAll_DiscoveredRequiredHealthFail_CountingBug proves that when a
// DISCOVERED endpoint later fails a REQUIRED health check, the boot summary
// wrongly decrements the Started (or Remote) counter even though the endpoint
// was counted in Discovered, never in Started. The discovered endpoint takes
// the Phase-1 "discovered" continue path, so it is never counted in Started
// nor Remote; decrementing Started in Phase 3 corrupts the summary.
func TestBootAll_DiscoveredRequiredHealthFail_CountingBug(t *testing.T) {
	eps := map[string]endpoint.ServiceEndpoint{
		"svc": endpoint.NewEndpoint().
			WithHost("h").WithPort("1").
			WithEnabled(true).
			WithRequired(true).
			WithDiscoveryEnabled(true).
			WithHealthType("tcp").
			Build(),
	}

	bm := boot.NewBootManager(
		eps,
		boot.WithDiscoverer(&mockDiscoverer{found: map[string]bool{"svc": true}}),
		boot.WithHealthChecker(&mockHealthChecker{results: map[string]bool{"svc": false}}),
	)

	summary, err := bm.BootAll(context.Background())
	assert.Error(t, err) // required svc failed health -> boot error

	// The svc was discovered (Discovered=1) then failed required health
	// (Failed=1). It was NEVER in Started, so Started MUST stay 0.
	assert.Equal(t, 1, summary.Failed, "failed count")
	assert.Equal(t, 0, summary.Started,
		"Started must not be decremented for a discovered endpoint")
	// And the result status must reflect the failure.
	assert.Equal(t, "failed", summary.Results["svc"].Status)
}
