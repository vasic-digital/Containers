package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
)

// depComposeOrch records the order + set of Up/Down calls (keyed by compose-file
// basename) and can fail a specific service's Up, so dependency-ordered startup
// is directly observable.
type depComposeOrch struct {
	mu    sync.Mutex
	upErr map[string]error
	ups   []string
	downs []string
}

func (d *depComposeOrch) Up(_ context.Context, p compose.ComposeProject) error {
	base := filepath.Base(p.File)
	d.mu.Lock()
	d.ups = append(d.ups, base)
	var err error
	if d.upErr != nil {
		err = d.upErr[base]
	}
	d.mu.Unlock()
	return err
}

func (d *depComposeOrch) Down(_ context.Context, p compose.ComposeProject) error {
	d.mu.Lock()
	d.downs = append(d.downs, filepath.Base(p.File))
	d.mu.Unlock()
	return nil
}

func (d *depComposeOrch) upFiles() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.ups))
	copy(out, d.ups)
	return out
}

func (d *depComposeOrch) downFiles() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.downs))
	copy(out, d.downs)
	return out
}

// orderComposeOrch proves ordering: dependency "a" blocks in its Up until the
// test releases it, then records that it returned; if dependent "b"'s Up runs
// before "a" has returned, it is recorded as a violation.
type orderComposeOrch struct {
	mu           sync.Mutex
	order        []string
	aReturned    bool
	bBeforeA     bool
	aEntered     chan struct{}
	aEnteredOnce sync.Once
	aRelease     chan struct{}
}

func (r *orderComposeOrch) Up(_ context.Context, p compose.ComposeProject) error {
	base := filepath.Base(p.File)
	r.mu.Lock()
	r.order = append(r.order, base)
	if base == "b.yml" && !r.aReturned {
		r.bBeforeA = true
	}
	r.mu.Unlock()

	if base == "a.yml" {
		r.aEnteredOnce.Do(func() { close(r.aEntered) })
		<-r.aRelease
		r.mu.Lock()
		r.aReturned = true
		r.mu.Unlock()
	}
	return nil
}

func (r *orderComposeOrch) Down(_ context.Context, _ compose.ComposeProject) error { return nil }

func (r *orderComposeOrch) bStartedBeforeAReturned() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bBeforeA
}

func (r *orderComposeOrch) upOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// TestWave16_Orchestrator_StartAll_StartsDependenciesBeforeDependents proves a
// dependent (b depends-on a) does not begin its own Up until its dependency's
// Up has returned. "a" blocks until the test releases it; under dependency
// waves b's level cannot start until a's level drains, so b never observes
// a-not-yet-returned. Under an unordered boot the two run concurrently and b
// begins while a is still blocked.
func TestWave16_Orchestrator_StartAll_StartsDependenciesBeforeDependents(t *testing.T) {
	tmpDir := t.TempDir()
	aPath := filepath.Join(tmpDir, "a.yml")
	bPath := filepath.Join(tmpDir, "b.yml")
	require.NoError(t, os.WriteFile(aPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("version: '3'\n"), 0644))

	rec := &orderComposeOrch{
		aEntered: make(chan struct{}),
		aRelease: make(chan struct{}),
	}
	o := New(WithLocalOrchestrator(rec), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "a", ComposeFile: aPath})
	o.AddService(Service{Name: "b", ComposeFile: bPath, Dependencies: []string{"a"}})

	done := make(chan error, 1)
	go func() { done <- o.StartAll(context.Background()) }()

	<-rec.aEntered      // a's Up is running (and blocked)
	close(rec.aRelease) // release a; only THEN may b's level start (under the fix)
	require.NoError(t, <-done)

	assert.False(t, rec.bStartedBeforeAReturned(),
		"dependent b must not begin its Up until dependency a's Up has returned")
	assert.ElementsMatch(t, []string{"a.yml", "b.yml"}, rec.upOrder(),
		"both services must eventually start")
}

// TestWave16_Orchestrator_StartAll_UnknownDependencyFailsBeforeStart proves an
// unknown dependency name fails StartAll up-front and starts nothing.
func TestWave16_Orchestrator_StartAll_UnknownDependencyFailsBeforeStart(t *testing.T) {
	tmpDir := t.TempDir()
	bPath := filepath.Join(tmpDir, "b.yml")
	require.NoError(t, os.WriteFile(bPath, []byte("version: '3'\n"), 0644))

	orch := &depComposeOrch{}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "b", ComposeFile: bPath, Dependencies: []string{"ghost"}})

	err := o.StartAll(context.Background())
	require.Error(t, err, "an unknown dependency must fail StartAll before any start")
	assert.Contains(t, err.Error(), "ghost")
	assert.Empty(t, orch.upFiles(),
		"no service may be started when a declared dependency is unknown")
}

// TestWave16_Orchestrator_StartAll_SkipsDependentOfFailedDependency proves a
// dependent is NOT started when its dependency fails, and (being Required) that
// skip fails StartAll.
func TestWave16_Orchestrator_StartAll_SkipsDependentOfFailedDependency(t *testing.T) {
	tmpDir := t.TempDir()
	aPath := filepath.Join(tmpDir, "a.yml")
	bPath := filepath.Join(tmpDir, "b.yml")
	require.NoError(t, os.WriteFile(aPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("version: '3'\n"), 0644))

	orch := &depComposeOrch{upErr: map[string]error{"a.yml": fmt.Errorf("boom")}}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "a", ComposeFile: aPath})
	o.AddService(Service{Name: "b", ComposeFile: bPath, Dependencies: []string{"a"}, Required: true})

	err := o.StartAll(context.Background())
	require.Error(t, err, "a required dependent whose dependency failed must fail StartAll")

	ups := orch.upFiles()
	assert.Contains(t, ups, "a.yml", "the dependency's start is attempted")
	assert.NotContains(t, ups, "b.yml",
		"the dependent must be skipped because its dependency a failed")
}

