package serviceregistry

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wave-20 SR2-HARD cluster guards (second audit pass over pkg/serviceregistry).
//
// Each guard is a GREEN-polarity regression guard (§11.4.115): it PASSES on
// the fixed tree, and a surgical revert of the corresponding fix reproduces a
// deterministic `--- FAIL` (captured as evidence, then restored). Guards
// reuse the package's existing test seams and helpers (captureLogger from
// wave16_serviceregistry_audit_test.go, readDiskServices from
// wave20_srhard_test.go) rather than reinventing them (§11.4.27).

// TestSR2_1_NullServiceEntry_DroppedNotStored proves SR2-1: a registry file
// containing `{"foo": null}` is syntactically valid JSON (json.Unmarshal
// succeeds, yielding a nil *Service for "foo") but must NOT be stored as-is —
// every accessor (Get/GetAll/List/UpdateHealth/Discover's cached path)
// dereferences the stored *Service, and a stored nil would nil-deref on the
// very first call. persist() would also happily re-marshal the nil back to
// `null`, making the corruption self-perpetuating across restarts.
func TestSR2_1_NullServiceEntry_DroppedNotStored(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "services.json")
	require.NoError(t, os.WriteFile(file, []byte(`{
		"foo": null,
		"bar": {"name":"bar","host":"localhost","port":9001,"protocol":"tcp","healthy":true}
	}`), 0644))

	lg := &captureLogger{}
	var r *ServiceRegistry
	require.NotPanics(t, func() {
		r = New(WithRegistryDir(dir), WithLogger(lg))
	}, "SR2-1: constructing a registry from a file with a null service entry must not panic")

	require.NotPanics(t, func() { r.List() }, "List must not nil-deref a dropped null entry")
	require.NotPanics(t, func() { r.GetAll() }, "GetAll must not nil-deref a dropped null entry")
	require.NotPanics(t, func() { r.Get("foo") }, "Get must not nil-deref a dropped null entry")

	all := r.GetAll()
	assert.NotContains(t, all, "foo", "SR2-1: a null JSON entry must never be stored as a service")
	assert.Contains(t, all, "bar", "a sibling valid entry in the same file must still load normally")
	assert.True(t, lg.hasError(), "SR2-1: dropping a null entry must be logged at Error level, not swallowed")

	// The registry must remain fully usable afterward (e.g. still able to
	// register and persist new services) -- the corruption must not wedge it.
	require.NoError(t, r.Register("baz", 9002))
	_, ok := r.Get("baz")
	assert.True(t, ok)
}

// TestSR2_2_UnregisterPersistFailureSurfaced_AndNoResurrectionMasking proves
// the Unregister half of SR2-2: the Wave-20 SR-HARD-3 fix propagated
// persist() failure for Register only; Unregister discarded it (void
// signature). A failed persist leaves the OLD (still-registered) record on
// disk, so a restart against the original directory resurrects a service the
// caller believed was gone. Unregister now returns that failure.
func TestSR2_2_UnregisterPersistFailureSurfaced_AndNoResurrectionMasking(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-a", 8001))
	require.Contains(t, readDiskServices(t, dir), "svc-a", "precondition: svc-a persisted before the break")

	// Force persist() to fail deterministically, same technique as the
	// SR-HARD-3 guard: point registryDir at a path whose parent is a regular
	// file, so os.MkdirAll fails with ENOTDIR.
	badParent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badParent, []byte("x"), 0o644))
	r.mu.Lock()
	r.registryDir = filepath.Join(badParent, "sub")
	r.mu.Unlock()

	err := r.Unregister("svc-a")
	require.Error(t, err, "SR2-2: Unregister must propagate persist() failure, not silently discard it")

	// The in-memory unregister still took effect ...
	_, ok := r.Get("svc-a")
	assert.False(t, ok, "in-memory unregister should still take effect even though persistence failed")

	// ... but the ORIGINAL, valid directory still has the STALE (pre-
	// unregister) snapshot on disk, because the failed persist() never
	// reached it. A fresh restart against that directory resurrects the
	// service the caller believed was gone -- the surfaced error above is
	// what lets an operator detect and react to exactly this outcome instead
	// of being told (via the old void return) that everything went fine.
	restarted := New(WithRegistryDir(dir))
	_, resurrected := restarted.Get("svc-a")
	assert.True(t, resurrected,
		"demonstration: without Unregister surfacing the persist failure, this resurrection "+
			"on restart would have gone completely undetected")
}

