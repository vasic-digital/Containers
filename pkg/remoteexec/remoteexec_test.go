package remoteexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic command-construction tests (run everywhere, no systemd needed).
// These guard the load-bearing anti-bluff property: the launch command MUST use
// linger + systemd-run --user, and MUST NOT use any session-bound mechanism
// (nohup/tmux/setsid) or a buffering `tail` pipe.
// ---------------------------------------------------------------------------

func TestBuildLaunchCommand_UsesLingerAndSystemdRun(t *testing.T) {
	h := handleForDir("qa-matrix", "/abs/cache/remoteexec")
	cmd := buildLaunchCommand(h, "/abs/cache/remoteexec", true)

	mustContain := []string{
		"export XDG_RUNTIME_DIR=/run/user/$(id -u)",
		"loginctl enable-linger",
		"systemd-run --user --unit='qa-matrix'",
		"--collect",
		"bash '/abs/cache/remoteexec/qa-matrix.runner.sh'",
	}
	for _, s := range mustContain {
		if !strings.Contains(cmd, s) {
			t.Errorf("launch command missing %q\ngot: %s", s, cmd)
		}
	}

	// A nohup/tmux/setsid launch dies with the SSH session (the whole bug).
	// A `tail` pipe buffers output until exit (the diagnostic trap). None may
	// appear — their presence would mean we reintroduced the bug.
	mustNotContain := []string{"nohup", "tmux", "setsid", "disown", "| tail", "|tail"}
	for _, s := range mustNotContain {
		if strings.Contains(cmd, s) {
			t.Errorf("launch command must NOT contain %q (session-bound / buffering)\ngot: %s", s, cmd)
		}
	}
}

func TestBuildLaunchCommand_LingerOffOmitsEnableLinger(t *testing.T) {
	h := handleForDir("u", "/d")
	if got := buildLaunchCommand(h, "/d", false); strings.Contains(got, "enable-linger") {
		t.Errorf("linger disabled but command still enables it: %s", got)
	}
}

func TestBuildRunnerScript_CapturesLogAndWritesSentinel(t *testing.T) {
	h := handleForDir("u", "/d")
	body := "echo hello; do_work --flag"
	got := buildRunnerScript(h, body)

	for _, s := range []string{
		body,
		`>"$__log" 2>&1`,
		`> "$__sentinel"`,
		"__rc=$?",
		"exit $__rc",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("runner script missing %q\ngot:\n%s", s, got)
		}
	}
	if strings.Contains(got, "tail ") || strings.Contains(got, "| tail") {
		t.Errorf("runner script must not pipe through tail (buffers): %s", got)
	}
}

func TestWaitForSentinel_TimesOutWhenNeverWritten(t *testing.T) {
	r := NewLocalRunner(nil)
	h := handleForDir("never", t.TempDir())
	_, err := WaitForSentinel(context.Background(), r, h, 50*time.Millisecond, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error when sentinel never appears, got nil")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("expected 'did not appear' timeout error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Real durable-execution test (the anti-bluff load-bearer).
// ---------------------------------------------------------------------------

// hasSystemdUser reports whether a usable systemd --user manager is reachable.
func hasSystemdUser(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	out, _ := exec.Command("bash", "-c",
		`export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user is-system-running 2>&1`).CombinedOutput()
	s := strings.TrimSpace(string(out))
	// "running" or "degraded" both mean the user manager is up; "offline"/
	// "Failed to connect" mean no usable user bus.
	return s == "running" || s == "degraded"
}

func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	// Prefer the unified (0::) line; fall back to the last line.
	var last string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		last = line
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::")
		}
	}
	return last
}

