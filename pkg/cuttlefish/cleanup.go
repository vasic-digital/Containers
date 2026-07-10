package cuttlefish

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cleanup.go — orphan Cuttlefish process reaping.
//
// `stop_cvd` is the graceful teardown, but an interrupted launch or a
// crashed cvd can leave orphan VMM + supervisor processes alive (the
// Cuttlefish analogue of the orphan `qemu-system-*` processes pkg/emulator
// reaps). This file reaps them by a STRICT comm allowlist so it can never
// signal an unrelated process — the same safety discipline as
// pkg/emulator.Cleanup, but with exact-match (not prefix) semantics
// because the Cuttlefish process names are exact, not parameterised.
//
// This is read-mostly + signal-only; it NEVER touches the developer's
// session processes (§12 host-session safety) — the allowlist is exact
// Cuttlefish daemon names.

// cuttlefishProcNames is the EXACT-match comm allowlist of orphan
// Cuttlefish processes Cleanup reaps. crosvm is the VMM (the crosvm/QEMU
// process that actually runs the Android VM); run_cvd + cvd_internal_start
// are the per-instance supervisors. Exact-match (not prefix) is the
// strictest safe matcher — it cannot collect an unrelated "crosvm-helper".
var cuttlefishProcNames = map[string]struct{}{
	"crosvm":             {},
	"run_cvd":            {},
	"cvd_internal_start": {},
}

// procWalker abstracts process enumeration so cleanup_test.go can inject
// synthetic data. Production uses the OS-appropriate implementation from
// newOSProcWalker (Linux: /proc; macOS/other: `ps -A`).
type procWalker interface {
	PidComms() (map[int]string, error)
	PidCmdlines() (map[int][]string, error)
}

// killer abstracts signalling for testability.
type killer interface {
	Signal(pid int, sig syscall.Signal) error
	Exists(pid int) bool
}

type osKiller struct{}

func (osKiller) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

func (osKiller) Exists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// Cleanup finds orphan Cuttlefish processes whose comm is in the exact
// allowlist (crosvm / run_cvd / cvd_internal_start), sends SIGTERM, waits
// up to 5 seconds for graceful exit, then SIGKILLs stragglers. Returns a
// CleanupReport.
//
// On Linux the process list is walked via /proc; on macOS and other POSIX
// systems it is obtained via `ps -A`. The per-OS dispatch happens inside
// newOSProcWalker so Cleanup itself has no OS-specific branches.
//
// Bluff-Audit (recorded in the implementing commit body):
//
//	Mutation: loosen the matcher from exact comm to strings.HasPrefix(comm, "cr").
//	Observed: TestCleanup_ExactNameMatch asserts a synthetic "crontab"
//	          process is NOT collected; the loosened matcher would include
//	          it, failing the test.
//	Reverted: yes.
func Cleanup(ctx context.Context, ownerToken string) (CleanupReport, error) {
	return cleanupWithDeps(ctx, ownerToken, newOSProcWalker(runtime.GOOS), osKiller{})
}