// TestSR2_2_UpdateHealthPersistFailureSurfaced_AndNoStaleHealthMasking proves
// the UpdateHealth half of SR2-2: a discarded persist() failure here left a
// STALE health status on disk, so a restart reverted the service to a status
// the in-memory flip had already superseded.
func TestSR2_2_UpdateHealthPersistFailureSurfaced_AndNoStaleHealthMasking(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-b", 8002))
	require.NoError(t, r.UpdateHealth("svc-b", false))

	onDisk := readDiskServices(t, dir)
	require.Contains(t, onDisk, "svc-b")
	require.False(t, onDisk["svc-b"].Healthy, "precondition: unhealthy status persisted before the break")

	badParent := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badParent, []byte("x"), 0o644))
	r.mu.Lock()
	r.registryDir = filepath.Join(badParent, "sub")
	r.mu.Unlock()

	err := r.UpdateHealth("svc-b", true)
	require.Error(t, err, "SR2-2: UpdateHealth must propagate persist() failure, not silently discard it")

	svc, ok := r.Get("svc-b")
	require.True(t, ok)
	assert.True(t, svc.Healthy, "in-memory health flip should still take effect even though persistence failed")

	// The ORIGINAL directory still has the STALE (unhealthy) status on disk.
	restarted := New(WithRegistryDir(dir))
	stale, ok := restarted.Get("svc-b")
	require.True(t, ok)
	assert.False(t, stale.Healthy,
		"demonstration: without UpdateHealth surfacing the persist failure, this stale-health "+
			"revert on restart would have gone completely undetected")
}

// TestSR2_2_NegativeControl_HealthyCallsReturnNil is the negative control for
// SR2-2: a Unregister/UpdateHealth call whose persist() genuinely succeeds
// must NOT report a spurious error.
func TestSR2_2_NegativeControl_HealthyCallsReturnNil(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-ok", 8003))
	assert.NoError(t, r.UpdateHealth("svc-ok", false),
		"negative control: a persistable UpdateHealth must return nil")
	assert.NoError(t, r.Unregister("svc-ok"),
		"negative control: a persistable Unregister must return nil")
}

// TestSR2_3_OrphanedTempFileReapedOnStartup proves SR2-3: a crash (SIGKILL/
// OOM) between persist()'s tmp.Close() and its atomic os.Rename leaves an
// orphaned services-*.json.tmp file that nothing else in this package ever
// revisits -- an unbounded disk leak across repeated crash/restart cycles.
// The existing persistBeforeRenameHook seam parks a real persist() call at
// exactly that instant (temp file written and closed, rename not yet run)
// and never releases it, faithfully simulating the crash rather than
// hand-crafting an artifact of the right shape.
func TestSR2_3_OrphanedTempFileReapedOnStartup(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-x", 8001))

	atRename := make(chan struct{})
	block := make(chan struct{}) // deliberately never closed: simulates the process dying here
	var fired atomic.Bool
	prev := persistBeforeRenameHook
	persistBeforeRenameHook = func() {
		if fired.CompareAndSwap(false, true) {
			close(atRename)
			<-block
		}
	}
	defer func() { persistBeforeRenameHook = prev }()

	go func() { _ = r.persist() }() // "crashes" parked at the hook; its temp file is orphaned
	<-atRename

	// Confirm the orphan genuinely exists before asserting the reap, so this
	// guard would honestly RED-fail (not silently pass on an empty dir) if
	// the seam ever stopped reproducing the leftover.
	orphans, err := filepath.Glob(filepath.Join(dir, "services-*.json.tmp"))
	require.NoError(t, err)
	require.Len(t, orphans, 1, "precondition: exactly one orphaned temp file must exist on disk")

	r2 := New(WithRegistryDir(dir))
	require.NotNil(t, r2)

	orphans, err = filepath.Glob(filepath.Join(dir, "services-*.json.tmp"))
	require.NoError(t, err)
	assert.Empty(t, orphans, "SR2-3: a fresh New() must reap leftover services-*.json.tmp files")

	// The reap must not disturb the still-valid services.json committed by
	// the earlier, completed persist inside Register.
	_, ok := r2.Get("svc-x")
	assert.True(t, ok, "reaping an orphaned temp file must not remove or corrupt the valid registry")
}

