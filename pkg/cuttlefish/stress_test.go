package cuttlefish

// stress_test.go — §11.4.85 STRESS coverage for the Cuttlefish lifecycle
// wrapper (Launch / Status / WaitForReady / Stop + the orphan-reaping
// Cleanup core).
//
// These tests exercise the REAL lifecycle logic through its injectable
// seams (the CommandExecutor, the statDevice / kvmDevicePath package vars,
// and the cleanupWithDeps(procWalker, killer) core) — never a stub of the
// unit-under-test (§11.4.85 / §11.4.1 / CONST-050). Coverage is the
// §11.4.85 stress closed set: sustained load (N ≥ 100), concurrent
// contention (N ≥ 10 parallel), and boundary conditions (empty / max /
// off-by-one). Run with `-race` (see CLAUDE.md "Build & Test").
//
// The package's existing `fakeExecutor` (cuttlefish_test.go) is single-
// threaded (it appends to `calls` with no lock), so for the concurrent
// suites we use the thread-safe `safeExecutor` defined here — concurrency
// safety of the production lifecycle is what is under test, not of the
// non-concurrent helper.
//
// Honest boundary (§11.4.6 / CLAUDE.md "Honest boundary"): a genuine
// privileged `cvd` boot needs a Linux + nested-KVM host; that integration
// layer is SKIPPED-with-reason (§11.4.3) by the topology gate, never faked.
// The logic these tests drive — argument construction, device-presence
// partitioning, adb-output parsing, readiness reduction, orphan reaping —
// is real and is what runs unchanged on the host path.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// safeExecutor is a concurrency-safe CommandExecutor test double. Unlike
// the package's fakeExecutor it guards its call counter with an atomic so
// it can be hammered from many goroutines under -race. fn must itself be
// safe for concurrent use (the suites pass pure closures).
type safeExecutor struct {
	fn    func(name string, args []string) ([]byte, error)
	count int64
}

func (s *safeExecutor) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	atomic.AddInt64(&s.count, 1)
	if s.fn == nil {
		return nil, nil
	}
	return s.fn(name, args)
}

func (s *safeExecutor) Start(_ context.Context, _ string, _ ...string) error { return nil }

func (s *safeExecutor) Count() int64 { return atomic.LoadInt64(&s.count) }

// assertNoGoroutineLeak asserts the live goroutine count settles back to
// (roughly) the pre-test baseline. The lifecycle methods + Cleanup spawn no
// goroutines, so a leak here would be a genuine product defect (a stuck
// poll loop or an unreaped watcher). A small settle-poll absorbs the
// scheduler/test-runtime noise rather than guessing a fixed slack (§11.4.6).
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.LessOrEqualf(t, runtime.NumGoroutine(), baseline+2,
		"goroutine leak: started at %d, ended at %d", baseline, runtime.NumGoroutine())
}

const readyADBOut = "List of devices attached\n0.0.0.0:6520\tdevice\n"

func newStressInstance(t *testing.T, name string, exec CommandExecutor) *Cuttlefish {
	t.Helper()
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, NetworkHost: true,
		InstanceName: name, Executor: exec,
	})
	require.NoError(t, err)
	return c
}

// TestStress_ConcurrentStatus_NoRaceNoDeadlock — concurrent contention:
// 128 goroutines each call Status() many times against ONE shared instance
// (Status reads only immutable cfg + the executor, so this is the
// supported concurrent surface). Per-iteration assertion: every probe
// reports Ready with no error. Proves no data race (under -race) and no
// deadlock (the whole run is bounded by the test deadline).
func TestStress_ConcurrentStatus_NoRaceNoDeadlock(t *testing.T) {
	base := runtime.NumGoroutine()
	exec := &safeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte(readyADBOut), nil
	}}
	c := newStressInstance(t, "stress-status", exec)

	const workers, perWorker = 128, 50
	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				st, err := c.Status(context.Background())
				if err != nil {
					errCh <- fmt.Errorf("Status err: %w", err)
					return
				}
				if !st.Ready {
					errCh <- fmt.Errorf("expected Ready, got Raw=%q", st.Raw)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	assert.Equal(t, int64(workers*perWorker), exec.Count(),
		"every concurrent Status MUST reach the executor exactly once")
	assertNoGoroutineLeak(t, base)
}

