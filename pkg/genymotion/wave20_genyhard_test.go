package genymotion

// Batch CT-HARDEN-GENY-HARD (Wave-20) — §11.4.115 RED→GREEN behavioral guards.
//
// Each guard is GREEN against the fixed genymotion.go and, per §11.4.115, was
// proven RED by a surgical revert of the corresponding fix (see the conductor
// evidence block). Guards drive the injectable exec seam / pure parseList — no
// live gmtool is required (§11.4.27).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// GENY-1 — StartAndWait MUST bound a wedged `admin list` by its own timeout.
//
// A wedged gmtool exec that honors ctx but blocks until cancelled (VM/hypervisor
// stall) must be cancelled at the StartAndWait deadline, NOT hang forever on the
// caller's context.Background(). Pre-fix (List used the caller ctx directly),
// StartAndWait never returns → the test-side timeout registers the hang as a
// genuine FAIL rather than an infinite test.
func TestStartAndWait_GENY1_deadlineBoundsWedgedList(t *testing.T) {
	t.Parallel()
	tool := &Tool{Path: "/fake/gmtool", exec: func(ctx context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "list" {
			<-ctx.Done() // honor ctx but never produce output (wedged gmtool)
			return "", ctx.Err()
		}
		return "", nil // admin start returns immediately
	}}

	type res struct {
		d   Device
		err error
	}
	done := make(chan res, 1)
	started := time.Now()
	go func() {
		d, err := tool.StartAndWait(context.Background(), "Google Pixel 9", 500*time.Millisecond)
		done <- res{d, err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("GENY-1: expected a deadline/timeout error, got nil (device %+v)", r.d)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("GENY-1: returned but took %s (>2s) — deadline not enforced against a wedged exec", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("GENY-1: StartAndWait HUNG past 3s on a wedged `admin list` — timeout not enforced against caller ctx (pre-fix behavior)")
	}
}

// GENY-4 — StartAndWait MUST keep polling to the deadline after a transient
// `admin list` error, not abort the whole wait on the first failure.
func TestStartAndWait_GENY4_retriesTransientListError(t *testing.T) {
	t.Parallel()
	var listCalls int
	tool := &Tool{Path: "/fake/gmtool", pollInterval: 5 * time.Millisecond, exec: func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "list" {
			listCalls++
			if listCalls == 1 {
				return "Error: device busy", errors.New("exit 1") // transient
			}
			return realAdminList, nil // recovered: On+serial
		}
		return "", nil // admin start
	}}

	d, err := tool.StartAndWait(context.Background(), "Google Pixel 9", 10*time.Second)
	if err != nil {
		t.Fatalf("GENY-4: a single transient list error aborted the wait: %v", err)
	}
	if d.ADBSerial != "127.0.0.1:6555" || !d.IsOn() {
		t.Errorf("GENY-4: resolved device = %+v, want On with serial 127.0.0.1:6555", d)
	}
	if listCalls < 2 {
		t.Errorf("GENY-4: expected a retry after the transient error, listCalls=%d", listCalls)
	}
}

// GENY-2 — when an ADBProbe is attached, StartAndWait MUST wait for Android to
// report booted (`sys.boot_completed=1`) before returning ready, not return at
// mere VM On+serial. Probe reports not-booted on the first call, booted on the
// second, so a compliant StartAndWait calls it at least twice.
func TestStartAndWait_GENY2_waitsForBootWhenProbeSet(t *testing.T) {
	t.Parallel()
	tool := &Tool{Path: "/fake/gmtool", pollInterval: 5 * time.Millisecond, exec: func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "list" {
			return realAdminList, nil // immediately On+serial
		}
		return "", nil // admin start
	}}
	var probeCalls int
	tool.WithADBProbe(func(_ context.Context, serial string) (bool, error) {
		if serial != "127.0.0.1:6555" {
			return false, fmt.Errorf("GENY-2: probe got unexpected serial %q", serial)
		}
		probeCalls++
		return probeCalls >= 2, nil // not booted first, booted second
	})

	d, err := tool.StartAndWait(context.Background(), "Google Pixel 9", 10*time.Second)
	if err != nil {
		t.Fatalf("GENY-2: StartAndWait with boot probe errored: %v", err)
	}
	if d.ADBSerial != "127.0.0.1:6555" {
		t.Errorf("GENY-2: resolved device = %+v, want serial 127.0.0.1:6555", d)
	}
	if probeCalls < 2 {
		t.Errorf("GENY-2: StartAndWait returned before boot completed; probeCalls=%d, want >=2 (returned at mere VM On+serial)", probeCalls)
	}
}

// GENY-2 (default boundary) — WITHOUT a probe attached, StartAndWait keeps its
// backward-compatible readiness boundary: it returns at VM On+serial. This pins
// the honest documented default and guards the nil-probe path.
func TestStartAndWait_GENY2_noProbeReturnsAtPoweredSerial(t *testing.T) {
	t.Parallel()
	tool := &Tool{Path: "/fake/gmtool", pollInterval: 5 * time.Millisecond, exec: func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "list" {
			return realAdminList, nil
		}
		return "", nil
	}}
	d, err := tool.StartAndWait(context.Background(), "Google Pixel 9", 10*time.Second)
	if err != nil {
		t.Fatalf("no-probe StartAndWait errored: %v", err)
	}
	if d.ADBSerial != "127.0.0.1:6555" || !d.IsOn() {
		t.Errorf("no-probe device = %+v, want On serial 127.0.0.1:6555", d)
	}
}

// GENY-3 — parseList MUST retain a row whose State is a transitional/unknown
// value ("Starting") rather than silently dropping it. Pure-function guard.
func TestParseList_GENY3_retainsTransitionalState(t *testing.T) {
	t.Parallel()
	out := ` State    |   ADB Serial    |                UUID                |      Name
----------+-----------------+------------------------------------+---------------
 Starting |  127.0.0.1:6559 |cccc1111-2222-3333-4444-555566667777| Pixel 9 Booting
`
	devices := parseList(out)
	if len(devices) != 1 {
		t.Fatalf("GENY-3: transitional-state row dropped; got %d devices: %+v", len(devices), devices)
	}
	if devices[0].State != "Starting" {
		t.Errorf("GENY-3: State = %q, want verbatim \"Starting\"", devices[0].State)
	}
	if devices[0].Name != "Pixel 9 Booting" {
		t.Errorf("GENY-3: Name = %q, want \"Pixel 9 Booting\"", devices[0].Name)
	}
	if devices[0].IsOn() {
		t.Errorf("GENY-3: IsOn() = true for \"Starting\", want false (only literal On is running)")
	}
}

// GENY-3 (regression) — an empty-State row is still dropped, and the header row
// is still skipped, so retaining unknown states does not admit junk rows.
func TestParseList_GENY3_dropsEmptyStateAndHeader(t *testing.T) {
	t.Parallel()
	out := ` State    |   ADB Serial    |                UUID                |      Name
----------+-----------------+------------------------------------+---------------
          |                 |                                    |
       On |  127.0.0.1:6555 |ed811e5e-df4d-46d8-ab1f-4250058e57e3| Google Pixel 9
`
	devices := parseList(out)
	if len(devices) != 1 {
		t.Fatalf("GENY-3: empty-state/header not dropped; got %d devices: %+v", len(devices), devices)
	}
	if devices[0].Name != "Google Pixel 9" || !devices[0].IsOn() {
		t.Errorf("GENY-3: device = %+v, want On Google Pixel 9", devices[0])
	}
}