// TestSR2_4_RegisterRejectsPortCollisionAcrossDifferentNames proves SR2-4:
// Register never checked for a port already claimed by a DIFFERENT service
// name, so two names could silently bind the same host:port -- a routing
// hazard for GetEndpoint/GetURL.
func TestSR2_4_RegisterRejectsPortCollisionAcrossDifferentNames(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-a", 8080))

	err := r.Register("svc-b", 8080)
	require.Error(t, err, "SR2-4: registering a second, DIFFERENT name at an already-claimed port must fail")
	assert.Contains(t, err.Error(), "svc-a", "the error should name the service that already holds the port")

	_, ok := r.Get("svc-b")
	assert.False(t, ok, "the colliding registration must not be stored")

	// Negative control: re-registering the SAME name at the same port is an
	// update, not a collision, and must still succeed.
	assert.NoError(t, r.Register("svc-a", 8080),
		"negative control: re-registering the same name at the same port must not error")

	// Negative control: a different name at a genuinely different port must
	// succeed.
	assert.NoError(t, r.Register("svc-c", 8081),
		"negative control: a distinct port must not be treated as a collision")
}

// TestSR2_4_RegisterAllowsSamePortOnDifferentHosts proves the collision
// check is host-scoped: two names binding the same port number on two
// DIFFERENT hosts is not a real collision (they are different sockets).
func TestSR2_4_RegisterAllowsSamePortOnDifferentHosts(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))
	require.NoError(t, r.Register("svc-host-a", 9090, WithHost("10.0.0.1")))
	assert.NoError(t, r.Register("svc-host-b", 9090, WithHost("10.0.0.2")),
		"the same port number on a DIFFERENT host is not a real collision")
}

// TestSR2_5_RegisterRejectsOutOfRangePort is the SR2-5 quick win: Register
// did not validate that port is within 1..65535, so a caller error (a
// negative port, an overflowed arithmetic result, a stray 0) would sit in the
// registry looking like a well-formed service.
func TestSR2_5_RegisterRejectsOutOfRangePort(t *testing.T) {
	dir := t.TempDir()
	r := New(WithRegistryDir(dir))

	for _, bad := range []int{0, -1, -8080, 65536, 100000} {
		err := r.Register("bad-port-svc", bad)
		assert.Errorf(t, err, "SR2-5: port %d is outside 1-65535 and must be rejected", bad)
		_, ok := r.Get("bad-port-svc")
		assert.Falsef(t, ok, "an invalid-port registration (port=%d) must not be stored", bad)
	}

	assert.NoError(t, r.Register("good-port-svc-low", 1), "port 1 is the valid lower bound")
	assert.NoError(t, r.Register("good-port-svc-high", 65535), "port 65535 is the valid upper bound")
}

// TestSR2_6_CorruptFilePreservation_DoesNotOverwritePriorForensicCopy proves
// the SR2-6 quick win: loadFromDisk's corrupt-file preservation always wrote
// to the same plain "<file>.corrupt" path, so a SECOND corruption event
// silently overwrote the FIRST forensic copy -- destroying the earlier
// evidence instead of accumulating it. The fix keeps the plain path for the
// first event (preserving the Wave-16 guard's exact assertion) and gives any
// later event its own distinctly-suffixed copy.
func TestSR2_6_CorruptFilePreservation_DoesNotOverwritePriorForensicCopy(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "services.json")

	require.NoError(t, os.WriteFile(file, []byte("{ first corruption not valid json"), 0644))
	r1 := New(WithRegistryDir(dir))
	require.NotNil(t, r1)

	firstCorrupt := file + ".corrupt"
	firstBytes, err := os.ReadFile(firstCorrupt)
	require.NoError(t, err, "first corrupt snapshot must be preserved at the plain .corrupt path")

	// A second corruption event lands at the same services.json path.
	require.NoError(t, os.WriteFile(file, []byte("{ second corruption also not valid json"), 0644))
	r2 := New(WithRegistryDir(dir))
	require.NotNil(t, r2)

	stillThere, err := os.ReadFile(firstCorrupt)
	require.NoError(t, err, "SR2-6: the first .corrupt forensic copy must survive a second corruption event")
	assert.Equal(t, firstBytes, stillThere, "SR2-6: the first .corrupt copy's bytes must be unchanged")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	corruptCount := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt") {
			corruptCount++
		}
	}
	assert.GreaterOrEqual(t, corruptCount, 2,
		"SR2-6: both corruption events must have a surviving forensic copy on disk, not just one")
}
