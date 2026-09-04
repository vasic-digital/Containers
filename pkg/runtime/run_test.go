//go:build !integration

package runtime

import (
	"bytes"
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
// The gap these tests pin down
//
// ContainerRuntime declared Name, Version, IsAvailable, Start, Stop, Remove,
// Status, List, Stats, Exec and Logs — and NO ephemeral-run primitive at all.
// Exec(ctx, id, cmd) needs a container that already exists and accepts no
// stdin. The only WithStdin in the module sat in
// pkg/remote/connection/interface.go, an interfaces-and-options-only package
// that nothing implements and no constructor returns.
//
// The real consumers are two umbrella scripts —
// _tools/helixtranslate-container.sh and _tools/helixtranslate-container/run.sh
// — which each perform a one-shot `run --rm -i` that streams a document on
// STDIN and reads the result back. That shape was inexpressible through this
// module, which is why §11.4.76(4) puts the fix HERE rather than as a parallel
// implementation in the consuming project.
//
// Run(ctx, image, cmd, opts...) closes it. The load-bearing option is
// WithRunStdin.
// ---------------------------------------------------------------------------

// stdinCapableExecutor is a CommandExecutor that ALSO implements StdinExecutor,
// recording what it was asked to run and what was streamed in.
type stdinCapableExecutor struct {
	gotName   string
	gotArgs   []string
	gotStdin  string
	stdinSeen bool

	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (s *stdinCapableExecutor) Execute(
	context.Context, string, ...string,
) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stdinCapableExecutor) ExecuteWithStderr(
	_ context.Context, name string, args ...string,
) ([]byte, []byte, int, error) {
	s.gotName, s.gotArgs = name, args
	return []byte(s.stdout), []byte(s.stderr), s.exitCode, s.err
}

func (s *stdinCapableExecutor) ExecuteStream(
	context.Context, string, ...string,
) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stdinCapableExecutor) ExecuteWithStdin(
	_ context.Context, r io.Reader, name string, args ...string,
) ([]byte, []byte, int, error) {
	s.stdinSeen = true
	s.gotName, s.gotArgs = name, args
	if r != nil {
		b, _ := io.ReadAll(r)
		s.gotStdin = string(b)
	}
	return []byte(s.stdout), []byte(s.stderr), s.exitCode, s.err
}

// stdinBlindExecutor is a CommandExecutor that does NOT implement
// StdinExecutor — the mutant's habitat.
type stdinBlindExecutor struct {
	called  bool
	gotArgs []string
}

func (s *stdinBlindExecutor) Execute(
	context.Context, string, ...string,
) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stdinBlindExecutor) ExecuteWithStderr(
	_ context.Context, _ string, args ...string,
) ([]byte, []byte, int, error) {
	s.called = true
	s.gotArgs = args
	return []byte("output produced with NO stdin"), nil, 0, nil
}

func (s *stdinBlindExecutor) ExecuteStream(
	context.Context, string, ...string,
) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

// ---------------------------------------------------------------------------
// 1. The primitive's shape
// ---------------------------------------------------------------------------

// TestPodmanRun_ProducesTheOneShotArgvTheConsumersNeed asserts the exact CLI
// form the two umbrella scripts use today: `run --rm -i <image> <cmd...>`.
func TestPodmanRun_ProducesTheOneShotArgvTheConsumersNeed(t *testing.T) {
	exe := &stdinCapableExecutor{stdout: "translated"}
	p := NewPodmanRuntimeWithExecutor(exe)

	_, err := p.Run(
		context.Background(),
		"helixtranslate:latest",
		[]string{"translate", "--to", "sr"},
		WithRunStdin(strings.NewReader("a document")),
	)
	require.NoError(t, err)

	assert.Equal(t, "podman", exe.gotName)
	assert.Equal(t, []string{
		"run", "--rm", "-i",
		"helixtranslate:latest",
		"translate", "--to", "sr",
	}, exe.gotArgs)
}

