package vm

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeProcessRunner is the seam through which QEMUVM launches qemu-system-*.
// Production uses os/exec; tests substitute this.
type fakeProcessRunner struct {
	startedCmds [][]string
	startError  error
}

func (f *fakeProcessRunner) StartDetached(name string, args ...string) error {
	f.startedCmds = append(f.startedCmds, append([]string{name}, args...))
	return f.startError
}

// fakeSSHClient is the seam for SSH/SCP operations.
type fakeSSHClient struct {
	listenerError error // returned by WaitForListener (I4 — listener-up probe)
	authError     error // returned by Authenticate (I4 — handshake + userauth)
	uploaded      []UploadSpec
	uploadError   error
	runRequest    string
	// runScripts records every script Run() received, in order. The
	// matrix-runner integration tests assert against this so a tc-qdisc
	// invocation is observable end-to-end.
	runScripts     []string
	runStdout      string
	runStderr      string
	runExitCode    int
	runError       error
	downloaded     []CaptureSpec
	downloadError  error
	closedSessions int
}

func (f *fakeSSHClient) WaitForListener(_ context.Context, _ int, _ time.Duration) error {
	return f.listenerError
}
func (f *fakeSSHClient) Authenticate(_ context.Context, _ int, _ time.Duration) error {
	return f.authError
}
func (f *fakeSSHClient) Upload(_ context.Context, hostPath, vmPath string) error {
	f.uploaded = append(f.uploaded, UploadSpec{HostPath: hostPath, VMPath: vmPath})
	return f.uploadError
}
func (f *fakeSSHClient) Run(_ context.Context, script string, _ map[string]string, _ time.Duration) (string, string, int, error) {
	f.runRequest = script
	f.runScripts = append(f.runScripts, script)
	return f.runStdout, f.runStderr, f.runExitCode, f.runError
}
func (f *fakeSSHClient) Download(_ context.Context, vmPath, hostPath string) error {
	f.downloaded = append(f.downloaded, CaptureSpec{VMPath: vmPath, HostSubpath: hostPath})
	return f.downloadError
}
func (f *fakeSSHClient) Close() error { f.closedSessions++; return nil }

// fakeQMPClient is the seam for the QEMU monitor (graceful shutdown
// + Phase 6 forensic screendump).
type fakeQMPClient struct {
	dialError       error
	powerdownErr    error
	closed          bool
	screendumpErr   error
	screendumpPaths []string
	// screendumpFile, when non-nil, writes these bytes to the host
	// path the caller passes (so unit tests asserting "the file
	// exists with these bytes" don't need a real qemu).
	screendumpFile []byte
}

func (f *fakeQMPClient) Dial(_ context.Context, _ int, _ time.Duration) error { return f.dialError }
func (f *fakeQMPClient) SystemPowerdown(_ context.Context) error              { return f.powerdownErr }
func (f *fakeQMPClient) Screendump(_ context.Context, hostPath string) error {
	f.screendumpPaths = append(f.screendumpPaths, hostPath)
	if f.screendumpErr != nil {
		return f.screendumpErr
	}
	if f.screendumpFile != nil {
		// Mimic real qemu writing the PPM to the host path.
		// Caller has ensured the dir exists via os.MkdirAll.
		_ = os.WriteFile(hostPath, f.screendumpFile, 0o644)
	}
	return nil
}
func (f *fakeQMPClient) Close() error { f.closed = true; return nil }

func TestQEMUVM_Boot_AssemblesCorrectArgsForX86_64KVM(t *testing.T) {
	r := &fakeProcessRunner{}
	v := newQEMUVMWithDeps(r, nil, nil, true /* kvmAvailable */)
	cfg := VMConfig{
		Target:   VMTarget{ID: "alpine-x86_64", Arch: "x86_64", Distro: "alpine"},
		QCowPath: "/tmp/alpine-x86_64.qcow2",
		ColdBoot: true,
	}
	got, err := v.Boot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
	if !got.Started {
		t.Fatalf("BootResult.Started false")
	}
	if got.SSHPort == 0 || got.MonitorPort == 0 {
		t.Fatalf("Boot did not assign ports: %+v", got)
	}
	if len(r.startedCmds) != 1 {
		t.Fatalf("expected 1 process started, got %d", len(r.startedCmds))
	}
	cmd := r.startedCmds[0]
	if !strings.Contains(cmd[0], "qemu-system-x86_64") {
		t.Fatalf("expected qemu-system-x86_64 binary, got %s", cmd[0])
	}
	full := strings.Join(cmd, " ")
	if !strings.Contains(full, "-enable-kvm") {
		t.Fatalf("expected -enable-kvm on x86_64 with KVM available; cmd=%s", full)
	}
	if !strings.Contains(full, "-snapshot") {
		t.Fatalf("expected -snapshot for ColdBoot=true; cmd=%s", full)
	}
}

