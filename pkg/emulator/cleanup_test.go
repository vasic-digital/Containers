package emulator

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProcWalker struct {
	pids     map[int]string
	cmdlines map[int][]string // pid -> argv (for the §11.4.174 -avd ownership gate)
	err      error
}

func (f fakeProcWalker) PidComms() (map[int]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pids, nil
}

// PidCmdlines serves the argv fixtures the §11.4.174 ownership gate matches
// against. Error is propagated symmetrically with PidComms.
func (f fakeProcWalker) PidCmdlines() (map[int][]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cmdlines, nil
}

type fakeKiller struct {
	sent       map[int][]syscall.Signal
	aliveAfter map[syscall.Signal]map[int]bool
}

func newFakeKiller() *fakeKiller {
	return &fakeKiller{
		sent: map[int][]syscall.Signal{},
		aliveAfter: map[syscall.Signal]map[int]bool{
			syscall.SIGTERM: {},
		},
	}
}

func (f *fakeKiller) Signal(pid int, sig syscall.Signal) error {
	f.sent[pid] = append(f.sent[pid], sig)
	return nil
}

func (f *fakeKiller) Exists(pid int) bool {
	// Note: cleanupWithDeps calls Exists only during the SIGTERM
	// grace-window poll, never after SIGKILL — so the post-SIGKILL
	// branch from a prior implementation was dead code and has been
	// removed. The Surviving-after-SIGKILL path is controlled
	// instead by fakeKiller.Signal() return values; today Signal()
	// always returns nil so Surviving is structurally untestable
	// without further fake extension.
	sent := f.sent[pid]
	for _, s := range sent {
		if s == syscall.SIGKILL {
			return false
		}
		if s == syscall.SIGTERM {
			return f.aliveAfter[syscall.SIGTERM][pid]
		}
	}
	return true // never signalled — alive
}

// TestCleanup_NoMatches confirms an empty /proc state returns an empty
// report and sends no signals. Falsifiability: change the prefix
// matcher to "" → all PIDs would be Found and signalled. Test fails.
func TestCleanup_NoMatches(t *testing.T) {
	w := fakeProcWalker{pids: map[int]string{
		1234: "bash",
		5678: "node",
		9999: "java",
	}}
	k := newFakeKiller()

	report, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.NoError(t, err)
	assert.Empty(t, report.Found)
	assert.Empty(t, report.TerminatedTERM)
	assert.Empty(t, report.KilledKILL)
	assert.Empty(t, report.Surviving)
	assert.Empty(t, k.sent)
}

// TestCleanup_OneMatch_TerminatesOnSIGTERM confirms the happy path:
// one qemu-system PID is found, SIGTERM is sent, the PID exits within
// the grace window (fakeKiller.Exists returns false after SIGTERM by
// default), no SIGKILL needed.
func TestCleanup_OneMatch_TerminatesOnSIGTERM(t *testing.T) {
	w := fakeProcWalker{
		pids: map[int]string{
			1234: "bash",
			7777: "qemu-system-x86_64",
		},
		cmdlines: map[int][]string{
			7777: {"qemu-system-x86_64", "-avd", "helix_api_31", "-port", "5554"},
		},
	}
	k := newFakeKiller()

	report, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.NoError(t, err)
	assert.Equal(t, []int{7777}, report.Found)
	assert.Equal(t, []int{7777}, report.TerminatedTERM)
	assert.Empty(t, report.KilledKILL)
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM}, k.sent[7777])
}

// TestCleanup_StrictPrefix is the falsifiability-rehearsal test for
// the prefix-matcher. Synthetic /proc contains "qemu-img" and "qemu"
// (NOT qemu-system processes). The strict prefix "qemu-system-" must
// NOT match them.
//
// Mutation: loosen prefix to "qemu-" → this test fails because PID
//
//	8888 (qemu-img) is now in Found.
//
// Reverted: yes.
func TestCleanup_StrictPrefix(t *testing.T) {
	w := fakeProcWalker{
		pids: map[int]string{
			7777: "qemu-system-x86_64", // legitimate match (OUR avd)
			8888: "qemu-img",           // NOT a qemu-system process
			9999: "qemu",               // NOT a qemu-system process
		},
		cmdlines: map[int][]string{
			7777: {"qemu-system-x86_64", "-avd", "helix_api_31"},
			8888: {"qemu-img", "-avd", "helix_api_31"}, // carries token but fails comm prefix
			9999: {"qemu", "-avd", "helix_api_31"},
		},
	}
	k := newFakeKiller()

	report, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.NoError(t, err)
	assert.Equal(t, []int{7777}, report.Found,
		"STRICT prefix qemu-system- MUST NOT match qemu-img or qemu (even with the -avd token)")
	assert.Empty(t, k.sent[8888])
	assert.Empty(t, k.sent[9999])
}