// TestLaunch_DurableSurvivesLauncher launches a sleeper via the helper and
// proves it KEEPS RUNNING under the user systemd manager after the launching
// shell (the bash -c that ran systemd-run) has already exited — and that it
// runs to completion, producing real on-disk output. This is the exact
// property that nohup/tmux/setsid fail to provide (see the falsifiability
// contrast in TestDurabilityPredicate_SystemdRunVsNohup, and the README RED
// rehearsal that mutates buildLaunchCommand to nohup).
func TestLaunch_DurableSurvivesLauncher(t *testing.T) {
	if !hasSystemdUser(t) {
		t.Skip("no systemd --user manager available (honest skip: durability requires systemd-run --user)")
	}
	r := NewLocalRunner(nil)
	unit := fmt.Sprintf("remoteexec-it-%d", time.Now().UnixNano())
	spec := Spec{
		Unit:   unit,
		Dir:    t.TempDir(),
		Script: "echo START_MARKER; sleep 6; echo DONE_MARKER",
	}

	h, err := Launch(context.Background(), r, spec)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer func() { _ = Stop(context.Background(), r, h) }()

	// The launching shell (LocalRunner's bash -c) has already returned. The job
	// must nonetheless be active under the user manager.
	active := false
	for i := 0; i < 30; i++ {
		if a, _ := IsActive(context.Background(), r, unit); a {
			active = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !active {
		t.Fatalf("unit %s is not active after launch — the job did not survive the launcher", unit)
	}

	// Survival proof at the cgroup layer: the job lives in its OWN .service
	// unit owned by the user manager, NOT in this test process's session scope.
	pid, err := MainPID(context.Background(), r, unit)
	if err != nil || pid <= 0 {
		t.Fatalf("could not read MainPID for %s: pid=%d err=%v", unit, pid, err)
	}
	jobCgroup := cgroupOf(t, pid)
	selfCgroup := cgroupOf(t, os.Getpid())
	if !strings.Contains(jobCgroup, "/"+unit+".service") {
		t.Errorf("job cgroup is not an independently-managed .service unit: %q", jobCgroup)
	}
	if jobCgroup == selfCgroup {
		t.Errorf("job shares this process's cgroup (%q) — it would be reaped with the launcher", jobCgroup)
	}
	t.Logf("job cgroup:  %s", jobCgroup)
	t.Logf("self cgroup: %s", selfCgroup)

	// It runs to completion AFTER the launcher exited, producing real output.
	rc, err := WaitForSentinel(context.Background(), r, h, 300*time.Millisecond, 25*time.Second)
	if err != nil {
		t.Fatalf("WaitForSentinel: %v", err)
	}
	if rc != 0 {
		t.Errorf("durable job exit code = %d, want 0", rc)
	}
	log, err := FetchLog(context.Background(), r, h)
	if err != nil {
		t.Fatalf("FetchLog: %v", err)
	}
	if !strings.Contains(log, "START_MARKER") || !strings.Contains(log, "DONE_MARKER") {
		t.Errorf("durable job log missing start/done markers (job did not run to completion):\n%s", log)
	}
}

// TestDurabilityPredicate_SystemdRunVsNohup is the in-suite falsifiability
// contrast: the systemd-run job satisfies the durability predicate
// (independently-managed .service unit) while the plain nohup baseline does NOT
// (it stays in the launcher's session scope and is reaped with it). If Launch
// were re-implemented with nohup, the durable assertion above would fail.
func TestDurabilityPredicate_SystemdRunVsNohup(t *testing.T) {
	if !hasSystemdUser(t) {
		t.Skip("no systemd --user manager available (honest skip)")
	}
	r := NewLocalRunner(nil)

	// Durable path.
	unit := fmt.Sprintf("remoteexec-pred-%d", time.Now().UnixNano())
	h, err := Launch(context.Background(), r, Spec{Unit: unit, Dir: t.TempDir(), Script: "sleep 5"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = Stop(context.Background(), r, h) }()
	for i := 0; i < 30; i++ {
		if a, _ := IsActive(context.Background(), r, unit); a {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	pid, _ := MainPID(context.Background(), r, unit)
	durableCgroup := cgroupOf(t, pid)
	if !isIndependentlyManaged(durableCgroup) {
		t.Errorf("systemd-run job NOT independently managed: %q", durableCgroup)
	}

	// nohup baseline (the OLD approach). Launch a sleeper, capture its pid.
	res, err := r.Run(context.Background(),
		"nohup bash -c 'sleep 5' >/dev/null 2>&1 & echo $!")
	if err != nil {
		t.Fatalf("nohup baseline launch: %v", err)
	}
	var nohupPID int
	_, _ = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &nohupPID)
	if nohupPID <= 0 {
		t.Fatalf("could not capture nohup pid from %q", res.Stdout)
	}
	defer func() { _, _ = r.Run(context.Background(), fmt.Sprintf("kill %d 2>/dev/null || true", nohupPID)) }()
	nohupCgroup := cgroupOf(t, nohupPID)

	if isIndependentlyManaged(nohupCgroup) {
		t.Errorf("nohup baseline unexpectedly looks independently managed: %q "+
			"(test cannot distinguish durable from non-durable — would be a bluff)", nohupCgroup)
	}
	t.Logf("durable (systemd-run) cgroup: %s  -> independently managed: %v",
		durableCgroup, isIndependentlyManaged(durableCgroup))
	t.Logf("baseline (nohup)        cgroup: %s  -> independently managed: %v",
		nohupCgroup, isIndependentlyManaged(nohupCgroup))
}

// isIndependentlyManaged reports whether a cgroup path is a transient user
// .service unit (owned by the user manager, survives the session) rather than a
// session/login .scope (reaped when the session ends).
func isIndependentlyManaged(cgroup string) bool {
	leaf := cgroup
	if i := strings.LastIndex(cgroup, "/"); i >= 0 {
		leaf = cgroup[i+1:]
	}
	return strings.HasSuffix(leaf, ".service")
}
