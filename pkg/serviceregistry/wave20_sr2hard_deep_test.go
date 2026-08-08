package serviceregistry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wave-20 SR2-HARD DEEP cluster guards (third audit pass over
// pkg/serviceregistry — defects that BOTH the SR-HARD (persist-ordering /
// Clear-race / silent-persist) AND the SR2-HARD (null-entry / persist-failure
// surfacing / orphan-reap / port-collision / port-range / corrupt-preserve)
// passes missed).
//
// Each guard is a GREEN-polarity regression guard (§11.4.115): it PASSES on the
// fixed tree, and a surgical single-line revert of the corresponding fix
// reproduces a deterministic `--- FAIL` (captured as evidence, then restored).
// Both guards are timing-categorical, never flaky (§11.4.50): SR2-DEEP-1
// separates two timestamps with a real elapsed sleep, SR2-DEEP-2 asserts on a
// captured log argument. Written to a `_deep` sibling so the already-landed
// wave20_sr2hard_test.go is untouched (§11.4.122/§11.4.124).

// infoCaptureLogger records the integer argument of the loadFromDisk
// "Loaded %d services from registry" Info line so SR2-DEEP-2 can assert the
// reported count equals the count actually stored (not len(loaded)).
type infoCaptureLogger struct {
	mu          sync.Mutex
	loadedCount int
	gotLoaded   bool
}

func (l *infoCaptureLogger) Info(msg string, args ...any) {
	if strings.Contains(msg, "Loaded") && strings.Contains(msg, "services from registry") && len(args) >= 1 {
		if n, ok := args[0].(int); ok {
			l.mu.Lock()
			l.loadedCount = n
			l.gotLoaded = true
			l.mu.Unlock()
		}
	}
}
func (l *infoCaptureLogger) Debug(string, ...any) {}
func (l *infoCaptureLogger) Warn(string, ...any)  {}
func (l *infoCaptureLogger) Error(string, ...any) {}
func (l *infoCaptureLogger) loaded() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadedCount, l.gotLoaded
}

// TestWave20_SR2_DiscoveredAtPreservedOnReRegister proves SR2-DEEP-1: Register
// always builds a FRESH *Service, so re-registering an already-known name
// (a refresh / heartbeat / port or label change) clobbered DiscoveredAt with
// the current time on every call — erasing the service's true first-seen age,
// even though the distinct LastChecked field already tracks the latest touch.
// The fix preserves the prior DiscoveredAt on re-register and only refreshes
// LastChecked.
//
// Deterministic (never flaky): a real ≥5ms sleep separates the first-seen time
// from the re-register time, so the fixed value (exactly equal to the original)
// and the reverted value (a strictly-later time.Now()) are categorically
// distinguishable — no clock-resolution guessing.
func TestWave20_SR2_DiscoveredAtPreservedOnReRegister(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))

	require.NoError(t, r.Register("web", 8080, WithHealthPath("/a")))
	first, ok := r.Get("web")
	require.True(t, ok)
	firstDiscovered := first.DiscoveredAt
	require.False(t, firstDiscovered.IsZero(), "precondition: first registration stamps DiscoveredAt")

	// Real elapsed time so a clobbered DiscoveredAt would land strictly later
	// than the original — categorical, never a nanosecond-resolution gamble.
	time.Sleep(5 * time.Millisecond)

	// Re-register the SAME name (an update: new health path, same port). This is
	// the refresh/heartbeat path, NOT a first discovery.
	require.NoError(t, r.Register("web", 8080, WithHealthPath("/b")))
	second, ok := r.Get("web")
	require.True(t, ok)

	// SR2-DEEP-1: the first-seen timestamp must survive the re-register.
	assert.True(t, second.DiscoveredAt.Equal(firstDiscovered),
		"SR2-DEEP-1: DiscoveredAt (first-seen) must be PRESERVED across a re-register; "+
			"got %v, want %v (a re-register clobbered the true first-seen age)",
		second.DiscoveredAt, firstDiscovered)

	// Negative control: the re-register genuinely DID update the record —
	// LastChecked advanced past the original first-seen time, and the health
	// path change took effect. This proves the guard is not passing merely
	// because the second Register was a no-op.
	assert.True(t, second.LastChecked.After(firstDiscovered),
		"negative control: LastChecked must advance on re-register (record was actually updated)")
	assert.Equal(t, "/b", second.HealthPath,
		"negative control: the re-register's field change must take effect")
}

// TestWave20_SR2_LoadedCountExcludesDroppedNullEntries proves SR2-DEEP-2:
// loadFromDisk logged len(loaded) as the "Loaded %d services" count, which
// includes null entries that SR2-1 drops — so a file with one null and one
// valid entry reported "Loaded 2 services" while only 1 was actually stored,
// an operator-facing count that contradicts the Error-level null-drop log. The
// fix reports len(r.services): the count actually stored.
func TestWave20_SR2_LoadedCountExcludesDroppedNullEntries(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "services.json")
	// One null entry (dropped by SR2-1) + two valid entries => 2 stored.
	require.NoError(t, os.WriteFile(file, []byte(`{
		"ghost": null,
		"alpha": {"name":"alpha","host":"localhost","port":9101,"protocol":"tcp","healthy":true},
		"beta":  {"name":"beta","host":"localhost","port":9102,"protocol":"tcp","healthy":true}
	}`), 0644))

	lg := &infoCaptureLogger{}
	r := New(WithRegistryDir(dir), WithLogger(lg))

	// The valid entries loaded; the null was dropped.
	all := r.GetAll()
	require.Len(t, all, 2, "precondition: exactly the two valid entries must load (null dropped)")
	require.NotContains(t, all, "ghost")

	count, got := lg.loaded()
	require.True(t, got, "loadFromDisk must emit its 'Loaded N services' summary")
	assert.Equal(t, len(all), count,
		"SR2-DEEP-2: the logged 'Loaded N services' count must equal the number actually "+
			"stored (%d), not len(loaded) which over-counts the dropped null entry", len(all))
}
