//go:build !integration

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// The defect these tests pin down
//
// PodmanRuntime.Logs returned a reader that produced ZERO BYTES and a NIL
// ERROR when the underlying CLI had failed outright. The caller could not tell
// "the container printed nothing" from "I never managed to read the logs" —
// both looked like a healthy, quiet container.
//
// Measured on this project's own build host, podman 5.7.1, against the live
// container workshop-curriculum_platform_1:
//
//	$ podman logs --tail all workshop-curriculum_platform_1 >/dev/null
//	Error: invalid argument "all" for "--tail" flag:
//	       strconv.ParseInt: parsing "all": invalid syntax
//	rc=125, 0 bytes on stdout, 122 bytes on stderr
//
// defaultLogOptions() supplies Tail:"all", so EVERY default-option call hit
// this. Two independent faults combined: podman rejects Docker's "all"
// sentinel for its integer --tail flag, and the stream reader reported the
// resulting failure nowhere a caller would look.
//
// THE CONTRACT, and the single sentence the whole file exists to enforce:
// ZERO BYTES WITH A NIL ERROR MUST BE IMPOSSIBLE WHEN THE READ ACTUALLY
// FAILED. A container that genuinely printed nothing must still read as
// (0 bytes, nil error), or the fix would have bought its safety by making
// every quiet container look broken.
// ---------------------------------------------------------------------------

// silentFailureVerdict is the contract check, factored out of the tests so the
// SAME predicate can be pointed at the real reader (which must satisfy it) and
// at a deliberately broken one (which must not). That is what makes the
// mutation below a real paired proof rather than a second hand-written
// assertion that could drift from the first.
//
// cmdFailed states whether the underlying command actually failed. The
// predicate returns "" when the reader honours the contract, or a description
// of the violation when it does not.
func silentFailureVerdict(rc io.ReadCloser, cmdFailed bool) string {
	data, readErr := io.ReadAll(rc)
	closeErr := rc.Close()

	if cmdFailed {
		// The failure must be visible from the READ path. Close alone is not
		// enough: `defer rc.Close()` discards it, which is the idiom that hid
		// this defect in the first place.
		if readErr == nil {
			return fmt.Sprintf(
				"SILENT FAILURE: command failed but io.ReadAll returned "+
					"%d byte(s) and a nil error (Close said %v)",
				len(data), closeErr,
			)
		}
		return ""
	}

	// The command succeeded: a quiet container must stay quiet, not become an
	// error.
	if readErr != nil {
		return fmt.Sprintf(
			"FALSE ALARM: command succeeded but read returned error %v", readErr,
		)
	}
	if closeErr != nil {
		return fmt.Sprintf(
			"FALSE ALARM: command succeeded but Close returned error %v", closeErr,
		)
	}
	return ""
}

// streamCmdStub drives ExecuteStream's real ReadCloser: it decides what the
// child wrote to stdout and how the child exited.
type streamCmdStub struct {
	stdout  string
	waitErr error
}

func (s *streamCmdStub) StdoutPipe() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.stdout)), nil
}
func (s *streamCmdStub) Start() error { return nil }
func (s *streamCmdStub) Wait() error  { return s.waitErr }

type streamCmdStubFactory struct {
	cmd      *streamCmdStub
	gotArgs  []string
	gotName  string
	callSeen bool
}

func (f *streamCmdStubFactory) CommandContext(
	_ context.Context, name string, args ...string,
) StreamCmd {
	f.callSeen = true
	f.gotName = name
	f.gotArgs = args
	return f.cmd
}

// ---------------------------------------------------------------------------
// 1. The defect itself
// ---------------------------------------------------------------------------

// TestExecuteStream_FailedCommandNeverReadsAsZeroBytesNilError is the
// regression test for the recorded defect. It is the test the mutation below
// must be able to fail.
func TestExecuteStream_FailedCommandNeverReadsAsZeroBytesNilError(t *testing.T) {
	// Exactly the observed shape: nothing on stdout, non-zero exit.
	factory := &streamCmdStubFactory{cmd: &streamCmdStub{
		stdout:  "",
		waitErr: &exec.ExitError{},
	}}
	exe := &defaultExecutor{streamFactory: factory}

	rc, err := exe.ExecuteStream(context.Background(), "podman", "logs", "x")
	require.NoError(t, err, "ExecuteStream itself starts fine; the failure is later")

	verdict := silentFailureVerdict(rc, true)
	assert.Empty(t, verdict, "the reader must surface the command's failure")
}

