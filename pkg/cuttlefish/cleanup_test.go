package cuttlefish

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWalker returns a fixed pid→comm map (and pid→argv for the §11.4.174
// ownership gate).
type fakeWalker struct {
	comms    map[int]string
	cmdlines map[int][]string
	err      error
}

func (f fakeWalker) PidComms() (map[int]string, error)      { return f.comms, f.err }
func (f fakeWalker) PidCmdlines() (map[int][]string, error) { return f.cmdlines, f.err }

// fakeKiller records signals and simulates process liveness. A pid is
// "alive" until it receives a signal in deadAfterSignal (then Exists
// returns false), modelling a graceful SIGTERM exit. pidsImmortal stay
// alive through SIGTERM (forcing the SIGKILL path).
type fakeKiller struct {
	mu         sync.Mutex
	signals    map[int][]syscall.Signal
	immortal   map[int]bool // survive SIGTERM (require SIGKILL)
	killProof  map[int]bool // SIGKILL succeeds (default true)
	gotSIGTERM map[int]bool
}

func newFakeKiller() *fakeKiller {
	return &fakeKiller{
		signals:    map[int][]syscall.Signal{},
		immortal:   map[int]bool{},
		killProof:  map[int]bool{},
		gotSIGTERM: map[int]bool{},
	}
}

func (k *fakeKiller) Signal(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.signals[pid] = append(k.signals[pid], sig)
	if sig == syscall.SIGTERM {
		k.gotSIGTERM[pid] = true
	}
	if sig == syscall.SIGKILL {
		if proof, ok := k.killProof[pid]; ok && !proof {
			return assertSignalErr
		}
	}
	return nil
}

func (k *fakeKiller) Exists(pid int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	// Immortal pids stay alive until SIGKILL.
	if k.immortal[pid] {
		for _, s := range k.signals[pid] {
			if s == syscall.SIGKILL {
				return false
			}
		}
		return true
	}
	// Mortal pids die on SIGTERM.
	return !k.gotSIGTERM[pid]
}

var assertSignalErr = &signalError{}

type signalError struct{}

func (*signalError) Error() string { return "no such process" }

// TestCleanup_ExactNameMatch is the falsifiability target for the exact
// comm allowlist: only crosvm / run_cvd / cvd_internal_start are collected;
// an unrelated process whose name shares a prefix is NOT.
func TestCleanup_ExactNameMatch(t *testing.T) {
	w := fakeWalker{
		comms: map[int]string{
			101: "crosvm",             // matched (VMM, OUR instance)
			102: "run_cvd",            // matched (supervisor, OUR instance)
			103: "cvd_internal_start", // matched (supervisor, OUR instance)
			104: "crontab",            // NOT matched (shares "cr" prefix)
			105: "crosvm-helper",      // NOT matched (exact-match, not prefix)
			106: "qemu-system-x86_64", // NOT matched (that is pkg/emulator's job)
		},
		cmdlines: map[int][]string{
			101: {"crosvm", "--base_instance_num=1"},
			102: {"run_cvd", "--base_instance_num=1"},
			103: {"cvd_internal_start", "--base_instance_num=1"},
			104: {"crontab", "--base_instance_num=1"}, // token present but comm fails allowlist
			105: {"crosvm-helper", "--base_instance_num=1"},
			106: {"qemu-system-x86_64", "--base_instance_num=1"},
		},
	}
	k := newFakeKiller()

	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{101, 102, 103}, report.Found,
		"only exact Cuttlefish daemon names may be reaped; got %v", report.Found)
	// The unrelated processes received NO signal.
	assert.Empty(t, k.signals[104])
	assert.Empty(t, k.signals[105])
	assert.Empty(t, k.signals[106])
}

func TestCleanup_SIGTERMGracefulExit(t *testing.T) {
	w := fakeWalker{
		comms:    map[int]string{201: "crosvm", 202: "run_cvd"},
		cmdlines: map[int][]string{201: {"crosvm", "--base_instance_num=1"}, 202: {"run_cvd", "--base_instance_num=1"}},
	}
	k := newFakeKiller() // both mortal — die on SIGTERM

	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{201, 202}, report.TerminatedTERM)
	assert.Empty(t, report.KilledKILL)
	assert.Contains(t, k.signals[201], syscall.SIGTERM)
}

func TestCleanup_SIGKILLStraggler(t *testing.T) {
	w := fakeWalker{
		comms:    map[int]string{301: "crosvm"},
		cmdlines: map[int][]string{301: {"crosvm", "--base_instance_num=1"}},
	}
	k := newFakeKiller()
	k.immortal[301] = true // survives SIGTERM → requires SIGKILL

	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{301}, report.KilledKILL)
	assert.Contains(t, k.signals[301], syscall.SIGTERM)
	assert.Contains(t, k.signals[301], syscall.SIGKILL)
}