// TestPodmanRun_StreamsStdinAndCapturesBothStreamsAndExitCode is the whole
// point of the primitive: a document goes in, output and an exit code come
// back.
func TestPodmanRun_StreamsStdinAndCapturesBothStreamsAndExitCode(t *testing.T) {
	const document = "Здраво, свете!\nsecond line\n"
	exe := &stdinCapableExecutor{
		stdout:   "Hello, world!\n",
		stderr:   "warning: guessed source language\n",
		exitCode: 3,
	}
	p := NewPodmanRuntimeWithExecutor(exe)

	res, err := p.Run(
		context.Background(), "img:1", []string{"cat"},
		WithRunStdin(strings.NewReader(document)),
	)

	require.NoError(t, err, "a non-zero CONTAINER exit is data, not a Run error")
	require.NotNil(t, res)
	assert.True(t, exe.stdinSeen, "the stdin-capable path must have been taken")
	assert.Equal(t, document, exe.gotStdin, "the document must arrive byte-for-byte")
	assert.Equal(t, "Hello, world!\n", res.Stdout)
	assert.Equal(t, "warning: guessed source language\n", res.Stderr)
	assert.Equal(t, 3, res.ExitCode)
}

// TestPodmanRun_WithoutStdinOmitsTheInteractiveFlag is the CONTROL for the -i
// flag: it appears because stdin was requested, not unconditionally.
func TestPodmanRun_WithoutStdinOmitsTheInteractiveFlag(t *testing.T) {
	exe := &stdinCapableExecutor{stdout: "ok"}
	p := NewPodmanRuntimeWithExecutor(exe)

	_, err := p.Run(context.Background(), "img:1", []string{"echo", "hi"})
	require.NoError(t, err)

	assert.NotContains(t, exe.gotArgs, "-i",
		"-i must be driven by WithRunStdin, not always present")
	assert.Contains(t, exe.gotArgs, "--rm")
	assert.False(t, exe.stdinSeen,
		"with no stdin requested the plain executor path is correct")
}

// TestRun_OptionsMapToFlags covers the rest of the option surface, including
// that env vars are emitted deterministically (Go map order is randomised, and
// a non-deterministic argv makes a test flaky rather than wrong).
func TestRun_OptionsMapToFlags(t *testing.T) {
	exe := &stdinCapableExecutor{}
	d := NewDockerRuntimeWithExecutor(exe)

	_, err := d.Run(
		context.Background(), "img:2", []string{"sh", "-c", "true"},
		WithRunRemove(false),
		WithRunName("one-shot"),
		WithRunEntrypoint("/bin/sh"),
		WithRunWorkDir("/w"),
		WithRunUser("1000:1000"),
		WithRunNetwork("none"),
		WithRunEnv(map[string]string{"ZED": "z", "ALPHA": "a"}),
		WithRunVolumes("/host:/ctr:ro"),
		WithRunExtraArgs("--read-only"),
	)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"run",
		"--name", "one-shot",
		"--entrypoint", "/bin/sh",
		"-w", "/w",
		"-u", "1000:1000",
		"--network", "none",
		"-e", "ALPHA=a",
		"-e", "ZED=z",
		"-v", "/host:/ctr:ro",
		"--read-only",
		"img:2",
		"sh", "-c", "true",
	}, exe.gotArgs)
	assert.NotContains(t, exe.gotArgs, "--rm",
		"WithRunRemove(false) must drop --rm")
}

// TestRun_EmptyImageIsRejected — an empty image would otherwise produce a
// command whose first positional argument is the container's own argv.
func TestRun_EmptyImageIsRejected(t *testing.T) {
	exe := &stdinCapableExecutor{}
	p := NewPodmanRuntimeWithExecutor(exe)

	res, err := p.Run(context.Background(), "", []string{"cat"})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "image is required")
}