// TestExecuteStream_QuietCommandStillSucceeds is the CONTROL.
//
// Without it, the test above could be satisfied by a reader that returns an
// error unconditionally — which would "fix" the defect by breaking every
// healthy container. This pins the other side of the distinction.
func TestExecuteStream_QuietCommandStillSucceeds(t *testing.T) {
	factory := &streamCmdStubFactory{cmd: &streamCmdStub{
		stdout:  "", // a container that genuinely printed nothing
		waitErr: nil,
	}}
	exe := &defaultExecutor{streamFactory: factory}

	rc, err := exe.ExecuteStream(context.Background(), "podman", "logs", "x")
	require.NoError(t, err)

	verdict := silentFailureVerdict(rc, false)
	assert.Empty(t, verdict,
		"a genuinely quiet container must still read as 0 bytes with no error")
}

// TestExecuteStream_RealBytesArePreserved is the second CONTROL: the fix must
// not cost content. A succeeding command's bytes arrive intact and unmodified.
func TestExecuteStream_RealBytesArePreserved(t *testing.T) {
	const payload = "UP: http://127.0.0.1:8087\n"
	factory := &streamCmdStubFactory{cmd: &streamCmdStub{stdout: payload}}
	exe := &defaultExecutor{streamFactory: factory}

	rc, err := exe.ExecuteStream(context.Background(), "podman", "logs", "x")
	require.NoError(t, err)

	data, readErr := io.ReadAll(rc)
	require.NoError(t, readErr)
	require.NoError(t, rc.Close())
	assert.Equal(t, payload, string(data))
	assert.Len(t, data, len(payload))
}

// TestExecuteStream_PartialOutputThenFailureIsStillAnError covers the nastier
// half: the command wrote something and THEN died. Truncated output that reads
// as success is the same lie in a subtler form.
func TestExecuteStream_PartialOutputThenFailureIsStillAnError(t *testing.T) {
	factory := &streamCmdStubFactory{cmd: &streamCmdStub{
		stdout:  "first line only",
		waitErr: &exec.ExitError{},
	}}
	exe := &defaultExecutor{streamFactory: factory}

	rc, err := exe.ExecuteStream(context.Background(), "podman", "logs", "x")
	require.NoError(t, err)

	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "first line only", string(data),
		"bytes that did arrive are still delivered")
	assert.Error(t, readErr,
		"truncated output must not be reported as a complete, successful read")
}

// TestExecuteStream_CloseAndReadAgreeAndWaitRunsOnce guards the memoisation:
// however Read and Close interleave, the child is reaped once and both report
// the same verdict.
func TestExecuteStream_CloseAndReadAgreeAndWaitRunsOnce(t *testing.T) {
	waitCalls := 0
	cmd := &countingWaitStub{
		streamCmdStub: streamCmdStub{stdout: "", waitErr: &exec.ExitError{}},
		calls:         &waitCalls,
	}
	exe := &defaultExecutor{streamFactory: &countingWaitFactory{cmd: cmd}}

	rc, err := exe.ExecuteStream(context.Background(), "podman", "logs", "x")
	require.NoError(t, err)

	_, readErr := io.ReadAll(rc)
	closeErr := rc.Close()

	require.Error(t, readErr)
	require.Error(t, closeErr)
	assert.Equal(t, readErr.Error(), closeErr.Error(),
		"Read and Close must report the same verdict")
	assert.Equal(t, 1, waitCalls, "the child must be reaped exactly once")
}

type countingWaitStub struct {
	streamCmdStub
	calls *int
}

func (c *countingWaitStub) Wait() error {
	*c.calls++
	return c.streamCmdStub.waitErr
}

type countingWaitFactory struct{ cmd *countingWaitStub }

func (f *countingWaitFactory) CommandContext(
	_ context.Context, _ string, _ ...string,
) StreamCmd {
	return f.cmd
}

// ---------------------------------------------------------------------------
// 2. THE PAIRED MUTATION (§1.1)
//
// A test that cannot fail asserts nothing. These prove the predicate above
// genuinely catches the regression, by pointing it at readers that reintroduce
// it. If someone weakens silentFailureVerdict — or "fixes" the reader by
// making it swallow failures again — these go red.
// ---------------------------------------------------------------------------

// silentlyEmptyReader is the MUTANT: the exact pre-fix behaviour. It reports
// clean EOF with no bytes while the command underneath it failed, and buries
// the failure in Close where `defer rc.Close()` discards it.
type silentlyEmptyReader struct{ closeErr error }

