package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killByPortHook is the package-level seam tests use to substitute a
// fake KillByQEMUMonitorPort implementation. Production uses
// KillByQEMUMonitorPort below — strict adjacent-token match against
// QEMU's `-monitor tcp:127.0.0.1:<port>,server,nowait` argv pair.
//
// NOTE: tests that override this MUST NOT use t.Parallel(). The
// swap-and-restore pattern (`prev := X; X = ...; defer func() { X = prev }()`)
// is not safe against concurrent test functions racing on the
// package-level var. (Same convention as pkg/emulator/android.go.)
var killByPortHook = KillByQEMUMonitorPort

// teardownGracePeriod is the wall-clock time Teardown waits between
// initiating QMP graceful shutdown and falling through to the
// KillByQEMUMonitorPort fast-path. Production: 30 seconds. Tests override.
var teardownGracePeriod = 30 * time.Second

// Teardown attempts a 3-stage shutdown:
//
//  1. QMP system_powerdown (initiates ACPI shutdown in the guest)
//  2. wait teardownGracePeriod for QEMU process to exit
//  3. KillByQEMUMonitorPort fast-path on the monitor port — strict-
//     adjacent argv match against `-monitor tcp:127.0.0.1:<port>,...`.
//     Skip-on-mismatch (Matched==0) returns the original "did not exit"
//     error. Group B Teardown pattern.
//
// The skip-on-mismatch behavior is the load-bearing safety property
// the falsifiability rehearsal targets — see TestTeardown_FastPath_-
// SkipsOnMismatch. A weakened Teardown that returns nil on Matched=0
// would silently lie about successful teardown of a stuck VM and
// corrupt the matrix runner's row outcome.
func (v *QEMUVM) Teardown(ctx context.Context, monitorPort, sshPort int) error {
	// Release our SSH client handle on every exit path. The guest is
	// shut down via QMP / KillByQEMUMonitorPort, not via this connection,
	// so closing it here only prevents a per-lifecycle SSH connection +
	// goroutine leak (serial matrix runs reuse one QEMUVM across N
	// targets). Resetting authedPort forces a fresh Authenticate on the
	// next boot's port.
	defer func() {
		// Close the shared SSH client ONLY if this Teardown still owns
		// the authenticated session (authedPort == this target's sshPort)
		// or nobody owns it (authedPort == 0). Under --concurrent>1 a
		// different, still-in-flight target may have re-authenticated the
		// shared connection to ITS own port since this target ran; closing
		// it here would kill a live connection that target is using. The
		// `mine` check prevents that cross-target close. Guarded by
		// guestMu (lock ordering guestMu → authMu, never reversed) so it
		// cannot interleave with a concurrent guest op. See CT-HARDEN-VM-1.
		v.guestMu.Lock()
		v.authMu.Lock()
		mine := v.authedPort == sshPort || v.authedPort == 0
		v.authMu.Unlock()
		if mine {
			if v.ssh != nil {
				_ = v.ssh.Close()
			}
			v.authMu.Lock()
			v.authedPort = 0
			v.authMu.Unlock()
		}
		v.guestMu.Unlock()
	}()

	// Stage 1: QMP powerdown — best-effort. Guarded by guestMu (bounded
	// ~5s Dial timeout, NOT the 30s grace sleep below) so a concurrent
	// target's screenshot capture can't race the shared qmp connection.
	v.guestMu.Lock()
	if v.qmp != nil {
		if err := v.qmp.Dial(ctx, monitorPort, 5*time.Second); err == nil {
			_ = v.qmp.SystemPowerdown(ctx)
			_ = v.qmp.Close()
		}
	}
	v.guestMu.Unlock()

	// Stage 2: wait for graceful exit. We can't directly observe the
	// QEMU process from here without a process handle; we sleep.
	// Production gets a richer signal from QMP's SHUTDOWN event;
	// implementation TBD when the real QMP client lands. For now,
	// the unit-test path uses teardownGracePeriod=200ms and the
	// production path uses 30s; the sleep is honest about that.
	deadline := time.Now().Add(teardownGracePeriod)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Stage 3: KillByQEMUMonitorPort fast-path on the monitor port.
	report, kerr := killByPortHook(ctx, monitorPort)
	if kerr != nil {
		// Forensic-only — do not block return.
		_ = kerr
	}
	if report.Matched == 0 {
		return fmt.Errorf(
			"vm on monitor port %d did not exit within %s; KillByQEMUMonitorPort matched 0 processes (skip-on-mismatch safety)",
			monitorPort, teardownGracePeriod,
		)
	}
	if len(report.Surviving) > 0 {
		return errors.New("vm Teardown: KillByQEMUMonitorPort succeeded but some PIDs surviving SIGKILL")
	}
	return nil
}

// vmProcWalker abstracts /proc enumeration so cleanup tests can inject
// synthetic /proc data. Mirror of pkg/emulator's procWalker — duplicated
// here per YAGNI / Decoupled Reusable Architecture: the walker is a
// 5-line stdlib wrapper; cross-package coupling for that small a
// primitive is not worth it. The strict-adjacent-token matcher in
// KillByQEMUMonitorPort is the load-bearing constitutional invariant,
// not the walker.
type vmProcWalker interface {
	PidCmdlines() (map[int][]string, error)
}