// TestCleanup_OwnershipScopedToOurAVD is the §11.4.174 ownership guard
// (§11.4.115 polarity via RED_MODE):
//
//	RED_MODE unset/1 (default) = GREEN guard on the FIXED code: ONLY the
//	  qemu running OUR avd is reaped; a foreign qemu (different avd) gets NO
//	  signal.
//	RED_MODE=0 = reproduce the pre-fix comm-only defect (asserts BOTH the
//	  ours + the foreign qemu are Found). This MUST FAIL on the fixed code
//	  (which finds only ours), proving the GREEN flip comes from the fix;
//	  it PASSES on the pre-fix comm-only matcher (which reaps both).
func TestCleanup_OwnershipScopedToOurAVD(t *testing.T) {
	redMode := os.Getenv("RED_MODE") != "0"
	w := fakeProcWalker{
		pids: map[int]string{
			701: "qemu-system-x86_64", // OURS
			702: "qemu-system-x86_64", // FOREIGN (different avd)
		},
		cmdlines: map[int][]string{
			701: {"qemu-system-x86_64", "-avd", "helix_api_31", "-port", "5554"},
			702: {"qemu-system-x86_64", "-avd", "someone_else", "-port", "5556"},
		},
	}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.NoError(t, err)
	if redMode {
		assert.ElementsMatch(t, []int{701}, report.Found,
			"only the qemu running OUR avd may be reaped (§11.4.174)")
		assert.Empty(t, k.sent[702], "foreign qemu (different avd) MUST receive NO signal")
	} else {
		assert.ElementsMatch(t, []int{701, 702}, report.Found,
			"RED: a comm-only matcher would reap the foreign qemu too")
	}
}

// TestCleanup_EmptyAVDRefusesReap is the §11.4.174 refuse-guard: with no
// ownership token, Cleanup reaps NOTHING even when qemu-system-* PIDs are
// present — it never falls back to a host-wide name match.
func TestCleanup_EmptyAVDRefusesReap(t *testing.T) {
	w := fakeProcWalker{
		pids: map[int]string{999: "qemu-system-x86_64"},
		cmdlines: map[int][]string{
			999: {"qemu-system-x86_64", "-avd", "whatever"},
		},
	}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "", w, k)
	require.NoError(t, err)
	assert.Empty(t, report.Found, "empty avdName MUST refuse to reap (§11.4.174)")
	assert.Empty(t, k.sent, "no signal may be sent when ownership cannot be established")
}

// TestCleanup_PropagatesProcReadErr confirms procWalker errors surface
// to the caller (we don't silently swallow /proc read failures).
func TestCleanup_PropagatesProcReadErr(t *testing.T) {
	w := fakeProcWalker{err: errors.New("permission denied")}
	k := newFakeKiller()

	_, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

// TestCleanup_StragglerRequiresSIGKILL exercises the path where a
// qemu-system PID survives the SIGTERM grace window and requires
// SIGKILL. Without this test, a regression that removed the SIGKILL
// block would not be caught.
//
// Falsifiability: comment out the `for _, pid := range stragglers {
// k.Signal(pid, syscall.SIGKILL) ... }` block in cleanupWithDeps.
// Test fails: report.KilledKILL is empty when it should be []int{7777}.
// Reverted: yes.
func TestCleanup_StragglerRequiresSIGKILL(t *testing.T) {
	w := fakeProcWalker{
		pids: map[int]string{
			7777: "qemu-system-x86_64",
		},
		cmdlines: map[int][]string{
			7777: {"qemu-system-x86_64", "-avd", "helix_api_31"},
		},
	}
	k := newFakeKiller()
	k.aliveAfter[syscall.SIGTERM][7777] = true // survives SIGTERM grace window

	// Use a context that bounds the test runtime (the production
	// poll loop runs for up to 5 real seconds; we want the fake to
	// short-circuit faster). The fake's Exists() returns true for
	// 7777 throughout the SIGTERM-poll window, so the loop exhausts
	// its 5-second deadline naturally. Acceptable for a unit test.
	report, err := cleanupWithDeps(context.Background(), "helix_api_31", w, k)
	require.NoError(t, err)
	assert.Equal(t, []int{7777}, report.Found)
	assert.Empty(t, report.TerminatedTERM,
		"PID survived SIGTERM grace window, MUST NOT be in TerminatedTERM")
	assert.Equal(t, []int{7777}, report.KilledKILL,
		"PID surviving SIGTERM MUST be SIGKILLed and recorded in KilledKILL")
	assert.Empty(t, report.Surviving)
	// Verify both signals were sent in the right order
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}, k.sent[7777],
		"SIGTERM MUST come before SIGKILL")
}

// ---------------------------------------------------------------------
// KillByPort tests — Group B clause 6.I extension
//
// Forensic anchor: the matrix-runner Teardown's `adb emu kill` retains
// its 30s grace, but stuck QEMU instances persist past it (clause
// 6.M-recorded behavior). Without a port-strict force-kill fast-path,
// the next iteration's Boot lands on 5556/5557 and the matrix
// accumulates concurrent emulators — observed flakes in the API 35 row
// of the 5-AVD matrix.
//
// SAFETY contract for KillByPort (the tests below verify each clause):
//   - Strict adjacent token match — substring `25554` must NOT match port 5554
//   - No-op on mismatch — concurrent emulators on other ports untouched
//   - SIGKILL only after 5s grace — graceful exit honored first
// ---------------------------------------------------------------------