func (s *silentlyEmptyReader) Read([]byte) (int, error) { return 0, io.EOF }
func (s *silentlyEmptyReader) Close() error             { return s.closeErr }

// TestMutation_SilentZeroByteReaderIsCaught is the decisive mutation named in
// the defect report: a reader that silently returns 0 bytes. The contract
// predicate MUST reject it.
func TestMutation_SilentZeroByteReaderIsCaught(t *testing.T) {
	mutant := &silentlyEmptyReader{closeErr: errors.New("exit status 125")}

	verdict := silentFailureVerdict(mutant, true)

	require.NotEmpty(t, verdict,
		"MUTATION ESCAPED: the pre-fix reader (0 bytes, nil read error, "+
			"failure hidden in Close) was accepted — the contract check is "+
			"vacuous and the regression test above proves nothing")
	assert.Contains(t, verdict, "SILENT FAILURE")
}

// TestMutation_SilentZeroByteReaderWithNoSignalAtAllIsCaught is the worse
// mutant: not even Close reports the failure.
func TestMutation_SilentZeroByteReaderWithNoSignalAtAllIsCaught(t *testing.T) {
	mutant := &silentlyEmptyReader{closeErr: nil}

	verdict := silentFailureVerdict(mutant, true)

	require.NotEmpty(t, verdict,
		"MUTATION ESCAPED: a reader reporting success on every channel while "+
			"the command failed was accepted")
}

// TestMutation_AlwaysErroringReaderIsAlsoCaught mutates in the OPPOSITE
// direction, and is why the CONTROL exists. A reader that errors even when the
// command succeeded is not a fix; it makes every quiet container look broken.
// The predicate must reject that too, so it cannot be satisfied by simply
// always returning an error.
func TestMutation_AlwaysErroringReaderIsAlsoCaught(t *testing.T) {
	verdict := silentFailureVerdict(&alwaysErroringReader{}, false)

	require.NotEmpty(t, verdict,
		"MUTATION ESCAPED: a reader that fails even on a successful command "+
			"was accepted — the contract would be satisfiable by always "+
			"returning an error")
	assert.Contains(t, verdict, "FALSE ALARM")
}

type alwaysErroringReader struct{}

func (alwaysErroringReader) Read([]byte) (int, error) {
	return 0, errors.New("unconditional failure")
}
func (alwaysErroringReader) Close() error { return nil }

// ---------------------------------------------------------------------------
// 3. The trigger: podman's integer --tail flag
// ---------------------------------------------------------------------------

// TestPodmanLogs_NeverPassesAllToIntegerTailFlag pins the argv-level half of
// the defect. Podman's --tail is an int; "all" is Docker's sentinel and podman
// rejects it with exit 125.
func TestPodmanLogs_NeverPassesAllToIntegerTailFlag(t *testing.T) {
	tests := []struct {
		name      string
		opts      []LogOption
		wantTail  bool
		tailValue string
	}{
		{
			name:     "package default (Tail:\"all\") omits the flag entirely",
			opts:     nil,
			wantTail: false,
		},
		{
			name:      "numeric tail is passed through",
			opts:      []LogOption{WithTail("20")},
			wantTail:  true,
			tailValue: "20",
		},
		{
			name:     "explicit \"all\" is dropped, never forwarded",
			opts:     []LogOption{WithTail("all")},
			wantTail: false,
		},
		{
			name:     "any other non-numeric value is dropped too",
			opts:     []LogOption{WithTail("banana")},
			wantTail: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			exe := &mockExecutor{
				executeStreamFunc: func(
					_ context.Context, _ string, args ...string,
				) (io.ReadCloser, error) {
					gotArgs = args
					return io.NopCloser(strings.NewReader("")), nil
				},
			}
			p := NewPodmanRuntimeWithExecutor(exe)

			rc, err := p.Logs(context.Background(), "cid", tt.opts...)
			require.NoError(t, err)
			require.NoError(t, rc.Close())

			joined := strings.Join(gotArgs, " ")
			assert.NotContains(t, joined, "--tail all",
				"podman rejects --tail all with exit 125; it must never be sent")

			idx := indexOfArg(gotArgs, "--tail")
			if !tt.wantTail {
				assert.Equal(t, -1, idx, "--tail must be absent: %v", gotArgs)
				return
			}
			require.NotEqual(t, -1, idx, "--tail expected in %v", gotArgs)
			require.Less(t, idx+1, len(gotArgs))
			assert.Equal(t, tt.tailValue, gotArgs[idx+1])
		})
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