func TestQEMUVM_Boot_FallsBackToTCGOnAARCH64(t *testing.T) {
	r := &fakeProcessRunner{}
	v := newQEMUVMWithDeps(r, nil, nil, true /* kvmAvailable, but irrelevant */)
	cfg := VMConfig{
		Target:   VMTarget{ID: "alpine-aarch64", Arch: "aarch64"},
		QCowPath: "/tmp/aarch64.qcow2",
	}
	if _, err := v.Boot(context.Background(), cfg); err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
	cmd := strings.Join(r.startedCmds[0], " ")
	if !strings.Contains(cmd, "qemu-system-aarch64") {
		t.Fatalf("expected qemu-system-aarch64; cmd=%s", cmd)
	}
	if strings.Contains(cmd, "-enable-kvm") {
		t.Fatalf("aarch64 cross-arch must NOT use KVM; cmd=%s", cmd)
	}
	if !strings.Contains(cmd, "-machine virt") {
		t.Fatalf("expected -machine virt for aarch64; cmd=%s", cmd)
	}
}

func TestQEMUVM_Boot_DistinctPortsAcrossInvocations(t *testing.T) {
	r := &fakeProcessRunner{}
	v := newQEMUVMWithDeps(r, nil, nil, true)
	cfg := VMConfig{Target: VMTarget{ID: "x", Arch: "x86_64"}, QCowPath: "/tmp/x"}
	a, _ := v.Boot(context.Background(), cfg)
	b, _ := v.Boot(context.Background(), cfg)
	if a.SSHPort == b.SSHPort {
		t.Fatalf("two Boots got same SSH port (%d) — port-allocator broken", a.SSHPort)
	}
	if a.MonitorPort == b.MonitorPort {
		t.Fatalf("two Boots got same monitor port (%d)", a.MonitorPort)
	}
}

func TestQEMUVM_WaitForReady_PollsListenerUntilTimeout(t *testing.T) {
	// I4 fix: WaitForReady now polls WaitForListener (TCP probe only)
	// instead of Dial (TCP + SSH handshake + empty-password userauth).
	// Production guests reject empty-password root, so the handshake-
	// based probe would have always timed out — this test now reflects
	// the intended listener-up semantics.
	ssh := &fakeSSHClient{listenerError: errors.New("connection refused")}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, nil, true)
	err := v.WaitForReady(context.Background(), 10022, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected 'did not become ready' in error, got: %v", err)
	}
}

func TestQEMUVM_Upload_Run_Download(t *testing.T) {
	ssh := &fakeSSHClient{
		runStdout:   "hello",
		runStderr:   "",
		runExitCode: 0,
	}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, nil, true)
	if err := v.Upload(context.Background(), 10022, "/tmp/h", "/tmp/v"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if ssh.uploaded[0].HostPath != "/tmp/h" || ssh.uploaded[0].VMPath != "/tmp/v" {
		t.Fatalf("upload args wrong: %+v", ssh.uploaded)
	}
	stdout, _, exitCode, err := v.Run(context.Background(), 10022, "echo hello", nil, time.Second)
	if err != nil || exitCode != 0 || stdout != "hello" {
		t.Fatalf("Run: stdout=%q exit=%d err=%v", stdout, exitCode, err)
	}
	if err := v.Download(context.Background(), 10022, "/tmp/v", "/tmp/h"); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// --- Teardown tests live alongside qemu_test.go because they share the
//     fakeSSHClient + fakeQMPClient + fakeProcessRunner seams. Mirror of
//     pkg/emulator's Teardown test pattern (Group B).

func TestTeardown_FastPath_SkipsOnMismatch(t *testing.T) {
	prev := killByPortHook
	killByPortHook = func(_ context.Context, _ int) (KillReport, error) {
		return KillReport{Matched: 0}, nil
	}
	defer func() { killByPortHook = prev }()
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 200 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	ssh := &fakeSSHClient{}
	qmp := &fakeQMPClient{powerdownErr: errors.New("qmp connect refused")}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, qmp, true)

	err := v.Teardown(context.Background(), 14444, 10022)
	if err == nil {
		t.Fatalf("expected Teardown to error when QMP fails AND KillByPort.Matched==0, got nil")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("expected error to mention 'did not exit', got: %v", err)
	}
}

func TestTeardown_FastPath_SucceedsAfterKillByPort(t *testing.T) {
	prev := killByPortHook
	killByPortHook = func(_ context.Context, _ int) (KillReport, error) {
		return KillReport{Matched: 1, Sigtermed: []int{12345}}, nil
	}
	defer func() { killByPortHook = prev }()
	prevGrace := teardownGracePeriod
	teardownGracePeriod = 200 * time.Millisecond
	defer func() { teardownGracePeriod = prevGrace }()

	ssh := &fakeSSHClient{}
	qmp := &fakeQMPClient{powerdownErr: errors.New("qmp connect refused")}
	v := newQEMUVMWithDeps(&fakeProcessRunner{}, ssh, qmp, true)

	if err := v.Teardown(context.Background(), 14444, 10022); err != nil {
		t.Fatalf("expected Teardown to succeed after KillByPort cleared the stuck VM, got: %v", err)
	}
}
