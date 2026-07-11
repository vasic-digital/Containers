package crossbuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// containerRunner is the seam for invoking podman/docker. Production
// uses realContainerRunner (auto-detects via pkg/runtime); tests
// inject a fake.
type containerRunner interface {
	ImageExists(ctx context.Context, imageRef string) bool
	Run(ctx context.Context, spec containerRunSpec) (exitCode int, err error)
}

// containerRunSpec is the minimum surface needed by the crossbuild
// orchestrator. Intentionally LESS than pkg/runtime's full surface
// to avoid coupling crossbuild's tests to runtime's internal state.
type containerRunSpec struct {
	Image       string
	MountSource string
	MountTarget string
	WorkDir     string
	Command     string
	Environment map[string]string
	Stdout      *bytes.Buffer
	Stderr      *bytes.Buffer
	// SharedMount selects the SELinux relabel mode WHEN the host runs
	// SELinux. false (default) => private/unshared relabel (":Z");
	// true => shared relabel (":z") so the volume can be reused across
	// multiple containers. On a non-SELinux host the suffix is empty
	// regardless of this field. See mountArg.
	SharedMount bool
}

// selinuxEnabled is the injectable detection seam. Production points it
// at the real detector (detectSELinux); tests replace it to simulate
// both states without a real SELinux host. It is a package-level var
// rather than a runner field so the pure helper mountArg can consult it
// without threading a receiver through the call site.
//
// CLAUDE-2 / §11.4.81 cross-platform parity: the relabel suffix is a
// Linux-SELinux-only concept. On macOS (podman-machine / Docker Desktop)
// and on non-SELinux Linux hosts, forcing ":Z" makes the bind mount
// fail or misbehave, so we MUST emit no suffix there.
var selinuxEnabled = detectSELinux

// detectSELinux reports whether the host is actively running SELinux
// (enforcing OR permissive). SELinux is a Linux-only facility, so on any
// non-Linux GOOS (darwin, windows, …) this short-circuits to false
// BEFORE touching the filesystem. On Linux, the canonical signal is the
// presence of the selinuxfs pseudo-file /sys/fs/selinux/enforce; if it
// cannot be stat-ed (selinuxfs not mounted, SELinux disabled at boot),
// SELinux is not active and we fail safe to false. We deliberately do
// NOT shell out to `getenforce` — it is absent on macOS and is not the
// canonical detection mechanism.
func detectSELinux() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err != nil {
		return false
	}
	return true
}

// mountArg builds the value of the podman/docker `-v` flag for the given
// spec, applying the SELinux relabel suffix only when appropriate:
//
//	SELinux active + private (SharedMount=false) -> "<src>:<dst>:Z"
//	SELinux active + shared  (SharedMount=true)  -> "<src>:<dst>:z"
//	SELinux NOT active                           -> "<src>:<dst>"   (no suffix)
//
// The :z (shared) vs :Z (private/unshared) distinction follows the
// official container-runtime semantics (see §11.4.99 sources_verified):
// lowercase z labels content shared among multiple containers, uppercase
// Z labels it private to a single container.
//
// It is a pure function of (spec, selinuxEnabled) so it is unit-testable
// in isolation. A panic from a misbehaving injected detector is caught
// and treated as "SELinux not active" (fail safe to no relabel) — an
// unreadable/erroring selinux fs means SELinux is not active.
func mountArg(spec containerRunSpec) string {
	base := spec.MountSource + ":" + spec.MountTarget
	suffix := relabelSuffix(spec.SharedMount)
	return base + suffix
}

// relabelSuffix returns the bind-mount relabel suffix ("", ":z", or ":Z")
// for the current host. It isolates the selinuxEnabled() call so a
// panicking detector cannot crash the run path (chaos-safe): any panic
// is recovered and treated as SELinux-not-active.
func relabelSuffix(shared bool) (suffix string) {
	defer func() {
		if recover() != nil {
			suffix = ""
		}
	}()
	if !selinuxEnabled() {
		return ""
	}
	if shared {
		return ":z"
	}
	return ":Z"
}

type realContainerRunner struct{}

func (realContainerRunner) ImageExists(ctx context.Context, imageRef string) bool {
	binary := pickContainerBinary()
	if binary == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, binary, "image", "exists", imageRef)
	return cmd.Run() == nil
}

func (realContainerRunner) Run(ctx context.Context, spec containerRunSpec) (int, error) {
	binary := pickContainerBinary()
	if binary == "" {
		return -1, fmt.Errorf("neither podman nor docker is installed on this host")
	}
	args := []string{
		"run", "--rm", "--read-only", "--tmpfs", "/tmp:rw",
		"-v", mountArg(spec),
		"-w", spec.WorkDir,
	}
	for k, v := range spec.Environment {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Image, "sh", "-c", spec.Command)
	cmd := exec.CommandContext(ctx, binary, args...)
	// XB2-1: bound Wait()'s pipe-drain and kill the whole process
	// group (not just this "docker"/"podman" CLI invocation) on ctx
	// cancellation, matching pkg/runtime/docker.go's identical guard.
	// This is belt-and-braces alongside "--rm": "--rm" only removes
	// THIS container on ITS OWN exit — it does nothing for a
	// detached/backgrounded process the in-container command itself
	// launched, and does nothing to unblock a wedged host-side
	// `docker`/`podman run` CLI process whose own descendants (if any)
	// are still holding the stdout/stderr pipes open.
	cmd.WaitDelay = subprocessWaitDelay
	configureProcessGroup(cmd)
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return exitCode, err
}

// pickContainerBinary mirrors pkg/runtime auto-detection but kept
// inline so pkg/crossbuild does not introduce a hard import on
// pkg/runtime (which would create a build-time cycle with the
// distribution package). The rule is the same: prefer rootless
// podman, fall back to docker.
func pickContainerBinary() string {
	for _, bin := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(bin); err == nil {
			return bin
		}
	}
	return ""
}