// TestWave16_Orchestrator_StartAll_DependencyCycleFailsNotDeadlock proves a
// dependency cycle fails StartAll with a "cycle" error BEFORE any start —
// crucially it must error, not deadlock the goroutine boot.
func TestWave16_Orchestrator_StartAll_DependencyCycleFailsNotDeadlock(t *testing.T) {
	tmpDir := t.TempDir()
	aPath := filepath.Join(tmpDir, "a.yml")
	bPath := filepath.Join(tmpDir, "b.yml")
	require.NoError(t, os.WriteFile(aPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("version: '3'\n"), 0644))

	orch := &depComposeOrch{}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "a", ComposeFile: aPath, Dependencies: []string{"b"}})
	o.AddService(Service{Name: "b", ComposeFile: bPath, Dependencies: []string{"a"}})

	err := o.StartAll(context.Background())
	require.Error(t, err, "a dependency cycle must fail StartAll (never deadlock)")
	assert.Contains(t, err.Error(), "cycle")
	assert.Empty(t, orch.upFiles(),
		"no service may start when there is a dependency cycle")
}

// TestWave16_Orchestrator_StartAll_RollsBackEarlierWaveOnLaterWaveFailure proves
// the cross-level rollback: a service started in an EARLIER dependency wave is
// torn down when a LATER wave's required service fails. This exercises the
// started-accumulator threaded across all levels (not reset per level).
func TestWave16_Orchestrator_StartAll_RollsBackEarlierWaveOnLaterWaveFailure(t *testing.T) {
	tmpDir := t.TempDir()
	aPath := filepath.Join(tmpDir, "a.yml")
	bPath := filepath.Join(tmpDir, "b.yml")
	require.NoError(t, os.WriteFile(aPath, []byte("version: '3'\n"), 0644))
	require.NoError(t, os.WriteFile(bPath, []byte("version: '3'\n"), 0644))

	// a (level 0) starts OK; b (level 1, depends on a, Required) FAILS its Up.
	orch := &depComposeOrch{upErr: map[string]error{"b.yml": fmt.Errorf("boom")}}
	o := New(WithLocalOrchestrator(orch), WithProjectDir(tmpDir))
	o.AddService(Service{Name: "a", ComposeFile: aPath})
	o.AddService(Service{Name: "b", ComposeFile: bPath, Dependencies: []string{"a"}, Required: true})

	err := o.StartAll(context.Background())
	require.Error(t, err, "a required later-wave service failing must fail StartAll")

	assert.Contains(t, orch.downFiles(), "a.yml",
		"a service started in an earlier wave must be rolled back when a later wave's required service fails")
	assert.Contains(t, orch.upFiles(), "a.yml", "the earlier-wave service did start (so it can be rolled back)")
}

// TestComputeStartLevels covers the pure ordering function directly: dependency
// waves, no-deps single level, duplicate-name prerequisites, unknown dep, cycle,
// and self-dependency (a one-node cycle).
func TestComputeStartLevels(t *testing.T) {
	names := func(level []Service) []string {
		out := make([]string, len(level))
		for i, s := range level {
			out[i] = s.Name
		}
		return out
	}

	t.Run("no dependencies collapse to one level", func(t *testing.T) {
		levels, err := computeStartLevels([]Service{{Name: "a"}, {Name: "b"}, {Name: "c"}})
		require.NoError(t, err)
		require.Len(t, levels, 1)
		assert.ElementsMatch(t, []string{"a", "b", "c"}, names(levels[0]))
	})

	t.Run("dependency waves order deps before dependents", func(t *testing.T) {
		levels, err := computeStartLevels([]Service{
			{Name: "app", Dependencies: []string{"db", "cache"}},
			{Name: "db"},
			{Name: "cache", Dependencies: []string{"db"}},
		})
		require.NoError(t, err)
		require.Len(t, levels, 3)
		assert.Equal(t, []string{"db"}, names(levels[0]))
		assert.Equal(t, []string{"cache"}, names(levels[1]))
		assert.Equal(t, []string{"app"}, names(levels[2]))
	})

	t.Run("duplicate dependency name waits for all", func(t *testing.T) {
		levels, err := computeStartLevels([]Service{
			{Name: "worker", Dependencies: []string{"db"}},
			{Name: "db", ComposeFile: "db1.yml"},
			{Name: "db", ComposeFile: "db2.yml"},
		})
		require.NoError(t, err)
		require.Len(t, levels, 2)
		assert.ElementsMatch(t, []string{"db", "db"}, names(levels[0]))
		assert.Equal(t, []string{"worker"}, names(levels[1]))
	})

	t.Run("unknown dependency errors", func(t *testing.T) {
		_, err := computeStartLevels([]Service{{Name: "a", Dependencies: []string{"ghost"}}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
	})

	t.Run("cycle errors", func(t *testing.T) {
		_, err := computeStartLevels([]Service{
			{Name: "a", Dependencies: []string{"b"}},
			{Name: "b", Dependencies: []string{"a"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
	})

	t.Run("self-dependency is a one-node cycle", func(t *testing.T) {
		_, err := computeStartLevels([]Service{{Name: "a", Dependencies: []string{"a"}}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
	})
}