// cleanupWithDeps is the testable core. Production uses Cleanup; tests
// inject a synthetic procWalker + killer.
//
// §11.4.174 ownership scoping: the exact comm allowlist (crosvm / run_cvd /
// cvd_internal_start) is ONLY a process-CLASS pre-filter; the LOAD-BEARING
// ownership proof is the caller-supplied ownerToken appearing as an argv token
// of the process (an instance selector such as "--base_instance_num=N"). A cvd
// process belonging to a DIFFERENT instance is NEVER signalled. An empty
// ownerToken is REFUSED (safe no-op) — we never fall back to a bare host-wide
// comm match. Cuttlefish's production Stop supplies an EMPTY token (no host-
// side ownership token exists for its container-namespaced cvd), so this
// refuse-guard guarantees Stop performs NO host-wide reap; `rm -f <container>`
// is the authoritative teardown.
func cleanupWithDeps(ctx context.Context, ownerToken string, w procWalker, k killer) (CleanupReport, error) {
	var report CleanupReport
	if ownerToken == "" {
		return report, nil
	}
	pidComms, err := w.PidComms()
	if err != nil {
		return report, err
	}
	cmdlines, err := w.PidCmdlines()
	if err != nil {
		return report, err
	}
	for pid, comm := range pidComms {
		if comm == "" {
			report.SkippedReadErr = append(report.SkippedReadErr, pid)
			continue
		}
		// EXACT comm allowlist pre-filter AND the argv ownership token, proving
		// this cvd process is OURS. Falsifiability targets:
		// TestCleanup_ExactNameMatch (allowlist) +
		// TestCleanup_OwnershipScopedToOurInstance (argv token).
		if _, ok := cuttlefishProcNames[comm]; ok && argvContains(cmdlines[pid], ownerToken) {
			report.Found = append(report.Found, pid)
		}
	}
	if len(report.Found) == 0 {
		return report, nil
	}
	// Send SIGTERM to all found PIDs.
	for _, pid := range report.Found {
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
		for _, pid := range report.Found {
			if k.Exists(pid) {
				stragglers = append(stragglers, pid)
			}
		}
		if len(stragglers) == 0 {
			break
		}
	}
	// Mark PIDs that exited within the grace window as TerminatedTERM.
	terminated := make(map[int]bool, len(report.Found))
	for _, pid := range report.Found {
		terminated[pid] = true
	}
	for _, pid := range stragglers {
		terminated[pid] = false
	}
	for _, pid := range report.Found {
		if terminated[pid] {
			report.TerminatedTERM = append(report.TerminatedTERM, pid)
		}
	}
	// SIGKILL stragglers.
	for _, pid := range stragglers {
		if err := k.Signal(pid, syscall.SIGKILL); err == nil {
			report.KilledKILL = append(report.KilledKILL, pid)
		} else {
			report.Surviving = append(report.Surviving, pid)
		}
	}
	return report, nil
}

// argvContains reports whether any argv token equals tok. (An instance
// selector such as "--base_instance_num=1" is emitted by cvd as a single argv
// token; substring matches are intentionally NOT honoured — that is the
// §11.4.174 name-match bluff vector this gate exists to prevent.)
func argvContains(argv []string, tok string) bool {
	for _, a := range argv {
		if a == tok {
			return true
		}
	}
	return false
}

// procFSWalker is the Linux /proc-based walker.
type procFSWalker struct{}

func (procFSWalker) PidComms() (map[int]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			out[pid] = ""
			continue
		}
		out[pid] = strings.TrimSpace(string(b))
	}
	return out, nil
}

func (procFSWalker) PidCmdlines() (map[int][]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int][]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
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

// psWalker implements procWalker using POSIX `ps -A -o pid,comm`, used on
// macOS and any other POSIX OS without /proc.
type psWalker struct {
	exec CommandExecutor
}

func (w psWalker) PidComms() (map[int]string, error) {
	out, err := w.exec.Execute(context.Background(), "ps", "-A", "-o", "pid,comm")
	if err != nil {
		return nil, err
	}
	result := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue // header line or non-pid
		}
		// `ps -o comm` yields the basename only — but the cvd/crosvm comm
		// may be reported with a path on some platforms; take the final
		// path element so the exact allowlist match works consistently.
		comm := fields[1]
		if idx := strings.LastIndex(comm, "/"); idx >= 0 {
			comm = comm[idx+1:]
		}
		result[pid] = comm
	}
	return result, nil
}

// PidCmdlines returns pid -> argv via `ps -Aww -o pid,args` (space-delimited,
// which the argv-token ownership matcher treats identically to the NUL-split
// /proc/pid/cmdline tokens on Linux).
func (w psWalker) PidCmdlines() (map[int][]string, error) {
	out, err := w.exec.Execute(context.Background(), "ps", "-Aww", "-o", "pid,args")
	if err != nil {
		return nil, err
	}
	result := make(map[int][]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		if len(fields) < 2 {
			result[pid] = nil
			continue
		}
		result[pid] = fields[1:]
	}
	return result, nil
}

// newOSProcWalker returns the OS-appropriate procWalker. goos is a
// parameter (not runtime.GOOS) so tests can select a specific OS without
// build tags or global state.
func newOSProcWalker(goos string) procWalker {
	if goos == "linux" {
		return procFSWalker{}
	}
	return psWalker{exec: osExecutor{}}
}