// TestRun_UnsupportedRuntimesReturnTheSentinelNotASilentSuccess pins the
// runtimes that have no equivalent one-shot form. They must say so with a
// sentinel a caller can test for — never return an empty result and nil error.
func TestRun_UnsupportedRuntimesReturnTheSentinelNotASilentSuccess(t *testing.T) {
	cases := map[string]ContainerRuntime{
		"cri-o":      NewCRIORuntimeWithExecutor(&stdinCapableExecutor{}),
		"lxd":        NewLXDRuntimeWithExecutor(&stdinCapableExecutor{}),
		"kubernetes": NewKubernetesRuntimeWithExecutor(&stdinCapableExecutor{}, "default"),
	}
	for name, rt := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := rt.Run(context.Background(), "img:1", []string{"cat"})
			require.Error(t, err)
			assert.Nil(t, res, "no result may accompany an unsupported runtime")
			assert.True(t, errors.Is(err, ErrRunUnsupported),
				"must wrap ErrRunUnsupported so callers can branch on it; got %v", err)
		})
	}
}

// ---------------------------------------------------------------------------
// 2. Stdin must never be silently dropped
//
// This is the same failure class the log reader was just repaired for: running
// the command WITHOUT the document, exiting 0, and handing back a
// plausible-looking result is indistinguishable from success.
// ---------------------------------------------------------------------------

// stdinDropVerdict is the contract check, shared by the real path and the
// mutant so the mutation below is a genuine paired proof.
func stdinDropVerdict(res *ExecResult, err error, executorRan bool) string {
	if err == nil {
		return fmt.Sprintf(
			"SILENTLY DROPPED STDIN: Run returned a result (%+v) and a nil "+
				"error although the executor could not stream stdin "+
				"(executor ran: %v)", res, executorRan,
		)
	}
	if !errors.Is(err, ErrStdinUnsupported) {
		return fmt.Sprintf(
			"WRONG SENTINEL: error %v does not wrap ErrStdinUnsupported, so a "+
				"caller cannot distinguish it from an ordinary run failure", err,
		)
	}
	return ""
}

// TestRun_StdinWithABlindExecutorIsAnErrorNotASilentDrop is the regression
// test: the command must NOT be run at all when the document cannot be
// delivered.
func TestRun_StdinWithABlindExecutorIsAnErrorNotASilentDrop(t *testing.T) {
	exe := &stdinBlindExecutor{}
	p := NewPodmanRuntimeWithExecutor(exe)

	res, err := p.Run(
		context.Background(), "img:1", []string{"cat"},
		WithRunStdin(strings.NewReader("a document that must not be lost")),
	)

	assert.Empty(t, stdinDropVerdict(res, err, exe.called))
	assert.False(t, exe.called,
		"the command must not run at all if the document cannot be delivered")
	assert.Nil(t, res)
}

// TestMutation_SilentStdinDropIsCaught is the PAIRED MUTATION (§1.1).
//
// It reproduces the tempting "graceful degradation" — fall back to the
// stdin-less executor and return its output — and proves the contract check
// rejects it. If someone makes runViaCLI degrade that way, or weakens
// stdinDropVerdict, this goes red.
func TestMutation_SilentStdinDropIsCaught(t *testing.T) {
	// The mutant: exactly what a "just fall back" implementation would return.
	mutantRes := &ExecResult{ExitCode: 0, Stdout: "output produced with NO stdin"}

	verdict := stdinDropVerdict(mutantRes, nil, true)

	require.NotEmpty(t, verdict,
		"MUTATION ESCAPED: a Run that silently ignored the document and "+
			"returned a zero-exit result was accepted — the regression test "+
			"above proves nothing")
	assert.Contains(t, verdict, "SILENTLY DROPPED STDIN")
}

// TestMutation_WrongSentinelIsCaught guards the other half: erroring is not
// enough if the caller cannot tell WHY. An unwrapped error must be rejected.
func TestMutation_WrongSentinelIsCaught(t *testing.T) {
	verdict := stdinDropVerdict(nil, errors.New("something went wrong"), false)

	require.NotEmpty(t, verdict,
		"MUTATION ESCAPED: an error that does not wrap ErrStdinUnsupported "+
			"was accepted")
	assert.Contains(t, verdict, "WRONG SENTINEL")
}

