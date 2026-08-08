package lifecycle_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delayOrchestrator is a stubOrchestrator variant whose Up() call sleeps
// for a configurable duration before returning, widening the window
// between DefaultManager.Start()'s "not yet running" check and its
// state="starting"/idleCtrl-assignment steps so concurrency hazards in
// that window become reliably observable under test (constitution/
// Constitution.md §11.4.115 — deterministic reproduction, not a hopeful
// real-clock sleep-and-guess).
type delayOrchestrator struct {
	upDelay    time.Duration
	upCalled   atomicCounter
	downCalled atomicCounter
}

func (d *delayOrchestrator) Up(
	_ context.Context, _ compose.ComposeProject, _ ...compose.UpOption,
) error {
	d.upCalled.add(1)
	if d.upDelay > 0 {
		time.Sleep(d.upDelay)
	}
	return nil
}

func (d *delayOrchestrator) Down(
	_ context.Context, _ compose.ComposeProject, _ ...compose.DownOption,
) error {
	d.downCalled.add(1)
	return nil
}

func (d *delayOrchestrator) Status(
	_ context.Context, _ compose.ComposeProject,
) ([]compose.ServiceStatus, error) {
	return nil, nil
}

func (d *delayOrchestrator) Logs(
	_ context.Context, _ compose.ComposeProject, _ string,
) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}

var _ compose.ComposeOrchestrator = (*delayOrchestrator)(nil)

// atomicCounter is a tiny race-free counter (avoids pulling in
// sync/atomic boilerplate at every call site above).
type atomicCounter struct {
	mu  sync.Mutex
	val int
}

func (c *atomicCounter) add(n int) {
	c.mu.Lock()
	c.val += n
	c.mu.Unlock()
}

func (c *atomicCounter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// TestDefaultManager_IdleCtrl_ConcurrentFieldAccessRace reproduces a data
// race on serviceEntry.idleCtrl: DefaultManager.Start() writes the field
// while holding m.mu, but Stop()/Acquire()/the Acquire release-closure
// read it WITHOUT holding m.mu. Run with -race: this test must trip the
// race detector on the pre-fix tree (constitution/Constitution.md
// §11.4.108 — sibling-primitive audit of the just-fixed idle.go
// stale-fire race per §11.4.102/§11.4.118).
//
// The test does not assert on outcome values (the race is the defect,
// not a wrong return value) — a clean `go test -race` run IS the proof
// once the fix lands; a race report on the unfixed tree is the RED
// baseline.
func TestDefaultManager_IdleCtrl_ConcurrentFieldAccessRace(t *testing.T) {
	orch := &delayOrchestrator{upDelay: 3 * time.Millisecond}
	hc := &stubHealthChecker{healthy: true}
	m := lifecycle.NewDefaultManager(orch, hc)

	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name:        "svc",
		ComposeFile: "c.yml",
		IdleTimeout: time.Hour, // long enough to never actually fire
	}))

	ctx := context.Background()
	var wg sync.WaitGroup
	stopSignal := make(chan struct{})

	// Writer goroutines: cycle Start/Stop repeatedly. Each Start() that
	// finds the service not-running writes entry.idleCtrl (under m.mu,
	// per current code) after unlocking from the "running" check.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopSignal:
					return
				default:
				}
				_ = m.Start(ctx, "svc")
				_ = m.Stop(ctx, "svc")
			}
		}()
	}

	// Reader goroutines: hammer Stop()/Acquire() concurrently. Both read
	// entry.idleCtrl without m.mu on the pre-fix tree.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopSignal:
					return
				default:
				}
				_ = m.Stop(ctx, "svc")
				if release, err := m.Acquire(ctx, "svc"); err == nil {
					release()
				}
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stopSignal)
	wg.Wait()
}

// TestDefaultManager_Start_ConcurrentCallsDoNotDoubleBoot reproduces a
// TOCTOU race in DefaultManager.Start(): the "already running" check and
// the state="starting" transition are two separate lock acquisitions
// with the lock released in between (to let dependency recursion run
// unlocked). Two concurrent Start() calls that both observe the service
// as not-yet-running can both pass the check and both execute the full
// boot sequence — double-invoking the orchestrator's Up() and (per the
// spec under test) creating two IdleShutdown controllers, one of which
// is silently orphaned (never Stop()'d, its timer still ticking in the
// background). Deterministic via delayOrchestrator widening the race
// window plus a released start barrier (constitution/Constitution.md
// §11.4.115/§11.4.50 — proven with N concurrent callers, not a single
// hopeful sleep).
func TestDefaultManager_Start_ConcurrentCallsDoNotDoubleBoot(t *testing.T) {
	orch := &delayOrchestrator{upDelay: 20 * time.Millisecond}
	hc := &stubHealthChecker{healthy: true}
	m := lifecycle.NewDefaultManager(orch, hc)

	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name:        "svc",
		ComposeFile: "c.yml",
	}))

	ctx := context.Background()
	const callers = 10
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	errs := make([]error, callers)

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			errs[idx] = m.Start(ctx, "svc")
		}(i)
	}
	close(startBarrier)
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "caller %d", i)
	}

	// The defect under test: concurrent Start() calls race past the
	// not-yet-running check and each invoke orchestrator.Up() once,
	// instead of exactly one caller performing the real boot and the
	// rest observing "already running"/"already starting" and skipping
	// the duplicate work.
	assert.Equal(t, 1, orch.upCalled.load(),
		"orchestrator.Up() must be invoked exactly once even under "+
			"concurrent Start() callers; a value >1 proves the "+
			"double-boot TOCTOU")

	status, err := m.Status("svc")
	require.NoError(t, err)
	assert.Equal(t, "running", status.State)
}
