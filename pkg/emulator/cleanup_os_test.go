// cleanup_os_test.go — tests for the per-OS procWalker dispatch and the
// macOS/POSIX `ps`-based walker (the load-bearing §6.AH-debt path).
//
// Anti-bluff posture (§6.J/§6.L + §6.N containers variant): every test
// here asserts on observable parsing behaviour or the concrete walker
// type selected for an OS — never on internal state alone. The
// FALSIFIABILITY REHEARSAL for this file is recorded in the commit body
// (Bluff-Audit stamp): the documented mutations make the named tests
// FAIL, proving they catch real defects in the production dispatch +
// parsing paths rather than passing vacuously.
package emulator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePsExecutor is the test seam for psWalker. It returns canned `ps`
// output depending on whether the invocation asked for the `comm`
// column (PidComms) or the `args` column (PidCmdlines), or a fixed
// error to exercise the error-propagation path.
type fakePsExecutor struct {
	commOutput string
	argsOutput string
	err        error
	// calls records each (name, args) invocation so a test can assert
	// the walker shelled out to `ps` with the expected column flags.
	calls [][]string
}

func (f *fakePsExecutor) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	for _, a := range args {
		if a == "pid,args" {
			return []byte(f.argsOutput), nil
		}
	}
	return []byte(f.commOutput), nil
}

// TestNewOSProcWalker_DispatchTable is the falsifiability-rehearsed gate
// for the per-OS dispatch. Changing the "linux" branch to return
// psWalker (or the default branch to return procFSWalker) flips the
// asserted concrete type and fails this test.
func TestNewOSProcWalker_DispatchTable(t *testing.T) {
	t.Run("linux selects procFSWalker", func(t *testing.T) {
		w := newOSProcWalker("linux")
		_, ok := w.(procFSWalker)
		assert.True(t, ok, "linux MUST select procFSWalker (real /proc walk); got %T", w)
	})

	t.Run("darwin selects psWalker", func(t *testing.T) {
		w := newOSProcWalker("darwin")
		_, ok := w.(psWalker)
		assert.True(t, ok, "darwin MUST select psWalker (ps -A) — a Linux /proc walk fails on macOS; got %T", w)
	})

	t.Run("other POSIX OSes select psWalker", func(t *testing.T) {
		for _, goos := range []string{"freebsd", "openbsd", "netbsd", "windows", "solaris"} {
			w := newOSProcWalker(goos)
			_, ok := w.(psWalker)
			assert.True(t, ok, "%s MUST fall back to psWalker; got %T", goos, w)
		}
	})

	t.Run("darwin psWalker is backed by the real os executor", func(t *testing.T) {
		w, ok := newOSProcWalker("darwin").(psWalker)
		require.True(t, ok)
		_, isOSExec := w.exec.(osExecutor)
		assert.True(t, isOSExec, "production psWalker MUST use the real osExecutor, not a nil/fake seam; got %T", w.exec)
	})
}

func TestPsWalker_PidComms_ParsesPsOutput(t *testing.T) {
	// Realistic `ps -A -o pid,comm` output: a header line plus three
	// processes, including the qemu-system process Cleanup targets.
	fake := &fakePsExecutor{commOutput: "" +
		"  PID COMM\n" +
		"    1 launchd\n" +
		"  4242 qemu-system-aarch64\n" +
		" 9001 adb\n",
	}
	w := psWalker{exec: fake}

	comms, err := w.PidComms()
	require.NoError(t, err)

	// Primary assertion: the qemu PID maps to its comm so the Cleanup
	// prefix matcher can find it. This is the user-visible behaviour
	// (orphan emulator reaping) the walker exists to enable.
	assert.Equal(t, "qemu-system-aarch64", comms[4242])
	assert.Equal(t, "launchd", comms[1])
	assert.Equal(t, "adb", comms[9001])
	// Header line ("PID COMM") MUST NOT become a pseudo-process.
	assert.Len(t, comms, 3, "header line must be filtered, not parsed as a PID")

	// Secondary assertion: the walker asked `ps` for the comm column.
	require.Len(t, fake.calls, 1)
	assert.Contains(t, fake.calls[0], "pid,comm")
}

func TestPsWalker_PidCmdlines_SplitsArgvTokens(t *testing.T) {
	// `ps -Aww -o pid,args` output: full command lines, space-delimited.
	fake := &fakePsExecutor{argsOutput: "" +
		"  PID ARGS\n" +
		"  4242 qemu-system-aarch64 -port 5554 -no-window\n" +
		" 7000 /usr/bin/adb fork-server server\n" +
		" 8000 soloproc\n",
	}
	w := psWalker{exec: fake}

	cmdlines, err := w.PidCmdlines()
	require.NoError(t, err)

	// Primary assertion: argv decomposes into adjacent tokens so
	// KillByPort's strict "-port" + "5554" adjacent-token matcher works
	// identically to the Linux /proc/<pid>/cmdline NUL-split.
	assert.Equal(t, []string{"qemu-system-aarch64", "-port", "5554", "-no-window"}, cmdlines[4242])
	assert.Equal(t, []string{"/usr/bin/adb", "fork-server", "server"}, cmdlines[7000])
	// A PID with a single-token command line still yields its argv.
	assert.Equal(t, []string{"soloproc"}, cmdlines[8000])
	assert.Len(t, cmdlines, 3, "header line must be filtered, not parsed as a PID")

	require.Len(t, fake.calls, 1)
	assert.Contains(t, fake.calls[0], "pid,args")
}

func TestPsWalker_PropagatesExecError(t *testing.T) {
	wantErr := errors.New("ps: command not found")
	w := psWalker{exec: &fakePsExecutor{err: wantErr}}

	_, commErr := w.PidComms()
	require.Error(t, commErr)
	assert.ErrorIs(t, commErr, wantErr, "PidComms MUST surface the executor error, not swallow it")

	_, cmdErr := w.PidCmdlines()
	require.Error(t, cmdErr)
	assert.ErrorIs(t, cmdErr, wantErr, "PidCmdlines MUST surface the executor error, not swallow it")
}
