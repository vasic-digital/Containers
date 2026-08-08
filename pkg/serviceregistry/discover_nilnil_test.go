package serviceregistry

import (
	"context"
	"net"
	"os"
	"testing"
)

// deleteOnFirstInfoLogger deterministically forces the "service unregistered
// mid-discovery" interleaving WITHOUT relying on goroutine scheduling luck.
// Register writes r.services[name], releases r.mu, and only THEN calls
// logger.Info("Registered service ..."). This logger deletes the just-registered
// entry on that first Info call, so Discover's subsequent Get(name) misses and
// returns (nil, false) — exactly the window a concurrent Unregister(name) opens.
type deleteOnFirstInfoLogger struct {
	reg   *ServiceRegistry
	name  string
	fired bool
}

func (l *deleteOnFirstInfoLogger) Info(msg string, args ...any) {
	// `fired` is set BEFORE calling Unregister so Unregister's own
	// "Unregistered service ..." Info re-enters here and returns immediately —
	// no infinite recursion. (sync.Once would DEADLOCK: Once.Do is not reentrant.)
	// Touched only from the single test goroutine, so the plain bool is race-free.
	if l.fired {
		return
	}
	l.fired = true
	l.reg.Unregister(l.name)
}
func (l *deleteOnFirstInfoLogger) Debug(msg string, args ...any) {}
func (l *deleteOnFirstInfoLogger) Warn(msg string, args ...any)  {}
func (l *deleteOnFirstInfoLogger) Error(msg string, args ...any) {}

// TestDiscover_UnregisteredMidDiscovery_NilNilContract proves Discover's
// non-cached path never returns (nil *Service, nil error).
//
// §11.4.115 polarity switch (committed form defaults to the GREEN guard so a
// plain `go test ./...` on the fixed tree is CI-GREEN):
//   - default (RED_MODE unset/0): permanent regression guard — Discover must
//     NEVER return (nil,nil); FAILS on the pre-fix artifact, PASSES on fixed.
//   - RED_MODE=1: reproduction mode — asserts the defect is PRESENT; PASSES on
//     the pre-fix artifact (captures reproduction), FAILS on fixed.
func TestDiscover_UnregisteredMidDiscovery_NilNilContract(t *testing.T) {
	reproduceDefect := os.Getenv("RED_MODE") == "1" // default: GREEN guard (assert defect ABSENT)

	r := newTestRegistry(t)

	// A real listener so Discover's checkPort() dial succeeds and the non-cached
	// path proceeds into Register -> (hook fires) -> Get.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	r.defaultHost = "127.0.0.1"
	r.logger = &deleteOnFirstInfoLogger{reg: r, name: "web"}

	svc, derr := r.Discover(context.Background(), "web", port, port, port+1)
	gotNilNil := svc == nil && derr == nil

	if reproduceDefect {
		if !gotNilNil {
			t.Fatalf("RED_MODE=1: expected to reproduce the (nil,nil) defect on the "+
				"pre-fix code, but Discover returned svc=%v err=%v", svc, derr)
		}
		t.Logf("REPRODUCED: Discover returned (nil *Service, nil error) after the " +
			"service was unregistered mid-discovery")
		return
	}

	// RED_MODE=0: permanent regression guard.
	if gotNilNil {
		t.Fatalf("CONTRACT VIOLATION: Discover returned (nil *Service, nil error); it " +
			"MUST return a non-nil *Service OR a non-nil error (caller nil-derefs otherwise)")
	}
	t.Logf("guard OK: Discover returned svc=%v err=%v", svc, derr)
}
