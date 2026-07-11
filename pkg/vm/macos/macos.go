// SPDX-License-Identifier: Apache-2.0

// Package macos orchestrates macOS virtual machines via Tart
// (https://tart.run), an Apple-blessed open-source tool for running
// macOS/iOS VMs on Apple Silicon hosts.
//
// # Constraint (honest disclosure, CONST-039)
//
// Tart requires:
//   - A macOS host with Apple Silicon (M1/M2/M3). It does NOT run on
//     Linux, Windows, or Intel Mac hosts. This is an Apple licensing
//     and virtualisation-framework constraint — not a code limitation.
//   - Tart CLI installed on the host (`brew install tart` or direct
//     download from tart.run).
//
// This package makes the constraint explicit at runtime: if the host
// is not macOS or Tart is not installed, every method returns an
// honest error citing the requirement rather than silently succeeding
// or panicking.
//
// # Anti-bluff posture (CONST-039)
//
// Tests skip with `// SKIP-OK: #tart-requires-macos-apple-silicon`
// when Tart is not installed. Orchestration logic is covered with an
// injected `tartRunner` seam so CI agents on non-macOS hosts can
// still verify the wiring is correct. The real-stack path (actual
// Tart VM boot) is tested via a Challenge script that skips
// identically on non-macOS hosts.
package macos

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// tartRunner is the seam through which MacOSBuilder invokes the Tart
// CLI. Production uses osExecTartRunner; tests inject a fake.
type tartRunner interface {
	// Version runs `tart --version` and returns the version string.
	// Returns error if Tart is not installed.
	Version(ctx context.Context) (string, error)

	// Clone clones a remote Tart image to a local VM name.
	Clone(ctx context.Context, remoteImage, vmName string) error

	// Run boots the named VM in headless mode. The VM runs until
	// Stop() is called or the context is cancelled.
	Run(ctx context.Context, vmName string, opts RunOptions) error

	// SSHExec executes cmd over SSH inside the named running VM.
	SSHExec(ctx context.Context, vmName string, user, pass, cmd string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)

	// Stop powers off the named VM.
	Stop(ctx context.Context, vmName string) error

	// Delete removes the named VM from the local image store.
	Delete(ctx context.Context, vmName string) error
}

// RunOptions controls how the Tart VM is booted.
type RunOptions struct {
	// Headless requests no graphical display (true for CI use).
	Headless bool

	// MountDir, if non-empty, mounts a host directory inside the VM
	// at the path specified by MountTarget.
	MountDir    string
	MountTarget string
}

// VMRunRequest is the input to MacOSBuilder.RunInVM.
type VMRunRequest struct {
	// Image is the Tart image name or OCI reference to boot.
	// Examples: "ghcr.io/cirruslabs/macos-sonoma-xcode:latest",
	//           "ghcr.io/vasic-digital/macos-sonoma-jdk21:latest"
	Image string

	// Command is the shell command to run inside the VM over SSH
	// once the VM is booted and ready.
	Command string

	// User + Pass are the SSH credentials for the macOS VM guest.
	// Standard for Tart images: user "admin", pass "admin".
	User string
	Pass string

	// MountDir optionally mounts a host directory inside the VM.
	MountDir    string
	MountTarget string

	// Timeout caps the total operation (clone + boot + run + stop).
	// Default 60 minutes.
	Timeout time.Duration

	// KeepVM, if true, skips the Delete step after the run so the
	// operator can inspect the VM state. Default false (delete on
	// completion or error).
	KeepVM bool
}

// VMRunResult captures the outcome of a VMRunRequest.
type VMRunResult struct {
	// VMName is the ephemeral local name used for this run
	// (generated from Image + timestamp to avoid collisions).
	VMName string

	// ExitCode is the exit code of Command inside the VM.
	ExitCode int

	// Stdout / Stderr capture Command's output (up to 64 KB each).
	Stdout string
	Stderr string

	// Duration is wall-clock from RunInVM invocation to return.
	Duration time.Duration

	// Error is non-nil if the operation failed (VM did not boot,
	// Command returned non-zero, SSH failed, etc.).
	Error error
}

// MacOSBuilder orchestrates macOS VMs via Tart.
//
// Use MacOSBuilder for:
//   - Building macOS .pkg / .dmg installers (jpackage target).
//   - Running iOS simulator tests inside a macOS VM.
//   - Any build step that requires macOS userspace + Xcode toolchain.
//
// Honesty note: MacOSBuilder.RunInVM returns ErrTartNotAvailable
// (a typed sentinel) when the host is not macOS-arm64 or Tart is not
// installed, so callers can detect this programmatically and skip
// gracefully in non-macOS CI.
type MacOSBuilder struct {
	tart tartRunner
}

