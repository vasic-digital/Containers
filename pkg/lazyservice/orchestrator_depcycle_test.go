// CT-HARDEN-70 — dependency-cycle unbounded-recursion reproduction.
//
// StartService resolves a service's declared Dependencies by recursing into
// StartService for each dependency BEFORE the LazyBooter once-guard is ever
// engaged. With NO cycle detection / visited-set / depth bound, a cyclic
// (A→B→A) or self-referential (A→A) Dependencies graph recurses forever →
// goroutine stack exceeds its limit → `fatal error: stack overflow` → the
// WHOLE process crashes (a Go stack overflow is fatal, NOT recoverable via
// recover()).
//
// Because the crash kills the whole test process, the reproduction runs in an
// isolated SUBPROCESS (the classic TestHelperProcess pattern): the worker test
// is inert unless LAZYSVC_CYCLE_WORKER=1, and the parent test re-execs the test
// binary with that env set, then asserts the CONTAINED behaviour the orchestrator
// must exhibit — a descriptive cycle error, no stack-overflow crash.
//
// Polarity: RED on the UNFIXED orchestrator (worker crashes with a stack
// overflow → parent Fatalf), GREEN on the FIXED orchestrator (worker exits 0
// having surfaced a cycle error). Same source, driven purely by the code under
// test (§11.4.115 RED-on-broken-artifact → GREEN-on-fixed-artifact).
package lazyservice

import (
	"context"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

const (
	cycleWorkerEnv = "LAZYSVC_CYCLE_WORKER"
	cycleKindEnv   = "LAZYSVC_CYCLE_KIND"
)

// TestLazyOrchestrator_CycleWorker is the isolated subprocess worker. It is a
// no-op unless LAZYSVC_CYCLE_WORKER=1, so it never crashes the normal suite.
// When active it registers a dependency cycle, drives StartService, and reports
// the contained outcome (or dies with a stack overflow on the unfixed code).
func TestLazyOrchestrator_CycleWorker(t *testing.T) {
	if os.Getenv(cycleWorkerEnv) != "1" {
		t.Skip("subprocess worker; inert unless " + cycleWorkerEnv + "=1")
	}

	// Bound the per-goroutine stack so the (unfixed) unbounded recursion
	// overflows quickly with low memory instead of climbing to the 1 GiB
	// default — fast RED and host-memory-safe (constitution §12 ceiling).
	debug.SetMaxStack(16 * 1024 * 1024)

	fo := newFakeOrchestrator()
	lo := newTestOrchestrator(t, fo, &fakeHealthChecker{})

	switch os.Getenv(cycleKindEnv) {
	case "self":
		// A depends on itself.
		mustRegisterDep(t, lo, "a", "a.yml", "a")
	default:
		// Two-node cycle: A → B → A.
		mustRegisterDep(t, lo, "a", "a.yml", "b")
		mustRegisterDep(t, lo, "b", "b.yml", "a")
	}

	// UNFIXED: recurses forever → the process dies HERE (never reaching the
	// writes below). FIXED: returns a cycle error promptly.
	err := lo.StartService(context.Background(), "a")

	switch {
	case err == nil:
		_, _ = os.Stdout.WriteString("WORKER_RESULT: NO_ERROR_NO_CRASH\n")
		os.Exit(3)
	case !strings.Contains(strings.ToLower(err.Error()), "cycle"):
		_, _ = os.Stdout.WriteString("WORKER_RESULT: NON_CYCLE_ERROR: " + err.Error() + "\n")
		os.Exit(4)
	default:
		_, _ = os.Stdout.WriteString("WORKER_RESULT: CYCLE_DETECTED: " + err.Error() + "\n")
		os.Exit(0)
	}
}

func mustRegisterDep(t *testing.T, lo *LazyOrchestrator, name, file, dep string) {
	t.Helper()
	if err := lo.RegisterService(&ServiceDefinition{
		Name: name, ComposeFile: file, Dependencies: []string{dep},
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// runCycleWorker re-execs THIS test binary running only the worker, with the
// activation env set, and returns its combined output + exit error.
func runCycleWorker(t *testing.T, kind string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestLazyOrchestrator_CycleWorker$", "-test.v")
	cmd.Env = append(os.Environ(), cycleWorkerEnv+"=1", cycleKindEnv+"="+kind)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestLazyOrchestrator_StartService_DependencyCycle_DoesNotStackOverflow drives
// the worker for a two-node cycle AND a self-cycle and asserts neither crashes
// with a stack overflow (RED on unfixed) and both surface a cycle error (GREEN
// on fixed).
func TestLazyOrchestrator_StartService_DependencyCycle_DoesNotStackOverflow(t *testing.T) {
	for _, kind := range []string{"two", "self"} {
		t.Run(kind, func(t *testing.T) {
			out, err := runCycleWorker(t, kind)
			low := strings.ToLower(out)

			// RED: unbounded recursion crashes the worker with a stack
			// overflow. On the unfixed orchestrator this branch fires.
			if strings.Contains(low, "stack overflow") ||
				strings.Contains(low, "goroutine stack exceeds") {
				t.Fatalf("REGRESSION (CT-HARDEN-70): cyclic Dependencies caused a "+
					"stack-overflow crash — StartService recurses through dependencies "+
					"with no cycle detection.\n--- worker output ---\n%s", out)
			}

			// GREEN: the worker exited cleanly having surfaced a cycle error.
			if err != nil {
				t.Fatalf("worker did not exit cleanly for kind=%s (exit err: %v)\n"+
					"--- worker output ---\n%s", kind, err, out)
			}
			if !strings.Contains(out, "WORKER_RESULT: CYCLE_DETECTED") {
				t.Fatalf("expected a descriptive cycle error to be surfaced for kind=%s\n"+
					"--- worker output ---\n%s", kind, out)
			}
		})
	}
}
