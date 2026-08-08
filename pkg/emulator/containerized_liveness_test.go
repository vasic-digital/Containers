package emulator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// containerized_liveness_test.go — LVA-014 fix #2 test pack
// (container-liveness check in Containerized.WaitForBoot).
//
// Forensic anchor: docs/CONTINUATION.md 2026-07-04. The containerized
// gate "boot hang" was a container that EXITED in ~4s (entrypoint
// AVD-not-found pre-check) while WaitForBoot — which had NO container
// liveness check — polled the dead forwarded adb port until its 5m/12m
// deadline, misreporting the 4s config error as a boot timeout. The
// container's --rm reaped the log, destroying the evidence.
//
// These tests pin:
//   - a container that exits mid-wait fails WaitForBoot FAST (long
//     before the deadline) with the container's captured logs in the
//     error,
//   - an inspect failure (container already reaped by --rm) is also an
//     immediate, honest failure,
//   - a running container keeps the EMU-1 adb-poll semantics intact,
//   - a Containerized without a Boot-set containerName performs NO
//     liveness exec at all (EMU-1 byte-for-byte preservation).

// livenessInspectKey returns the fakeExecutor script key for the
// liveness inspect call against the given container name.
func livenessInspectKey(containerName string) string {
	return "podman inspect --format " + containerInspectStateFormat + " " + containerName
}

// livenessLogsKey returns the fakeExecutor script key for the log
// capture call against the given container name.
func livenessLogsKey(containerName string) string {
	return "podman logs --tail 100 " + containerName
}

// TestContainerized_WaitForBoot_FailsFastWhenContainerExits reproduces
// the 2026-07-04 incident against the fixed code path: the container
// exits before sys.boot_completed=1; WaitForBoot MUST return
// immediately (not at the 60s deadline) with an error that carries the
// container's log tail — the entrypoint's "AVD not found" diagnostic
// the --rm reaper destroyed in the original incident.
//
//	Bluff-Audit (executed, see commit body): comment out the
//	containerExited block in WaitForBoot → this test FAILS with
//	"WaitForBoot timed out after 1m0s …" (the deadline path) instead
//	of the fast container-exit error, and the duration assertion fires
//	("took ~1m0s, want ≪ 60s deadline"). Reverted: yes.
func TestContainerized_WaitForBoot_FailsFastWhenContainerExits(t *testing.T) {
	const container = "lava-emu-CZ_API34_Phone-1"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555": {Out: []byte("connected\n")},
			"adb -s localhost:5555 shell getprop sys.boot_completed": {Out: []byte("\n")},
			livenessInspectKey(container):                           {Out: []byte("false 1\n")},
			livenessLogsKey(container): {Out: []byte(
				"ERROR: AVD 'CZ_API34_Phone' not found in image. Available:\n    Name: default\n",
			)},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "reg.test/emu:api34-x86_64",
		Executor:      fake,
	})
	// Simulate a prior Boot having named the container (the existing
	// test pack sets the field directly for authorizeADB; same seam).
	c.containerName = container

	start := time.Now()
	_, err := c.WaitForBoot(context.Background(), 5555, 60*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitForBoot must fail when the emulator container exits mid-wait")
	}
	if elapsed >= 30*time.Second {
		t.Errorf("WaitForBoot took %v — the liveness check must fail FAST, not at the 60s deadline", elapsed)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error must NOT be the deadline timeout path; got: %v", err)
	}
	for _, want := range []string{"exited", "exit code 1", "not found in image", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q (container exit + captured logs); got: %v", want, err)
		}
	}
	// The log-capture exec MUST have happened (the --rm reaper races
	// us; capturing on exit detection is the whole point).
	foundLogs := false
	for _, call := range fake.calls {
		if call.Name == "podman" && len(call.Args) >= 1 && call.Args[0] == "logs" {
			foundLogs = true
		}
	}
	if !foundLogs {
		t.Errorf("container logs must be captured on exit detection; calls: %+v", fake.calls)
	}
}