// ErrTartNotAvailable is returned by all MacOSBuilder methods when
// Tart is not installed on the current host.
var ErrTartNotAvailable = fmt.Errorf(
	"Tart is not installed or host is not macOS Apple Silicon; " +
		"install via `brew install tart` on macOS arm64 — " +
		"see https://tart.run (SKIP-OK: #tart-requires-macos-apple-silicon)")

// NewMacOSBuilder returns the production builder. It does NOT fail if
// Tart is absent — callers discover the absence via RunInVM's return
// value, which lets them skip gracefully.
func NewMacOSBuilder() *MacOSBuilder {
	return &MacOSBuilder{tart: &osExecTartRunner{}}
}

// newMacOSBuilderWithRunner is the test seam.
func newMacOSBuilderWithRunner(r tartRunner) *MacOSBuilder {
	return &MacOSBuilder{tart: r}
}

// TartVersion returns the installed Tart version string, or
// ErrTartNotAvailable if Tart is absent or the host is non-macOS.
func (m *MacOSBuilder) TartVersion(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrTartNotAvailable
	}
	v, err := m.tart.Version(ctx)
	if err != nil {
		return "", ErrTartNotAvailable
	}
	return v, nil
}

// RunInVM clones image to an ephemeral local VM, boots it, SSHs in,
// runs req.Command, captures output, and tears down the VM.
//
// Returns ErrTartNotAvailable immediately when:
//   - runtime.GOOS != "darwin", OR
//   - `tart --version` fails (Tart not installed).
func (m *MacOSBuilder) RunInVM(ctx context.Context, req VMRunRequest) VMRunResult {
	start := time.Now()

	if runtime.GOOS != "darwin" {
		return VMRunResult{
			Duration: time.Since(start),
			Error:    ErrTartNotAvailable,
		}
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 60 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Verify Tart is present before cloning the (potentially large) image.
	if _, err := m.tart.Version(ctx); err != nil {
		return VMRunResult{Duration: time.Since(start), Error: ErrTartNotAvailable}
	}

	if req.Image == "" {
		return VMRunResult{
			Duration: time.Since(start),
			Error:    fmt.Errorf("MacOSBuilder.RunInVM: VMRunRequest.Image is required"),
		}
	}
	if req.Command == "" {
		return VMRunResult{
			Duration: time.Since(start),
			Error:    fmt.Errorf("MacOSBuilder.RunInVM: VMRunRequest.Command is required"),
		}
	}

	user := req.User
	if user == "" {
		user = "admin"
	}
	pass := req.Pass
	if pass == "" {
		pass = "admin"
	}

	vmName := fmt.Sprintf("crossbuild-%d", time.Now().UnixNano())

	result := VMRunResult{VMName: vmName}

	// Clone image → local VM.
	if err := m.tart.Clone(ctx, req.Image, vmName); err != nil {
		result.Duration = time.Since(start)
		result.Error = fmt.Errorf("tart clone %q → %q: %w", req.Image, vmName, err)
		return result
	}
	defer func() {
		if !req.KeepVM {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			_ = m.tart.Stop(stopCtx, vmName)
			_ = m.tart.Delete(stopCtx, vmName)
		}
	}()

	// Boot VM in background goroutine. Tart's `run` command blocks
	// until the VM shuts down (or the context is cancelled). The
	// goroutine sends its exit error on runErrCh; a nil means the VM
	// shut down cleanly AFTER the caller's SSHExec has already
	// returned — that is normal. A non-nil sent BEFORE SSHExec
	// completes means the VM crashed during boot.
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- m.tart.Run(ctx, vmName, RunOptions{
			Headless:    true,
			MountDir:    req.MountDir,
			MountTarget: req.MountTarget,
		})
	}()

	// Give the VM a moment to start listening on the SSH port that
	// Tart maps to the host. In tests the fake Run() returns quickly;
	// we don't treat that as a hard failure — we proceed to SSHExec
	// which will fail fast if the VM is genuinely unavailable.
	bootWait := time.NewTimer(30 * time.Second)
	select {
	case err := <-runErrCh:
		if err != nil {
			// VM exited with an error before we could SSH in — hard failure.
			result.Duration = time.Since(start)
			result.Error = fmt.Errorf("tart run %q: exited with error: %w", vmName, err)
			return result
		}
		// VM exited without error early (test scenario or very fast boot).
		// Proceed to SSHExec; it will fail if the VM is truly gone.
	case <-bootWait.C:
		// 30s elapsed — continue to SSHExec regardless.
	case <-ctx.Done():
		result.Duration = time.Since(start)
		result.Error = fmt.Errorf("context cancelled during VM boot: %w", ctx.Err())
		return result
	}

	// SSH-exec the consumer command.
	stdout, stderr, exitCode, err := m.tart.SSHExec(ctx, vmName, user, pass, req.Command, timeout/2)
	result.Stdout = tailString(stdout, 65536)
	result.Stderr = tailString(stderr, 65536)
	result.ExitCode = exitCode
	result.Duration = time.Since(start)
	if err != nil {
		result.Error = fmt.Errorf("tart SSH exec in %q: %w", vmName, err)
		return result
	}
	if exitCode != 0 {
		result.Error = fmt.Errorf("command in VM %q exited %d", vmName, exitCode)
	}
	return result
}

