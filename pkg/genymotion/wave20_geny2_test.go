package genymotion

// Batch CT-HARDEN-GENY-HARD (Wave-20 DEEPER, §11.4.118 2nd pass) — §11.4.115
// RED→GREEN behavioral guard for a NEW defect found beyond GENY-1..5.
//
// Each guard is GREEN against the fixed genymotion.go and was proven RED by a
// surgical single-line revert of the corresponding fix (see FIX GENY2-N in the
// conductor report). Guards drive the injectable exec seam — no live gmtool is
// required (§11.4.27).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// GENY2-1 (security, argument injection §11.4.174-adjacent) — Start / Stop /
// StartAndWait MUST refuse a caller-supplied device Name/UUID that BEGINS WITH
// '-' BEFORE spawning gmtool.
//
// gmtool subcommands are spawned as a bare argv (exec.CommandContext(path,
// "admin", "start"/"stop", name), no shell) with the device name as the FINAL
// positional and NO "--" end-of-options guard, so a name like
// "-oProxyCommand=..." or "--help" is parsed by gmtool's OWN option parser as a
// FLAG, not a device — argument injection (exact mirror of pkg/network §NET3 /
// pkg/egress §EG2-1, where ssh has the same no-reliable-"--" property). The guard
// MUST fire BEFORE any exec (a rejected name spawns NO gmtool process) and MUST
// NOT over-reject a normal name (negative control below is the anti-tautology
// discriminator against a blanket "always refuse").
func TestWave20_GENY2_RejectsLeadingDashDeviceName(t *testing.T) {
	t.Parallel()

	newRec := func() (*Tool, *[][]string) {
		var calls [][]string
		tool := &Tool{Path: "/fake/gmtool", pollInterval: time.Millisecond, exec: func(_ context.Context, _ string, args ...string) (string, error) {
			calls = append(calls, args)
			return realAdminList, nil
		}}
		return tool, &calls
	}

	// Begins with '-': gmtool getopt would treat this as a flag, not a device.
	const evil = "-oProxyCommand=/bin/sh"

	// Start refuses + spawns nothing.
	tool, calls := newRec()
	if err := tool.Start(context.Background(), evil); err == nil {
		t.Fatalf("GENY2-1: Start(%q) returned nil — leading-dash name not refused (argv injection)", evil)
	} else if !errors.Is(err, ErrUnsafeName) {
		t.Fatalf("GENY2-1: Start(%q) error = %v, want errors.Is ErrUnsafeName", evil, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("GENY2-1: Start(%q) spawned gmtool %v — guard must fire BEFORE exec", evil, *calls)
	}

	// Stop refuses + spawns nothing.
	tool, calls = newRec()
	if err := tool.Stop(context.Background(), evil); err == nil || !errors.Is(err, ErrUnsafeName) {
		t.Fatalf("GENY2-1: Stop(%q) error = %v, want errors.Is ErrUnsafeName", evil, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("GENY2-1: Stop(%q) spawned gmtool %v — guard must fire BEFORE exec", evil, *calls)
	}

	// StartAndWait refuses transitively (its first act is t.Start) + spawns nothing.
	tool, calls = newRec()
	if _, err := tool.StartAndWait(context.Background(), evil, 5*time.Second); err == nil || !errors.Is(err, ErrUnsafeName) {
		t.Fatalf("GENY2-1: StartAndWait(%q) error = %v, want errors.Is ErrUnsafeName", evil, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("GENY2-1: StartAndWait(%q) spawned gmtool %v — guard must fire BEFORE exec", evil, *calls)
	}

	// Negative control (anti-tautology): a NORMAL name is NOT rejected — Start
	// proceeds to exec `admin start Google Pixel 9`. A blanket "always refuse"
	// implementation FAILs this assertion.
	tool, calls = newRec()
	if err := tool.Start(context.Background(), "Google Pixel 9"); err != nil {
		t.Fatalf("GENY2-1: Start(normal) over-rejected a legitimate device name: %v", err)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 3 || (*calls)[0][2] != "Google Pixel 9" {
		t.Fatalf("GENY2-1: Start(normal) did not spawn `admin start Google Pixel 9`: %v", *calls)
	}
}
