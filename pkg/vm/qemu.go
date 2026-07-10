package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// processRunner is the seam through which QEMUVM launches qemu-system-*.
// Production uses os/exec; tests inject a fake.
type processRunner interface {
	StartDetached(name string, args ...string) error
}

type osProcessRunner struct{}

func (osProcessRunner) StartDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// sshClient abstracts SSH session + SCP operations.
//
// WaitForListener vs Authenticate (I4 split): WaitForListener is the
// listener-up TCP probe used by WaitForReady; Authenticate is the full
// SSH handshake + userauth used before Upload/Run/Download. Collapsing
// these into one call (the v0.1 pre-fix `Dial`) made WaitForReady
// require empty-password root auth to succeed, which production guests
// reject — leaving the matrix runner timing out against fully-booted
// VMs. See clients.go for production semantics.
type sshClient interface {
	WaitForListener(ctx context.Context, port int, timeout time.Duration) error
	Authenticate(ctx context.Context, port int, timeout time.Duration) error
	Upload(ctx context.Context, hostPath, vmPath string) error
	Run(ctx context.Context, script string, env map[string]string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)
	Download(ctx context.Context, vmPath, hostPath string) error
	Close() error
}

// qmpClient abstracts QEMU's monitor (qmp) socket for graceful shutdown
// AND forensic screenshot capture (Phase 6 Group C remaining).
//
// Screendump writes a PPM file at the supplied HOST path — qemu-system
// is a host process, so the path is interpreted on the host directly
// (no guest→host transfer step).
type qmpClient interface {
	Dial(ctx context.Context, port int, timeout time.Duration) error
	SystemPowerdown(ctx context.Context) error
	Screendump(ctx context.Context, hostPath string) error
	Close() error
}

// QEMUVM is the production VM impl.
type QEMUVM struct {
	procs        processRunner
	ssh          sshClient
	qmp          qmpClient
	kvmAvailable bool
	nextSSHPort  atomic.Int32 // starts at 10022
	nextMonPort  atomic.Int32 // starts at 14444

	authMu     sync.Mutex
	authedPort int // sshPort already authenticated; 0 = not authenticated
}

// NewQEMUVM constructs a production QEMUVM.
func NewQEMUVM() *QEMUVM {
	return newQEMUVMWithDeps(osProcessRunner{}, defaultSSHClient(), defaultQMPClient(), kvmAvailable())
}

func newQEMUVMWithDeps(p processRunner, s sshClient, q qmpClient, kvm bool) *QEMUVM {
	v := &QEMUVM{procs: p, ssh: s, qmp: q, kvmAvailable: kvm}
	v.nextSSHPort.Store(10022)
	v.nextMonPort.Store(14444)
	return v
}

func kvmAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

func qemuBinary(arch string) string {
	return "qemu-system-" + arch
}

// Boot assembles the QEMU command line and launches detached.
// Returns BootResult with allocated SSH + monitor ports.
//
// SAFETY: every call gets unique ports via the atomic counters.
// Concurrent Boot calls do NOT collide. This is the load-bearing
// safety property the falsifiability rehearsal targets — see
// TestQEMUVM_Boot_DistinctPortsAcrossInvocations.
//
// Per-arch policy:
//   - x86_64 + KVM available  → -enable-kvm + -cpu host
//   - x86_64 without KVM      → -cpu max (TCG)
//   - aarch64                 → -machine virt + -cpu max + AAVMF UEFI
//   - riscv64                 → -machine virt + -cpu max
func (v *QEMUVM) Boot(ctx context.Context, config VMConfig) (BootResult, error) {
	startedAt := time.Now()
	sshPort := int(v.nextSSHPort.Add(1) - 1)
	monPort := int(v.nextMonPort.Add(1) - 1)

	args := []string{
		"-name", config.Target.ID,
		"-m", "2048",
		"-smp", "2",
		"-nographic",
		"-no-reboot",
		"-drive", "file=" + config.QCowPath + ",if=virtio",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-monitor", fmt.Sprintf("tcp:127.0.0.1:%d,server,nowait", monPort),
		"-serial", "null",
	}

	switch config.Target.Arch {
	case "x86_64":
		if v.kvmAvailable {
			args = append(args, "-enable-kvm", "-cpu", "host")
		} else {
			args = append(args, "-cpu", "max")
		}
	case "aarch64":
		args = append(args, "-machine", "virt", "-cpu", "max", "-bios", "/usr/share/AAVMF/AAVMF_CODE.fd")
	case "riscv64":
		args = append(args, "-machine", "virt", "-cpu", "max")
	}

	if config.ColdBoot {
		args = append(args, "-snapshot")
	}

	binary := qemuBinary(config.Target.Arch)
	if err := v.procs.StartDetached(binary, args...); err != nil {
		return BootResult{
			Target:       config.Target,
			Started:      false,
			BootDuration: time.Since(startedAt),
			Error:        fmt.Errorf("qemu launch failed: %w", err),
		}, err
	}
	return BootResult{
		Target:       config.Target,
		Started:      true,
		SSHPort:      sshPort,
		MonitorPort:  monPort,
		BootDuration: time.Since(startedAt),
	}, nil
}

// WaitForReady polls a plain TCP probe of the SSH listener until it
// accepts. Bounded by timeout. Per I4 fix: this MUST be a
// listener-up-only check, NOT a full SSH handshake + userauth — the
// full path requires empty-password root which production guests
// reject, so a handshake-based readiness probe would always time out.
func (v *QEMUVM) WaitForReady(ctx context.Context, sshPort int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := v.ssh.WaitForListener(ctx, sshPort, 5*time.Second); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("vm on ssh port %d did not become ready within %s", sshPort, timeout)
}

// ensureAuthenticated performs the SSH handshake + userauth for sshPort
// exactly once per port before any guest interaction. The I4 split
// (see the sshClient interface doc) deliberately makes WaitForReady a
// listener-up-only probe; the actual authentication MUST happen here,
// before Upload/Run/Download, or every real guest call fails with
// "not authenticated". Re-authenticating on every op would re-dial and
// leak the prior *ssh.Client, so the authenticated port is cached and
// reset on Teardown.
func (v *QEMUVM) ensureAuthenticated(ctx context.Context, sshPort int) error {
	v.authMu.Lock()
	defer v.authMu.Unlock()
	if v.authedPort == sshPort {
		return nil
	}
	if err := v.ssh.Authenticate(ctx, sshPort, 30*time.Second); err != nil {
		return err
	}
	v.authedPort = sshPort
	return nil
}

func (v *QEMUVM) Upload(ctx context.Context, sshPort int, hostPath, vmPath string) error {
	if err := v.ensureAuthenticated(ctx, sshPort); err != nil {
		return fmt.Errorf("qemu upload: authenticate ssh port %d: %w", sshPort, err)
	}
	return v.ssh.Upload(ctx, hostPath, vmPath)
}

func (v *QEMUVM) Run(ctx context.Context, sshPort int, script string, env map[string]string, timeout time.Duration) (string, string, int, error) {
	if err := v.ensureAuthenticated(ctx, sshPort); err != nil {
		return "", "", -1, fmt.Errorf("qemu run: authenticate ssh port %d: %w", sshPort, err)
	}
	return v.ssh.Run(ctx, script, env, timeout)
}

func (v *QEMUVM) Download(ctx context.Context, sshPort int, vmPath, hostPath string) error {
	if err := v.ensureAuthenticated(ctx, sshPort); err != nil {
		return fmt.Errorf("qemu download: authenticate ssh port %d: %w", sshPort, err)
	}
	return v.ssh.Download(ctx, vmPath, hostPath)
}