// TestControl_CorrectBehaviourPassesTheSameCheck is the CONTROL for the two
// mutations: the predicate is satisfiable, so the mutation tests above are not
// passing merely because it rejects everything.
func TestControl_CorrectBehaviourPassesTheSameCheck(t *testing.T) {
	wrapped := fmt.Errorf("podman run img:1: %w", ErrStdinUnsupported)
	assert.Empty(t, stdinDropVerdict(nil, wrapped, false),
		"the correct shape must pass the very check that rejects the mutants")
}

// ---------------------------------------------------------------------------
// 3. The executor's own stdin plumbing, against real processes
// ---------------------------------------------------------------------------

// TestDefaultExecutor_ExecuteWithStdin_RoundTripsRealBytes runs a real process
// rather than a fake, so the stdin wiring is proved rather than asserted.
func TestDefaultExecutor_ExecuteWithStdin_RoundTripsRealBytes(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		// This test drives a REAL child process to prove the stdin wiring;
		// with no `cat` there is nothing to prove against, and a fake would
		// only test the fake. The fake-executor tests above still cover the
		// contract unconditionally, so this removes no coverage of it.
		// SKIP-OK: no `cat` on PATH means no real process to round-trip through.
		t.Skip("cat not on PATH")
	}
	const payload = "line one\nline two\nдва\n"
	exe := newDefaultExecutor()

	stdout, _, code, err := exe.ExecuteWithStdin(
		context.Background(), strings.NewReader(payload), "cat",
	)

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, payload, string(stdout))
	assert.Len(t, stdout, len(payload))
}

// TestDefaultExecutor_ExecuteWithStdin_ReportsNonZeroExitAsData mirrors
// ExecuteWithStderr's contract: a non-zero exit is a code, not a Go error.
func TestDefaultExecutor_ExecuteWithStdin_ReportsNonZeroExitAsData(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		// As above — this asserts the non-zero-exit contract against a real
		// process. Without a shell there is no process to exit non-zero; the
		// fake-executor coverage of the same contract still runs.
		// SKIP-OK: no `sh` on PATH means no real process to exit non-zero.
		t.Skip("sh not on PATH")
	}
	exe := newDefaultExecutor()

	_, stderr, code, err := exe.ExecuteWithStdin(
		context.Background(), bytes.NewReader(nil),
		"sh", "-c", "echo boom >&2; exit 7",
	)

	require.NoError(t, err, "a non-zero exit is data, not an error")
	assert.Equal(t, 7, code)
	assert.Contains(t, string(stderr), "boom")
}

// TestDefaultExecutor_SatisfiesStdinExecutor is the compile-time guarantee
// that the production executor takes the stdin path rather than the
// ErrStdinUnsupported branch.
func TestDefaultExecutor_SatisfiesStdinExecutor(t *testing.T) {
	var _ StdinExecutor = (*defaultExecutor)(nil)
	_, ok := CommandExecutor(newDefaultExecutor()).(StdinExecutor)
	assert.True(t, ok,
		"the real executor must support stdin, or every Run with a document "+
			"would fail with ErrStdinUnsupported in production")
}

// TestResolveRunSpec_ExposesArgvAndStdinForOutOfPackageImplementers pins the
// seam remote.RemoteRuntime uses. Stdin must be VISIBLE on the spec: an
// implementation that cannot stream it can only fail loudly if it can see that
// it was asked for.
func TestResolveRunSpec_ExposesArgvAndStdinForOutOfPackageImplementers(t *testing.T) {
	spec := ResolveRunSpec("img:1", []string{"cat"}, []RunOption{
		WithRunStdin(strings.NewReader("doc")),
	})
	assert.Equal(t, []string{"run", "--rm", "-i", "img:1", "cat"}, spec.Args)
	require.NotNil(t, spec.Stdin, "stdin must be visible to the implementer")

	bare := ResolveRunSpec("img:1", nil, nil)
	assert.Equal(t, []string{"run", "--rm", "img:1"}, bare.Args)
	assert.Nil(t, bare.Stdin)
}
