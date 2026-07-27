package compose

// Wave-20 batch CT-HARDEN-COMP-HARD guard tests (§11.4.115 GREEN-polarity
// committed guards; committed default = GUARD). Each guard proves a hardening
// fix's LOGIC via an injected seam (§11.4.107 honest boundary): a
// caller-supplied cap>len composeArgs slice for the append-aliasing race, and
// a temp "hung binary" stand-in for the timeout-bounding probes. No real
// docker/compose is exercised (§11.4.27); the guards drive the package's own
// runner/factory/exec seams directly.
//
// Surgical-revert RED evidence (recorded in the batch report):
//   COMP-1 Logs   : revert the Logs defensive copy -> TestComp1_Logs* FAILs.
//   COMP-1 run/out: revert the run+output defensive copies -> the concurrent
//                   guard FAILs under `go test -race` (DATA RACE).
//   COMP-2        : revert the WithDeadline in waitForServices -> the hung-ps
//                   guard FAILs (blocks past the timeout).
//   COMP-3        : revert composeCmdWorks to exec.Command (unbounded) -> the
//                   hung-version guard FAILs (blocks past the timeout).

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- capturing Cmd seam for the deterministic COMP-1 Logs guard ---

// capturingCmdFactory records the exact argv slice header passed to
// CommandContext WITHOUT copying it, so a subsequent aliasing append into the
// same backing array is observable.
type capturingCmdFactory struct {
	mu       sync.Mutex
	lastArgs []string
}

func (f *capturingCmdFactory) CommandContext(
	_ context.Context, _ string, args ...string,
) Cmd {
	f.mu.Lock()
	f.lastArgs = args // header stored as-is (no copy) — aliasing must show through
	f.mu.Unlock()
	return &capturingCmd{}
}

type capturingCmd struct{}

func (c *capturingCmd) SetDir(string) {}
func (c *capturingCmd) Start() error  { return nil }
func (c *capturingCmd) Wait() error   { return nil }
func (c *capturingCmd) StdoutPipe() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestComp1_LogsDefensiveCopy_NoAliasCorruption proves the Logs site
// defensively COPIES o.composeArgs instead of append-aliasing its backing
// array. Seam: a cap>len composeArgs makes pre-fix append() reuse the backing
// array; a 2nd Logs call then overwrites the 1st call's captured argv tail.
// Deterministic (no -race required).
func TestComp1_LogsDefensiveCopy_NoAliasCorruption(t *testing.T) {
	base := make([]string, 1, 16) // len 1, cap 16 -> pre-fix append stays in place
	base[0] = "compose"

	factory := &capturingCmdFactory{}
	o := NewOrchestratorWithFactory("mock", base, "/tmp", nil, factory)
	project := ComposeProject{File: "f.yaml", Name: "proj"}

	r1, err := o.Logs(context.Background(), project, "svcA")
	require.NoError(t, err)
	_ = r1.Close()

	firstArgs := factory.lastArgs
	require.NotEmpty(t, firstArgs)
	lastIdx := len(firstArgs) - 1
	require.Equal(t, "svcA", firstArgs[lastIdx],
		"first Logs call must build the svcA argv")

	// Second call. Pre-fix this reuses base's backing array and clobbers
	// firstArgs' tail (svcA -> svcB); post-fix firstArgs owns its backing and
	// stays stable.
	r2, err := o.Logs(context.Background(), project, "svcB")
	require.NoError(t, err)
	_ = r2.Close()

	assert.Equal(t, "svcA", firstArgs[lastIdx],
		"COMP-1: first Logs call's argv was corrupted by the second call "+
			"(shared-backing append aliasing) — defensive copy missing")
}

// TestComp1_RunOutputConcurrent_NoDataRace proves run() (via Up) and output()
// (via Status) defensively copy o.composeArgs so concurrent calls do not write
// into a shared backing array. A cap>len composeArgs makes the pre-fix
// append() reuse the backing array; concurrent Up/Status then race on it,
// which fires under `go test -race`. `true` (ignores args, exits 0) is a
// harmless real command — no docker/compose (§11.4.27). Post-fix each call
// owns its argv backing -> race-free.
func TestComp1_RunOutputConcurrent_NoDataRace(t *testing.T) {
	base := make([]string, 1, 16) // cap>len -> pre-fix append aliases backing
	base[0] = "ignored"           // 'true' ignores all operands
	o := NewOrchestrator("true", base, "/tmp", nil)
	project := ComposeProject{File: "f.yaml", Name: "proj"}

	const goroutines = 8
	const iters = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			<-start // bunch all goroutines onto the raced append window
			for i := 0; i < iters; i++ {
				_ = o.Up(context.Background(), project)        // -> run()
				_, _ = o.Status(context.Background(), project) // -> output()
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestComp2_WaitForServices_BoundedByTimeout proves waitForServices bounds a
// single hung Status/ps probe by `timeout` via a deadline-derived context.
// Seam: a temp script stands in for a wedged `ps` (sleeps, ignoring args);
// Status -> output -> exec.CommandContext(ctx) must be killed at the deadline.
// Pre-fix (no WithDeadline; ctx=Background has no deadline) the probe blocks
// for the full script sleep, ignoring `timeout`. Honest boundary (§11.4.107):
// the real waitForServices+Status+output+exec path runs; only the external
// `ps` binary is a stand-in (§11.4.27).
func TestComp2_WaitForServices_BoundedByTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "hung-ps.sh")
	require.NoError(t, os.WriteFile(scriptPath,
		[]byte("#!/bin/sh\nexec sleep 20\n"), 0o755))

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)
	project := ComposeProject{File: "f.yaml", Name: "proj"}

	const timeoutSecs = 1
	done := make(chan error, 1)
	go func() {
		done <- o.waitForServices(context.Background(), project, timeoutSecs)
	}()

	select {
	case err := <-done:
		// Bounded: returned within the guard window with an error (services
		// never became ready) — not a false success.
		require.Error(t, err)
	case <-time.After(6 * time.Second):
		t.Fatalf("COMP-2: waitForServices did not return within 6s for a "+
			"%ds timeout — a hung ps probe blocked past the timeout "+
			"(deadline-bounded context missing)", timeoutSecs)
	}
}

// TestComp3_ComposeCmdWorks_BoundedByTimeout proves the compose-command
// detection probe is bounded: composeCmdWorks(name, args, timeout) must kill a
// wedged client binary at `timeout` (exec.CommandContext) so detectComposeCmd
// / NewDefaultOrchestrator cannot block indefinitely. Seam: a temp script that
// ignores args and hangs stands in for a wedged `<cmd> version` (§11.4.27, no
// real docker/podman). Pre-fix (exec.Command, unbounded) the probe blocks for
// the full script sleep; post-fix it returns ~timeout with false.
func TestComp3_ComposeCmdWorks_BoundedByTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "hung-version.sh")
	require.NoError(t, os.WriteFile(scriptPath,
		[]byte("#!/bin/sh\nexec sleep 20\n"), 0o755))

	startedAt := time.Now()
	done := make(chan bool, 1)
	go func() {
		done <- composeCmdWorks(scriptPath, nil, 500*time.Millisecond)
	}()

	select {
	case ok := <-done:
		assert.False(t, ok,
			"COMP-3: a hung version probe must not report success")
		assert.Less(t, time.Since(startedAt), 4*time.Second,
			"COMP-3: probe returned but took too long — timeout not applied")
	case <-time.After(4 * time.Second):
		t.Fatalf("COMP-3: composeCmdWorks did not return within 4s for a " +
			"500ms timeout — a wedged binary blocked detection (unbounded probe)")
	}
}
