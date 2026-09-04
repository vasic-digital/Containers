package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"time"
)

// ErrRunUnsupported is returned by ContainerRuntime.Run implementations whose
// underlying tool has no one-shot ephemeral-run form matching Run's contract.
// It is a SENTINEL, not a silent no-op: a caller can distinguish "this runtime
// cannot do it" from "it ran and failed" with errors.Is, and neither case is
// ever reported as success. Wrapped by crictl, LXD, Kubernetes and the remote
// SSH proxy.
var ErrRunUnsupported = errors.New("ephemeral run is not supported by this runtime")

// ErrStdinUnsupported is returned when a caller supplies WithRunStdin but the
// runtime's injected CommandExecutor cannot stream stdin (it does not implement
// StdinExecutor).
//
// This is deliberately an ERROR and not a degradation to "run it without
// stdin". The whole point of the primitive is streaming a document into the
// container; running the same command with an empty stdin would produce a
// plausible-looking empty result and a zero exit code, which is precisely the
// silent-failure shape the log reader in this package was just fixed for.
var ErrStdinUnsupported = errors.New(
	"executor does not support stdin: implement runtime.StdinExecutor",
)

// StdinExecutor is the OPTIONAL stdin-capable half of CommandExecutor.
//
// It is a separate interface rather than three more methods on CommandExecutor
// because CommandExecutor is exported and adding to it would break every
// external implementer (§11.4.122). defaultExecutor implements it, so the
// production path always has stdin; an injected fake that does not gets
// ErrStdinUnsupported the moment stdin is actually requested — never a
// silently discarded document.
type StdinExecutor interface {
	// ExecuteWithStdin runs a command with stdin streamed from r (nil means no
	// stdin) and returns its stdout, stderr and exit code. As with
	// ExecuteWithStderr, a non-zero exit is reported through the exit code with
	// a nil error; the error is reserved for failures to run the command at all.
	ExecuteWithStdin(
		ctx context.Context, r io.Reader, name string, args ...string,
	) ([]byte, []byte, int, error)
}

// ExecuteWithStdin implements StdinExecutor for the real os/exec path.
func (e *defaultExecutor) ExecuteWithStdin(
	ctx context.Context, r io.Reader, name string, args ...string,
) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	if r != nil {
		cmd.Stdin = r
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
		err = nil
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode, err
}

// --- Run Options ---

type runOptions struct {
	Stdin      io.Reader
	Remove     bool
	Name       string
	Entrypoint string
	WorkDir    string
	User       string
	Network    string
	Env        map[string]string
	Volumes    []string
	ExtraArgs  []string
}

// RunOption configures one-shot container execution.
type RunOption func(*runOptions)

// defaultRunOptions models the shape the two real consumers need: a one-shot
// `run --rm -i` that streams a document in and is gone afterwards. Remove
// defaults to TRUE because the alternative — leaking a stopped container per
// invocation — is a resource leak a caller has to remember to avoid, and the
// primitive exists precisely for throwaway work.
func defaultRunOptions() *runOptions {
	return &runOptions{
		Remove: true,
		Env:    make(map[string]string),
	}
}

