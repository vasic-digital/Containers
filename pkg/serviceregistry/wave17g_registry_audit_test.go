package serviceregistry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWave17_ServiceRegistry_AccessorsDeepCopyLabels proves Get / GetAll / List
// / Discover return a Service whose Labels map is a DEEP COPY, not an alias of
// the registry's internal map. A shallow struct copy (`copy := *svc`) copies
// only the map header, so a caller mutating the returned Labels would corrupt
// the registry's stored state — and a concurrent accessor ranging the same map
// (GetAll/List marshalling, or another Get) could panic with "concurrent map
// iteration and map write". RED on the pre-fix source: the caller's mutation
// leaks into the registry. The polarity harness reverts the deep copy per site.
func TestWave17_ServiceRegistry_AccessorsDeepCopyLabels(t *testing.T) {
	newReg := func(t *testing.T) *ServiceRegistry {
		t.Helper()
		r := New(WithRegistryDir(t.TempDir()))
		require.NoError(t, r.Register("web", 8080,
			WithLabels(map[string]string{"env": "prod", "tier": "db"})))
		return r
	}

	assertInternalIntact := func(t *testing.T, r *ServiceRegistry) {
		t.Helper()
		r.mu.RLock()
		got := r.services["web"].Labels
		r.mu.RUnlock()
		assert.Equal(t, "prod", got["env"],
			"internal Labels was corrupted through an aliased accessor return")
		assert.NotContains(t, got, "INJECTED",
			"a caller-injected key leaked into the registry's internal Labels")
	}

	t.Run("Get", func(t *testing.T) {
		r := newReg(t)
		svc, ok := r.Get("web")
		require.True(t, ok)
		svc.Labels["env"] = "CORRUPTED"
		svc.Labels["INJECTED"] = "x"
		assertInternalIntact(t, r)
	})

	t.Run("GetAll", func(t *testing.T) {
		r := newReg(t)
		all := r.GetAll()
		all["web"].Labels["env"] = "CORRUPTED"
		all["web"].Labels["INJECTED"] = "x"
		assertInternalIntact(t, r)
	})

	t.Run("List", func(t *testing.T) {
		r := newReg(t)
		list := r.List()
		require.Len(t, list, 1)
		list[0].Labels["env"] = "CORRUPTED"
		list[0].Labels["INJECTED"] = "x"
		assertInternalIntact(t, r)
	})

	t.Run("Discover", func(t *testing.T) {
		r := newReg(t)
		svc, err := r.Discover(context.Background(), "web", 8080)
		require.NoError(t, err)
		svc.Labels["env"] = "CORRUPTED"
		svc.Labels["INJECTED"] = "x"
		assertInternalIntact(t, r)
	})
}