// tailString returns the last n bytes of s as a string. Used to cap
// log output at a sane size without losing the tail (where failures
// usually manifest).
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// osExecTartRunner is the production tartRunner implementation.
type osExecTartRunner struct{}

func (o *osExecTartRunner) Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "tart", "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tart --version: %w", err)
	}
	return string(bytes.TrimSpace(out)), nil
}

func (o *osExecTartRunner) Clone(ctx context.Context, remoteImage, vmName string) error {
	cmd := exec.CommandContext(ctx, "tart", "clone", remoteImage, vmName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tart clone: %s: %w", string(out), err)
	}
	return nil
}

func (o *osExecTartRunner) Run(ctx context.Context, vmName string, opts RunOptions) error {
	args := []string{"run"}
	if opts.Headless {
		args = append(args, "--no-graphics")
	}
	if opts.MountDir != "" && opts.MountTarget != "" {
		args = append(args, "--dir", opts.MountTarget+":"+opts.MountDir)
	}
	args = append(args, vmName)
	cmd := exec.CommandContext(ctx, "tart", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tart run %q: %s: %w", vmName, string(out), err)
	}
	return nil
}

func (o *osExecTartRunner) SSHExec(ctx context.Context, vmName, user, pass, command string, timeout time.Duration) (stdout, stderr string, exitCode int, err error) {
	// VM3-9 (SECURITY argv flag-injection): the ssh destination positional is
	// "<user>@<ip>"; a user beginning with '-' (e.g. "-oProxyCommand=<cmd>")
	// makes the whole destination begin with '-', which ssh's getopt parses as
	// an OPTION rather than a host → ProxyCommand arbitrary command execution on
	// the Tart HOST. ssh has no reliable "--" host terminator for the
	// destination, so refuse a leading-dash user BEFORE any spawn (mirrors
	// pkg/remote sshDestination + pkg/vm/ios checkSimctlArgs). A legitimate
	// macOS VM user (the Tart default "admin") never begins with '-'; an empty
	// user yields "@<ip>", which begins with '@' and is inert to getopt.
	if strings.HasPrefix(user, "-") {
		return "", "", -1, fmt.Errorf(
			"macos: SSH user %q begins with '-' (would be parsed as an ssh "+
				"option — argument injection); refusing", user)
	}
	// Tart provides `tart ip <vm>` to get the guest's IP; we then
	// use standard ssh. This is the minimal production implementation.
	// A full implementation would use golang.org/x/crypto/ssh for
	// in-process SSH — left as a follow-up per CONST-039 gap tracker.
	ipCmd := exec.CommandContext(ctx, "tart", "ip", "--wait", "60", vmName)
	ipOut, ipErr := ipCmd.Output()
	if ipErr != nil {
		return "", "", -1, fmt.Errorf("tart ip %q: %w", vmName, ipErr)
	}
	ip := string(bytes.TrimSpace(ipOut))

	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=30",
		user + "@" + ip,
		command,
	}
	sshCmd := exec.CommandContext(ctx, "sshpass", append([]string{"-p", pass, "ssh"}, sshArgs...)...)
	var stdoutBuf, stderrBuf bytes.Buffer
	sshCmd.Stdout = &stdoutBuf
	sshCmd.Stderr = &stderrBuf
	runErr := sshCmd.Run()
	code := 0
	if sshCmd.ProcessState != nil {
		code = sshCmd.ProcessState.ExitCode()
	}
	return stdoutBuf.String(), stderrBuf.String(), code, runErr
}

func (o *osExecTartRunner) Stop(ctx context.Context, vmName string) error {
	cmd := exec.CommandContext(ctx, "tart", "stop", vmName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tart stop %q: %s: %w", vmName, string(out), err)
	}
	return nil
}

func (o *osExecTartRunner) Delete(ctx context.Context, vmName string) error {
	cmd := exec.CommandContext(ctx, "tart", "delete", vmName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tart delete %q: %s: %w", vmName, string(out), err)
	}
	return nil
}