func applyRunOptions(opts []RunOption) *runOptions {
	o := defaultRunOptions()
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// WithRunStdin streams r into the container's standard input.
//
// Supplying it makes the runtime pass the CLI's interactive-stdin flag (-i),
// so the container sees a real pipe rather than a closed descriptor. If the
// runtime's executor cannot stream stdin, Run fails with ErrStdinUnsupported
// rather than running the command without it.
func WithRunStdin(r io.Reader) RunOption {
	return func(o *runOptions) {
		o.Stdin = r
	}
}

// WithRunRemove sets whether the container is deleted once it exits (--rm).
// Defaults to true.
func WithRunRemove(remove bool) RunOption {
	return func(o *runOptions) {
		o.Remove = remove
	}
}

// WithRunName assigns a name to the ephemeral container (--name).
func WithRunName(name string) RunOption {
	return func(o *runOptions) {
		o.Name = name
	}
}

// WithRunEntrypoint overrides the image's entrypoint (--entrypoint).
func WithRunEntrypoint(entrypoint string) RunOption {
	return func(o *runOptions) {
		o.Entrypoint = entrypoint
	}
}

// WithRunWorkDir sets the working directory inside the container (-w).
func WithRunWorkDir(dir string) RunOption {
	return func(o *runOptions) {
		o.WorkDir = dir
	}
}

// WithRunUser sets the user the container process runs as (-u).
func WithRunUser(user string) RunOption {
	return func(o *runOptions) {
		o.User = user
	}
}

// WithRunNetwork attaches the container to a named network (--network).
func WithRunNetwork(network string) RunOption {
	return func(o *runOptions) {
		o.Network = network
	}
}

// WithRunEnv adds environment variables for the ephemeral container (-e).
func WithRunEnv(env map[string]string) RunOption {
	return func(o *runOptions) {
		for k, v := range env {
			o.Env[k] = v
		}
	}
}

// WithRunVolumes adds bind mounts in the CLI's own "source:target[:opts]" form
// (-v).
func WithRunVolumes(volumes ...string) RunOption {
	return func(o *runOptions) {
		o.Volumes = append(o.Volumes, volumes...)
	}
}

// WithRunExtraArgs appends verbatim flags immediately before the image name,
// for runtime-specific options this package does not model.
func WithRunExtraArgs(args ...string) RunOption {
	return func(o *runOptions) {
		o.ExtraArgs = append(o.ExtraArgs, args...)
	}
}

// buildRunArgs assembles the argv for a Docker-CLI-compatible `run`, shared by
// the docker, podman and nerdctl runtimes, whose run subcommands take the same
// flags. Env keys are emitted in sorted order so the argv is deterministic —
// Go map iteration is randomised, and a non-deterministic argv makes a test
// that asserts on it flaky rather than wrong.
func buildRunArgs(o *runOptions, image string, cmd []string) []string {
	args := []string{"run"}
	if o.Remove {
		args = append(args, "--rm")
	}
	if o.Stdin != nil {
		args = append(args, "-i")
	}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.Entrypoint != "" {
		args = append(args, "--entrypoint", o.Entrypoint)
	}
	if o.WorkDir != "" {
		args = append(args, "-w", o.WorkDir)
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	if o.Network != "" {
		args = append(args, "--network", o.Network)
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, o.Env[k]))
	}
	for _, v := range o.Volumes {
		args = append(args, "-v", v)
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, image)
	return append(args, cmd...)
}

// RunSpec is the resolved form of a Run call: the Docker-CLI-compatible argv
// (beginning with the "run" subcommand, excluding the binary name) and the
// stdin reader, if any.
//
// It exists so a ContainerRuntime implemented OUTSIDE this package —
// remote.RemoteRuntime is the one in this repository — can assemble exactly the
// same command line from the same options instead of re-deriving the flag
// mapping and drifting from it. Stdin is surfaced as a field rather than hidden
// because an implementation that cannot stream it MUST fail loudly, and it can
// only do that if it can see that stdin was asked for.
type RunSpec struct {
	// Args is the argv after the runtime binary, e.g.
	// ["run", "--rm", "-i", "alpine:3.20", "cat"].
	Args []string
	// Stdin is the reader supplied by WithRunStdin, or nil.
	Stdin io.Reader
}

// ResolveRunSpec applies the given RunOptions and returns the resolved argv and
// stdin reader. See RunSpec.
func ResolveRunSpec(image string, cmd []string, opts []RunOption) RunSpec {
	o := applyRunOptions(opts)
	return RunSpec{Args: buildRunArgs(o, image, cmd), Stdin: o.Stdin}
}

// runViaCLI performs the one-shot run for every Docker-CLI-compatible runtime.
//
// A non-zero container exit is NOT an error: the exit code is data the caller
// asked for, exactly as ExecResult already models it for Exec. An error means
// the run could not be performed — no image name, no stdin support, or the
// binary could not be executed at all.
func runViaCLI(
	ctx context.Context,
	executor CommandExecutor,
	binary, name, image string,
	cmd []string,
	opts []RunOption,
) (*ExecResult, error) {
	if image == "" {
		return nil, fmt.Errorf("%s run: image is required", name)
	}
	o := applyRunOptions(opts)
	args := buildRunArgs(o, image, cmd)

	var (
		stdout, stderr []byte
		exitCode       int
		err            error
	)
	if o.Stdin != nil {
		stdinExec, ok := executor.(StdinExecutor)
		if !ok {
			return nil, fmt.Errorf("%s run %s: %w", name, image, ErrStdinUnsupported)
		}
		stdout, stderr, exitCode, err = stdinExec.ExecuteWithStdin(
			ctx, o.Stdin, binary, args...,
		)
	} else {
		stdout, stderr, exitCode, err = executor.ExecuteWithStderr(
			ctx, binary, args...,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("%s run %s: %w", name, image, err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdout),
		Stderr:   string(stderr),
	}, nil
}