type osVMProcWalker struct{}

func (osVMProcWalker) PidCmdlines() (map[int][]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	out := make(map[int][]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmdPath := filepath.Join("/proc", e.Name(), "cmdline")
		b, err := os.ReadFile(cmdPath)
		if err != nil {
			// Best-effort: process may have exited mid-walk.
			out[pid] = nil
			continue
		}
		raw := strings.TrimRight(string(b), "\x00")
		if raw == "" {
			out[pid] = nil
			continue
		}
		out[pid] = strings.Split(raw, "\x00")
	}
	return out, nil
}

// vmKiller abstracts signalling for testability. Mirror of
// pkg/emulator's killer.
type vmKiller interface {
	Signal(pid int, sig syscall.Signal) error
	Exists(pid int) bool
}

type osVMKiller struct{}

func (osVMKiller) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

func (osVMKiller) Exists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// KillByQEMUMonitorPort attempts to terminate the QEMU process(es)
// whose argv contains the adjacent pair `-monitor`,
// `tcp:127.0.0.1:<port>,server,nowait` — the exact form QEMUVM.Boot
// emits. Used by Teardown's fast-path: when QMP shutdown fails AND
// the grace window elapses, this function targets the specific QEMU
// child by its monitor TCP socket spec.
//
// SAFETY (THE LOAD-BEARING INVARIANT):
//
//   - The match is STRICT EXACT EQUALITY on the second token. A process
//     whose argv contains "-monitor" "tcp:127.0.0.1:14444,server,nowait"
//     MATCHES for monPort=14444. A process whose argv contains
//     "-monitor" "tcp:127.0.0.1:114444,server,nowait" does NOT match
//     for monPort=14444 — the substring "14444" inside "114444" is
//     irrelevant; tokenization is exact.
//   - On Matched=0, KillByQEMUMonitorPort is a complete no-op. No
//     signals are sent. Concurrent VMs on other monitor ports, sibling
//     emulator processes, and developer-spawned QEMUs are NEVER touched.
//
// pkg/emulator.KillByPort cannot be reused here: that function matches
// `-port <N>` adjacent argv, which QEMU's argv as assembled in
// pkg/vm/qemu.go does NOT contain (QEMU emits `-monitor` and `-netdev`
// instead). Using KillByPort against real QEMU would always yield
// Matched=0 and Teardown would never short-circuit a stuck VM.
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation: weaken the matcher to strings.Contains(argv[i+1], "<port>")
//	Observed: TestKillByQEMUMonitorPort_StrictAdjacentMatch asserts that
//	          a synthetic /proc fixture containing "tcp:127.0.0.1:114444,..."
//	          (port 114444, contains "14444" as substring) is NOT matched
//	          for monPort=14444; the weakened matcher matches it because
//	          "114444,server,nowait" contains "14444" as a substring.
//	Reverted: yes
func KillByQEMUMonitorPort(ctx context.Context, monPort int) (KillReport, error) {
	return killByQEMUMonitorPortWithDeps(ctx, monPort, osVMProcWalker{}, osVMKiller{})
}

// killByQEMUMonitorPortWithDeps is the testable core; production
// KillByQEMUMonitorPort wires the real vmProcWalker + vmKiller.
func killByQEMUMonitorPortWithDeps(
	ctx context.Context,
	monPort int,
	w vmProcWalker,
	k vmKiller,
) (KillReport, error) {
	var report KillReport
	cmdlines, err := w.PidCmdlines()
	if err != nil {
		return report, err
	}
	target := fmt.Sprintf("tcp:127.0.0.1:%d,server,nowait", monPort)
	for pid, argv := range cmdlines {
		// STRICT adjacent-token + STRICT EXACT EQUALITY match. Walk
		// argv looking for the literal token "-monitor" immediately
		// followed by the EXACT target string. Substring matches are
		// intentionally NOT honoured — the substring would let port
		// 14444 collide with port 114444 and other ports containing
		// the digits as a substring; that is the bluff vector this
		// API exists to prevent.
		matched := false
		for i := 0; i < len(argv)-1; i++ {
			if argv[i] == "-monitor" && argv[i+1] == target {
				matched = true
				break
			}
		}
		if matched {
			report.Matched++
			report.Sigtermed = append(report.Sigtermed, pid)
		}
	}
	if report.Matched == 0 {
		return report, nil
	}
	for _, pid := range report.Sigtermed {
		_ = k.Signal(pid, syscall.SIGTERM)
	}
	// Wait up to 5 seconds, polling every 250ms.
	deadline := time.Now().Add(5 * time.Second)
	var stragglers []int
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		stragglers = stragglers[:0]
		for _, pid := range report.Sigtermed {
			if k.Exists(pid) {
				stragglers = append(stragglers, pid)
			}
		}
		if len(stragglers) == 0 {
			break
		}
	}
	for _, pid := range stragglers {
		if err := k.Signal(pid, syscall.SIGKILL); err == nil {
			report.Sigkilled = append(report.Sigkilled, pid)
		} else {
			report.Surviving = append(report.Surviving, pid)
		}
	}
	return report, nil
}