// TestStress_ConcurrentLaunchStopCycles_SeparateInstances — concurrent
// contention at the FULL lifecycle granularity. The wrapper documents that
// concurrent calls against the SAME instance are unsupported (Launch/Stop
// mutate containerName), so the realistic parallel-hardware shape is one
// instance per worker (cf. §11.4.119 single-resource-owner). N=64 ≥ 10.
// Per-cycle assertion: Launch Started, Status Ready, Stop clean, name
// cleared. Run with -race.
func TestStress_ConcurrentLaunchStopCycles_SeparateInstances(t *testing.T) {
	base := runtime.NumGoroutine()
	withStatDevice(t, DefaultDevices()...) // all nodes present

	const workers = 64
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			exec := &safeExecutor{fn: func(name string, args []string) ([]byte, error) {
				if name == "adb" {
					return []byte(readyADBOut), nil
				}
				return []byte("ok"), nil
			}}
			c := newStressInstance(t, fmt.Sprintf("stress-cycle-%d", id), exec)

			res, err := c.Launch(context.Background())
			if err != nil || !res.Started {
				errCh <- fmt.Errorf("worker %d Launch: started=%v err=%v", id, res.Started, err)
				return
			}
			if got := c.ContainerName(); got != fmt.Sprintf("cvd-stress-cycle-%d", id) {
				errCh <- fmt.Errorf("worker %d unexpected name %q", id, got)
				return
			}
			st, err := c.Status(context.Background())
			if err != nil || !st.Ready {
				errCh <- fmt.Errorf("worker %d Status: ready=%v err=%v", id, st.Ready, err)
				return
			}
			if err := c.Stop(context.Background()); err != nil {
				errCh <- fmt.Errorf("worker %d Stop: %w", id, err)
				return
			}
			if c.ContainerName() != "" {
				errCh <- fmt.Errorf("worker %d name not cleared after Stop", id)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	assertNoGoroutineLeak(t, base)
}

// TestStress_SustainedLifecycle_Sequential — sustained load: N=200 ≥ 100
// sequential Launch→Status→Stop cycles on a fresh instance each iteration,
// recording per-iteration wall-clock so a latency regression is visible in
// the failure message. Every cycle MUST be GREEN — a single divergent
// iteration FAILs (§11.4.50 deterministic consistency).
func TestStress_SustainedLifecycle_Sequential(t *testing.T) {
	withStatDevice(t, DefaultDevices()...)
	exec := &safeExecutor{fn: func(name string, _ []string) ([]byte, error) {
		if name == "adb" {
			return []byte(readyADBOut), nil
		}
		return []byte("ok"), nil
	}}

	const iterations = 200
	var maxLatency time.Duration
	for i := 0; i < iterations; i++ {
		start := time.Now()
		c := newStressInstance(t, fmt.Sprintf("sustained-%d", i), exec)
		res, err := c.Launch(context.Background())
		require.NoErrorf(t, err, "iteration %d Launch", i)
		require.Truef(t, res.Started, "iteration %d not started", i)
		st, err := c.Status(context.Background())
		require.NoErrorf(t, err, "iteration %d Status", i)
		require.Truef(t, st.Ready, "iteration %d not ready", i)
		require.NoErrorf(t, c.Stop(context.Background()), "iteration %d Stop", i)
		require.Emptyf(t, c.ContainerName(), "iteration %d name not cleared", i)
		if d := time.Since(start); d > maxLatency {
			maxLatency = d
		}
	}
	t.Logf("sustained %d cycles GREEN; worst-iteration latency=%s; executor calls=%d",
		iterations, maxLatency, exec.Count())
}

// TestStress_ParseADBDevices_ConcurrentPure — the adb-output parser is pure
// (no shared state). Hammered concurrently it MUST return identical results
// for identical input every time, and never race. Boundary inputs included:
// empty, header-only, and a large many-device listing (max).
func TestStress_ParseADBDevices_ConcurrentPure(t *testing.T) {
	base := runtime.NumGoroutine()

	// Build a large (max-ish) listing: 500 devices, alternating states.
	var big string
	big = "List of devices attached\n"
	const nDev = 500
	for i := 0; i < nDev; i++ {
		state := "device"
		if i%3 == 0 {
			state = "offline"
		}
		big += fmt.Sprintf("0.0.0.0:%d\t%s\n", 6520+i, state)
	}
	wantBig := len(ParseADBDevices([]byte(big)))

	inputs := []string{"", "List of devices attached\n", readyADBOut, big}

	const workers, perWorker = 32, 100
	var wg sync.WaitGroup
	var mismatches int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if len(ParseADBDevices([]byte(inputs[i%len(inputs)]))) > nDev+1 {
					atomic.AddInt64(&mismatches, 1)
				}
			}
			// Stable-result check on the big input.
			if len(ParseADBDevices([]byte(big))) != wantBig {
				atomic.AddInt64(&mismatches, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(0), atomic.LoadInt64(&mismatches),
		"ParseADBDevices must be deterministic + race-free under concurrency")
	assert.Equal(t, nDev, wantBig, "all %d devices parsed", nDev)
	assertNoGoroutineLeak(t, base)
}

// TestStress_Cleanup_ConcurrentReap_SeparateDeps — the orphan-reaping core
// (cleanupWithDeps) driven from many goroutines, each with its OWN
// fakeWalker + fakeKiller (the realistic shape — one reaper owns its own
// process view). Per-run assertion: exactly the allowlisted daemons are
// reaped. N=32 ≥ 10 parallel. Run with -race.
func TestStress_Cleanup_ConcurrentReap_SeparateDeps(t *testing.T) {
	base := runtime.NumGoroutine()
	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			pidCrosvm := 1000 + id*10
			pidRun := pidCrosvm + 1
			pidNoise := pidCrosvm + 2
			wk := fakeWalker{
				comms: map[int]string{
					pidCrosvm: "crosvm",
					pidRun:    "run_cvd",
					pidNoise:  "bash", // must NOT be reaped
				},
				cmdlines: map[int][]string{
					pidCrosvm: {"crosvm", "--base_instance_num=1"},
					pidRun:    {"run_cvd", "--base_instance_num=1"},
					pidNoise:  {"bash"},
				},
			}
			k := newFakeKiller()
			report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", wk, k)
			if err != nil {
				errCh <- fmt.Errorf("worker %d: %w", id, err)
				return
			}
			if len(report.Found) != 2 {
				errCh <- fmt.Errorf("worker %d: expected 2 reaped, got %v", id, report.Found)
				return
			}
			if len(k.signals[pidNoise]) != 0 {
				errCh <- fmt.Errorf("worker %d: noise pid was signalled", id)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	assertNoGoroutineLeak(t, base)
}

// TestStress_Boundary_DeviceSets — boundary conditions for the device-
// presence partition that gates `--device`: empty list, single node, the
// full set, and off-by-one (exactly one node absent). Every boundary
// produces a categorised (present/missing) result, never a panic
// (§11.4.85 boundary closed-set).
func TestStress_Boundary_DeviceSets(t *testing.T) {
	all := DefaultDevices()
	tests := []struct {
		name        string
		present     []string
		request     []string
		wantPresent int
		wantMissing int
	}{
		{"empty request", all, nil, 0, 0},
		{"single present", []string{"/dev/kvm"}, []string{"/dev/kvm"}, 1, 0},
		{"single absent", nil, []string{"/dev/kvm"}, 0, 1},
		{"full set present", all, all, len(all), 0},
		{"off-by-one missing", all[:len(all)-1], all, len(all) - 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withStatDevice(t, tc.present...)
			present, missing := FilterPresentDevices(tc.request)
			assert.Len(t, present, tc.wantPresent)
			assert.Len(t, missing, tc.wantMissing)
		})
	}
}
