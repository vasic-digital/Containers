// cleanup_os.go — OS-specific procWalker implementations.
//
// §6.AH-debt close: the original osProcWalker in cleanup.go walked
// /proc directly, which is a Linux-only API — it fails on macOS with
// "open /proc: no such file or directory". This file provides the
// per-OS dispatch table that cleanup.go's Cleanup + KillByPort use
// at runtime (via newOSProcWalker()).
//
// OS mapping (fact, not guess):
//   - "linux"  → procFSWalker reads /proc/<pid>/comm + /proc/<pid>/cmdline.
//     Identical to the pre-fix behaviour; no behavioral regression.
//   - "darwin" → psWalker uses `ps -A -o pid,comm` (comms) and
//     `ps -A -o pid,args` (cmdlines). pgrep is not used because it does
//     not expose the full argv the strict adjacent-token matcher in
//     KillByPort requires.
//   - other    → psWalker (POSIX `ps` is available on every POSIX OS
//     we target; safe conservative default).
//
// Anti-bluff posture (§6.J/§6.L):
//   - procFSWalker is unit-tested by injecting fake /proc fixtures
//     (existing cleanup_test.go).
//   - psWalker is unit-tested by injecting a fake CommandExecutor
//     (cleanup_os_test.go — the load-bearing macOS path).
//   - A test that asserts "the correct walker is selected for the OS"
//     (TestNewOSProcWalker_DispatchTable) is the falsifiability-
//     rehearsed gate for this dispatch.
package emulator

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procFSWalker is the Linux /proc-based walker used on Linux hosts.
// Identical semantics to the pre-fix osProcWalker; extracted here so
// newOSProcWalker can select it per-OS.
type procFSWalker struct{}

func (procFSWalker) PidComms() (map[int]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	out := make(map[int]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		commPath := "/proc/" + e.Name() + "/comm"
		b, err := os.ReadFile(commPath)
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
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	out := make(map[int][]string)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmdPath := "/proc/" + e.Name() + "/cmdline"
		b, err := os.ReadFile(cmdPath)
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

// psExecutor is the seam used by psWalker to run `ps`. Production uses
// the real os/exec path via osExecutor; tests inject a fake.
type psExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

// psWalker implements procWalker using POSIX `ps`. Used on macOS (and
// any OS without /proc). It asks `ps` for the full process list rather
// than walking a filesystem, so it works wherever `ps -A` works.
//
// Why `ps -A -o pid,comm` and `ps -A -o pid,args`:
//   - `comm` is the basename of the executable (matches the comm field
//     that Linux /proc/pid/comm exposes — same semantics).
//   - `args` is the full command line including all argv tokens (matches
//     Linux /proc/pid/cmdline semantics — same semantics, space-delimited
//     here vs NUL-delimited in /proc).
//
// Parsing: we split each line on whitespace, taking the first token as
// the PID and the rest as the value. `ps` headers are filtered by the
// non-numeric first token.
type psWalker struct {
	exec psExecutor
}

// PidComms returns pid → process name (comm) via `ps -A -o pid,comm`.
func (w psWalker) PidComms() (map[int]string, error) {
	ctx := context.Background()
	out, err := w.exec.Execute(ctx, "ps", "-A", "-o", "pid,comm")
	if err != nil {
		return nil, fmt.Errorf("ps -A -o pid,comm: %w", err)
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
		// comm is the second field; `ps -o comm` on macOS outputs the
		// basename only (no path) — consistent with Linux /proc/pid/comm.
		result[pid] = fields[1]
	}
	return result, nil
}

// PidCmdlines returns pid → argv tokens via `ps -A -o pid,args`. The
// `args` column from `ps` is space-delimited (unlike the NUL-delimited
// /proc/pid/cmdline on Linux); we split on whitespace, which is
// semantically equivalent for the KillByPort adjacent-token matcher.
func (w psWalker) PidCmdlines() (map[int][]string, error) {
	ctx := context.Background()
	// Use ww flag to prevent `ps` from truncating long command lines.
	out, err := w.exec.Execute(ctx, "ps", "-Aww", "-o", "pid,args")
	if err != nil {
		return nil, fmt.Errorf("ps -Aww -o pid,args: %w", err)
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
			continue // header line or non-pid
		}
		if len(fields) < 2 {
			result[pid] = nil
			continue
		}
		// argv tokens: fields[1:] — the same token decomposition a
		// Linux /proc/pid/cmdline NUL-split produces. KillByPort's
		// adjacent-token matcher works identically on both.
		result[pid] = fields[1:]
	}
	return result, nil
}

// newOSProcWalker returns the OS-appropriate procWalker.
// Per §6.AH-debt: the production path for Linux is unchanged
// (procFSWalker); the production path for macOS + other POSIX OSes is
// psWalker backed by the real osExecutor.
//
// The function takes goos as a parameter (not runtime.GOOS) so tests can
// inject a specific OS without needing build tags or global state.
//
// Anti-bluff: this function is the falsifiability-rehearsal target for
// TestNewOSProcWalker_DispatchTable — changing "linux" to return psWalker
// makes that test fail because the returned type changes.
func newOSProcWalker(goos string) procWalker {
	if goos == "linux" {
		return procFSWalker{}
	}
	// macOS, Windows (via WSL/POSIX layer), and any other POSIX OS: use
	// the ps-based walker.
	return psWalker{exec: osExecutor{}}
}