type fakeProcWalkerWithCmdlines struct {
	cmdlines map[int][]string // pid → argv
}

func (f fakeProcWalkerWithCmdlines) PidComms() (map[int]string, error) {
	out := make(map[int]string)
	for pid, argv := range f.cmdlines {
		if len(argv) > 0 {
			out[pid] = argv[0]
		} else {
			out[pid] = ""
		}
	}
	return out, nil
}

func (f fakeProcWalkerWithCmdlines) PidCmdlines() (map[int][]string, error) {
	return f.cmdlines, nil
}

type fakeKillerByPort struct {
	signaled   map[int][]syscall.Signal
	aliveAfter map[syscall.Signal]map[int]bool // post-signal aliveness override
}

func newFakeKillerByPort() *fakeKillerByPort {
	return &fakeKillerByPort{
		signaled:   make(map[int][]syscall.Signal),
		aliveAfter: make(map[syscall.Signal]map[int]bool),
	}
}

func (f *fakeKillerByPort) Signal(pid int, sig syscall.Signal) error {
	f.signaled[pid] = append(f.signaled[pid], sig)
	return nil
}

func (f *fakeKillerByPort) Exists(pid int) bool {
	// Default: SIGTERM clears the process; SIGKILL clears it.
	// Tests override aliveAfter[sig][pid] = true to keep the process
	// alive past the corresponding signal (forces SIGKILL path).
	signals, ok := f.signaled[pid]
	if !ok {
		return true // never signaled → still alive
	}
	last := signals[len(signals)-1]
	if alive, override := f.aliveAfter[last][pid]; override {
		return alive
	}
	return false
}

func TestKillByPort_NoMatch_NoOp(t *testing.T) {
	w := fakeProcWalkerWithCmdlines{cmdlines: map[int][]string{
		1234: {"qemu-system-x86_64", "-avd", "Pixel_9a", "-port", "5556"},
		5678: {"chrome", "--user-data-dir=/tmp/x"},
	}}
	k := newFakeKillerByPort()
	report, err := killByPortWithDeps(context.Background(), 5554, w, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Matched != 0 {
		t.Fatalf("expected Matched=0 (no proc has -port 5554 adjacent), got %d", report.Matched)
	}
	if len(k.signaled) != 0 {
		t.Fatalf("expected no kill signals issued, got %v", k.signaled)
	}
}

func TestKillByPort_StrictAdjacentMatch(t *testing.T) {
	w := fakeProcWalkerWithCmdlines{cmdlines: map[int][]string{
		1111: {"qemu-system-x86_64", "-avd", "A1", "-port", "5554"}, // MATCH
		2222: {"qemu-system-x86_64", "-avd", "A2", "-port", "5556"}, // no match
	}}
	k := newFakeKillerByPort()
	report, err := killByPortWithDeps(context.Background(), 5554, w, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Matched != 1 {
		t.Fatalf("expected Matched=1, got %d", report.Matched)
	}
	if len(report.Sigtermed) != 1 || report.Sigtermed[0] != 1111 {
		t.Fatalf("expected Sigtermed=[1111], got %v", report.Sigtermed)
	}
	if _, signaled := k.signaled[2222]; signaled {
		t.Fatalf("pid 2222 was signaled despite different port — safety violation")
	}
}

func TestKillByPort_SubstringSafety(t *testing.T) {
	// pid 9999 has the literal string "5554" inside its argv but NOT
	// adjacent to "-port". KillByPort(5554) MUST NOT match it.
	w := fakeProcWalkerWithCmdlines{cmdlines: map[int][]string{
		9999: {"qemu-system-x86_64", "-avd", "A1", "-port", "25554"}, // 25554 ≠ 5554
		8888: {"qemu-system-x86_64", "-avd", "A2", "-pidfile", "/tmp/5554.pid"},
	}}
	k := newFakeKillerByPort()
	report, err := killByPortWithDeps(context.Background(), 5554, w, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Matched != 0 {
		t.Fatalf("expected Matched=0 (no adjacent token pair), got %d (signaled=%v)",
			report.Matched, k.signaled)
	}
}

func TestKillByPort_RequiresSIGKILL_AfterGrace(t *testing.T) {
	w := fakeProcWalkerWithCmdlines{cmdlines: map[int][]string{
		7777: {"qemu-system-x86_64", "-avd", "A1", "-port", "5554"},
	}}
	k := newFakeKillerByPort()
	// Make pid 7777 survive SIGTERM — forces the SIGKILL grace path.
	// After SIGKILL, default Exists() returns false, so the process
	// is reported as Sigkilled, not Surviving.
	k.aliveAfter[syscall.SIGTERM] = map[int]bool{7777: true}
	report, err := killByPortWithDeps(context.Background(), 5554, w, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Matched != 1 {
		t.Fatalf("expected Matched=1, got %d", report.Matched)
	}
	if len(report.Sigkilled) != 1 || report.Sigkilled[0] != 7777 {
		t.Fatalf("expected Sigkilled=[7777], got %v", report.Sigkilled)
	}
}
