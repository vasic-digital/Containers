package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// LinuxRunRequest describes an arbitrary shell command to execute
// inside a Linux container with a host directory bind-mounted. It is
// the generic, project-agnostic primitive a consumer uses to run (and
// capture the output of) a command in a Linux environment from a macOS
// host — independent of the artifact-producing Build() flow.
//
// Unlike BuildRequest, LinuxRunRequest does NOT assert a produced
// artifact: it captures stdout/stderr + the command's exit code and
// hands them back verbatim. This is what makes it suitable for
// "run the test suite and tell me what it printed" use cases.
type LinuxRunRequest struct {
	// Image is the OCI image reference to run (e.g.
	// "docker.io/library/alpine:latest"). Required.
	Image string

	// Command is the shell command executed via `sh -c` inside the
	// container, with WorkDir (or the mount target) as the working
	// directory. Required.
	Command string

	// MountSource is the absolute host path bind-mounted into the
	// container at MountTarget. Optional — when empty no mount is
	// added (the command runs against the image's own filesystem).
	MountSource string

	// MountTarget is the in-container path MountSource is mounted at.
	// Defaults to "/work/src" when MountSource is set and this is
	// empty.
	MountTarget string

	// WorkDir is the in-container working directory. Defaults to
	// MountTarget when set, else "/".
	WorkDir string

	// Environment is additional env vars set inside the container.
	Environment map[string]string

	// Timeout caps the wall-clock of the run. Defaults to 10 minutes.
	Timeout time.Duration
}

// LinuxRunResult is the captured outcome of a LinuxRunRequest.
type LinuxRunResult struct {
	// Stdout is the full captured stdout of the command.
	Stdout string
	// Stderr is the full captured stderr of the command.
	Stderr string
	// ExitCode is the command's exit code (0 on success). -1 when the
	// command could not be launched.
	ExitCode int
	// Duration is wall-clock from invocation to return.
	Duration time.Duration
	// BackendName identifies which engine ran the command.
	BackendName string
	// Error is non-nil if the command could not be launched or the
	// engine was unavailable. A non-zero ExitCode alone is NOT an
	// Error — the caller inspects ExitCode for the command's verdict.
	Error error
}

// RunInLinuxContainer executes Command inside a Linux container managed
// by Apple's `container` CLI, with MountSource bind-mounted, capturing
// stdout/stderr + exit code. It is the generic primitive consumers use
// to run an arbitrary command in a Linux environment on a macOS host.
//
// This uses the production realAppleContainerRunner. Tests inject a
// fake via runLinuxContainerWithRunner.
func RunInLinuxContainer(ctx context.Context, req LinuxRunRequest) LinuxRunResult {
	return runLinuxContainerWithRunner(ctx, req, realAppleContainerRunner{})
}

// runLinuxContainerWithRunner is the test seam for RunInLinuxContainer.
func runLinuxContainerWithRunner(ctx context.Context, req LinuxRunRequest, runner appleContainerRunner) LinuxRunResult {
	start := time.Now()
	name := "apple-container"

	if req.Image == "" {
		return LinuxRunResult{ExitCode: -1, BackendName: name, Duration: time.Since(start),
			Error: fmt.Errorf("crossbuild: LinuxRunRequest.Image is required")}
	}
	if req.Command == "" {
		return LinuxRunResult{ExitCode: -1, BackendName: name, Duration: time.Since(start),
			Error: fmt.Errorf("crossbuild: LinuxRunRequest.Command is required")}
	}
	if req.MountSource != "" && !filepath.IsAbs(req.MountSource) {
		return LinuxRunResult{ExitCode: -1, BackendName: name, Duration: time.Since(start),
			Error: fmt.Errorf("crossbuild: LinuxRunRequest.MountSource must be absolute, got %q", req.MountSource)}
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !runner.Available() {
		return LinuxRunResult{ExitCode: -1, BackendName: name, Duration: time.Since(start),
			Error: fmt.Errorf(
				"apple `container` CLI not found on PATH; install per " +
					"https://github.com/apple/container then `container system start`")}
	}

	mountTarget := req.MountTarget
	if req.MountSource != "" && mountTarget == "" {
		mountTarget = "/work/src"
	}
	workDir := req.WorkDir
	if workDir == "" {
		if mountTarget != "" {
			workDir = mountTarget
		} else {
			workDir = "/"
		}
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := runner.Run(ctx, appleContainerRunSpec{
		Image:       req.Image,
		MountSource: req.MountSource,
		MountTarget: mountTarget,
		WorkDir:     workDir,
		Command:     req.Command,
		Environment: req.Environment,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})

	return LinuxRunResult{
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		ExitCode:    exitCode,
		Duration:    time.Since(start),
		BackendName: name,
		Error:       err,
	}
}