func TestCleanup_NothingFound(t *testing.T) {
	w := fakeWalker{comms: map[int]string{401: "bash", 402: "sshd"}}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	assert.Empty(t, report.Found)
	assert.Empty(t, k.signals)
}

func TestCleanup_EmptyCommSkipped(t *testing.T) {
	w := fakeWalker{comms: map[int]string{501: ""}}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{501}, report.SkippedReadErr)
	assert.Empty(t, report.Found)
}

// TestCleanup_OwnershipScopedToOurInstance is the §11.4.174 ownership guard
// (§11.4.115 polarity via RED_MODE):
//
//	RED_MODE unset/1 (default) = GREEN guard on the FIXED code: ONLY cvd
//	  processes carrying OUR instance token are reaped; a foreign instance's
//	  crosvm/run_cvd (different token) gets NO signal.
//	RED_MODE=0 = reproduce the pre-fix comm-only defect (asserts the foreign
//	  cvd is Found too). MUST FAIL on the fixed code; PASSES on the pre-fix
//	  comm-only matcher.
func TestCleanup_OwnershipScopedToOurInstance(t *testing.T) {
	redMode := os.Getenv("RED_MODE") != "0"
	w := fakeWalker{
		comms: map[int]string{
			801: "crosvm",  // OURS    (--base_instance_num=1)
			802: "crosvm",  // FOREIGN (--base_instance_num=2)
			803: "run_cvd", // OURS
		},
		cmdlines: map[int][]string{
			801: {"crosvm", "--base_instance_num=1"},
			802: {"crosvm", "--base_instance_num=2"},
			803: {"run_cvd", "--base_instance_num=1"},
		},
	}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "--base_instance_num=1", w, k)
	require.NoError(t, err)
	if redMode {
		assert.ElementsMatch(t, []int{801, 803}, report.Found,
			"only cvd carrying OUR instance token may be reaped (§11.4.174)")
		assert.Empty(t, k.signals[802], "foreign cvd instance MUST receive NO signal")
	} else {
		assert.ElementsMatch(t, []int{801, 802, 803}, report.Found,
			"RED: a comm-only matcher would reap the foreign cvd too")
	}
}

// TestCleanup_EmptyTokenRefusesReap is the §11.4.174 refuse-guard: with no
// ownership token (Cuttlefish Stop's production call), Cleanup reaps NOTHING
// even when crosvm/run_cvd PIDs are present — it never falls back to a host-
// wide comm match. Teardown relies on `rm -f <container>` instead.
func TestCleanup_EmptyTokenRefusesReap(t *testing.T) {
	w := fakeWalker{
		comms:    map[int]string{901: "crosvm", 902: "run_cvd"},
		cmdlines: map[int][]string{901: {"crosvm", "--base_instance_num=1"}, 902: {"run_cvd", "--base_instance_num=1"}},
	}
	k := newFakeKiller()
	report, err := cleanupWithDeps(context.Background(), "", w, k)
	require.NoError(t, err)
	assert.Empty(t, report.Found, "empty ownerToken MUST refuse to reap (§11.4.174)")
	assert.Empty(t, k.signals, "no signal may be sent when ownership cannot be established")
}

func TestNewOSProcWalker_DispatchTable(t *testing.T) {
	// Linux → /proc walker; everything else → ps walker. Changing the
	// dispatch (e.g. linux → psWalker) flips the concrete type and fails
	// this assertion.
	_, isProcFS := newOSProcWalker("linux").(procFSWalker)
	assert.True(t, isProcFS, "linux must dispatch to procFSWalker")

	_, isPS := newOSProcWalker("darwin").(psWalker)
	assert.True(t, isPS, "darwin must dispatch to psWalker")

	_, isPSOther := newOSProcWalker("freebsd").(psWalker)
	assert.True(t, isPSOther, "non-linux must dispatch to psWalker")
}

func TestPSWalker_Parse(t *testing.T) {
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("  PID COMM\n  101 crosvm\n  202 /usr/bin/run_cvd\n  bad line\n"), nil
	}}
	w := psWalker{exec: fe}
	comms, err := w.PidComms()
	require.NoError(t, err)
	assert.Equal(t, "crosvm", comms[101])
	// Path-prefixed comm is reduced to its basename so the exact allowlist
	// match works.
	assert.Equal(t, "run_cvd", comms[202])
	// The "PID COMM" header and "bad line" are not numeric pids → skipped.
	assert.NotContains(t, comms, 0)
}