// TestContainerized_WaitForBoot_InspectFailureTreatedAsExit pins the
// reaped-container path: once --rm has removed the exited container,
// `podman inspect` errors — that is STILL an immediate honest failure
// (never a poll-to-deadline), and the error says the logs could not be
// recovered rather than presenting an empty log block.
func TestContainerized_WaitForBoot_InspectFailureTreatedAsExit(t *testing.T) {
	const container = "lava-emu-gone-1"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555": {Out: []byte("connected\n")},
			"adb -s localhost:5555 shell getprop sys.boot_completed": {Out: []byte("\n")},
			livenessInspectKey(container): {
				Out: []byte("Error: no such container\n"),
				Err: errors.New("exit 1"),
			},
			livenessLogsKey(container): {
				Out: []byte("Error: no such container\n"),
				Err: errors.New("exit 1"),
			},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "reg.test/emu:api34-x86_64",
		Executor:      fake,
	})
	c.containerName = container

	start := time.Now()
	_, err := c.WaitForBoot(context.Background(), 5555, 60*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitForBoot must fail when the container is already reaped")
	}
	if elapsed >= 30*time.Second {
		t.Errorf("WaitForBoot took %v — reaped-container detection must be fast", elapsed)
	}
	if !strings.Contains(err.Error(), "reaped") {
		t.Errorf("error must report the reaped-container inspect failure; got: %v", err)
	}
	if !strings.Contains(err.Error(), "logs unavailable") {
		t.Errorf("error must honestly report the unrecoverable logs; got: %v", err)
	}
}

// TestContainerized_WaitForBoot_KeepsPollingWhileContainerAlive pins
// the non-regression of the happy path: a RUNNING container ("true 0")
// keeps the EMU-1 adb poll semantics — first poll not-booted, second
// poll booted, success.
func TestContainerized_WaitForBoot_KeepsPollingWhileContainerAlive(t *testing.T) {
	const container = "lava-emu-alive-1"
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555": {Out: []byte("connected\n")},
			livenessInspectKey(container): {Out: []byte("true 0\n")},
		},
		sequencedScripts: map[string][]fakeScript{
			"adb -s localhost:5555 shell getprop sys.boot_completed": {
				{Out: []byte("\n")},  // not yet booted
				{Out: []byte("1\n")}, // booted
			},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "reg.test/emu:api34-x86_64",
		Executor:      fake,
	})
	c.containerName = container

	if _, err := c.WaitForBoot(context.Background(), 5555, 30*time.Second); err != nil {
		t.Fatalf("WaitForBoot must succeed while the container stays alive; got: %v", err)
	}
	// The liveness inspect MUST actually have run on the alive path —
	// otherwise the fast-fail test above would be exercising dead code.
	foundInspect := false
	for _, call := range fake.calls {
		if call.Name == "podman" && len(call.Args) >= 1 && call.Args[0] == "inspect" {
			foundInspect = true
		}
	}
	if !foundInspect {
		t.Errorf("liveness inspect must run when containerName is set; calls: %+v", fake.calls)
	}
}

// TestContainerized_WaitForBoot_NoContainerNameSkipsLiveness pins the
// EMU-1 preservation contract: WaitForBoot invoked on an instance that
// never ran Boot (containerName == "") performs NO container-liveness
// exec — the plain adb-poll behavior is byte-for-byte the pre-LVA-014
// one (covered end-to-end by the pre-existing
// TestContainerized_WaitForBoot_PollsGetpropUntilCompleted).
func TestContainerized_WaitForBoot_NoContainerNameSkipsLiveness(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555": {Out: []byte("connected\n")},
			"adb -s localhost:5555 shell getprop sys.boot_completed": {Out: []byte("1\n")},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "reg.test/emu:api34-x86_64",
		Executor:      fake,
	})
	// containerName deliberately left empty (no Boot).

	if _, err := c.WaitForBoot(context.Background(), 5555, 30*time.Second); err != nil {
		t.Fatalf("WaitForBoot: %v", err)
	}
	for _, call := range fake.calls {
		if call.Name == "podman" {
			t.Errorf("no podman exec may happen when containerName is empty (EMU-1 preservation); got: %v", call.Args)
		}
	}
}
