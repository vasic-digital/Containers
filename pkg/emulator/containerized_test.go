package emulator

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Bluff-Audit posture (parent project's clauses 6.J + 6.L + 6.X):
// Every test in this file is paired with a deliberate-mutation
// rehearsal documented in the commit body that introduces it.
// The §6.X-debt close requires that the Containerized type's
// observable behavior cannot pass while the production code path is
// broken. The fakeExecutor seam (shared with android_test.go) lets
// these tests assert on the EXACT command lines the production code
// would have issued to podman/docker/adb/gradle on a real Linux
// x86_64 host — that's the wire-observable surface a real container
// runtime would see.
//
// Tests that REQUIRE a real container runtime (podman/docker) +
// /dev/kvm are NOT here; they live in containerized_realstack_test.go
// behind a build tag + a t.Skip("SKIP-OK: §6.X-debt — darwin/arm64
// has no /dev/kvm; this test runs on Linux x86_64 gate runners.")
// when the host doesn't satisfy the gate.

func TestContainerized_NewContainerized_RejectsEmptyRuntimeBinary(t *testing.T) {
	_, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "",
		Image:         "any:tag",
	})
	if err == nil {
		t.Fatal("expected error when RuntimeBinary is empty; got nil")
	}
	if !strings.Contains(err.Error(), "RuntimeBinary") {
		t.Errorf("error message must mention RuntimeBinary; got: %v", err)
	}
}

func TestContainerized_NewContainerized_RejectsEmptyImage(t *testing.T) {
	_, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "",
	})
	if err == nil {
		t.Fatal("expected error when Image is empty; got nil")
	}
	if !strings.Contains(err.Error(), "Image") {
		t.Errorf("error message must mention Image; got: %v", err)
	}
}

// TestContainerized_Boot_InvokesRuntimeRunWithKvmAndPortForward is the
// §6.X primary falsifiability anchor. It asserts that Boot's recorded
// invocation against the runtime CLI carries:
//   - `run -d --name <generated> --rm`
//   - `--device /dev/kvm` (the KVM-passthrough flag that §6.X requires)
//   - `-p <hostPort>:5555/tcp` (the ADB-port forward)
//   - `-p <hostPort-1>:5554/tcp` (the console-port forward)
//   - the image reference verbatim
//
// Bluff-Audit (mutation rehearsal): removing the conditional
// `--device <kvm>` append in buildContainerRunArgs() makes this test
// fail with "captured args missing --device <kvm>" — which is the
// user-visible signal that the container would NOT have KVM access and
// emulator boot would fail with `qemu-system-x86_64: Could not access
// KVM kernel module`.
//
// §6.AH-debt note: KVM passthrough is now CONDITIONAL on the host
// exposing the KVM device (kvmAvailable()) — on macOS the podman VM has
// no /dev/kvm and the flag is correctly omitted (TCG fallback;
// see TestContainerized_KVMPresence_Absent). This test pins the
// KVM-PRESENT branch by redirecting kvmDevicePath to an existing file,
// so the passthrough contract is asserted deterministically on any host.
func TestContainerized_Boot_InvokesRuntimeRunWithKvmAndPortForward(t *testing.T) {
	// Pin the KVM-present branch host-independently: point kvmDevicePath
	// at a file that exists so kvmAvailable() is true even on a macOS
	// host where the real /dev/kvm is absent.
	presentKVM := t.TempDir() + "/kvm"
	if err := os.WriteFile(presentKVM, nil, 0o644); err != nil {
		t.Fatalf("seed fake kvm device: %v", err)
	}
	origKVM := kvmDevicePath
	kvmDevicePath = presentKVM
	t.Cleanup(func() { kvmDevicePath = origKVM })

	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"podman": {Out: []byte("container-id\n"), Err: nil},
		},
	}
	c, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "registry.test/lava-android-emulator:api34",
		Executor:      fake,
	})
	if err != nil {
		t.Fatalf("NewContainerized: %v", err)
	}

	result, err := c.Boot(context.Background(), AVD{Name: "Pixel_API34_Phone", APILevel: 34}, true)
	if err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
	if !result.Started {
		t.Errorf("BootResult.Started must be true on success")
	}
	if c.ContainerName() == "" {
		t.Errorf("ContainerName() must be populated after Boot")
	}
	if c.HostADBPort() == 0 {
		t.Errorf("HostADBPort() must be populated after Boot")
	}
	if !strings.Contains(c.ContainerName(), "Pixel_API34_Phone") {
		t.Errorf("ContainerName() must contain AVD name; got %q", c.ContainerName())
	}

	// §6.J primary assertion: the wire-observable command line.
	call := firstCallMatching(t, fake.calls, "podman")
	hasKvm := false
	hasADBPort := false
	hasConsolePort := false
	hasImage := false
	hasName := false
	hasRm := false
	hasColdBoot := false
	for i, a := range call.Args {
		if a == "--device" && i+1 < len(call.Args) && call.Args[i+1] == kvmDevicePath {
			hasKvm = true
		}
		if a == "-p" && i+1 < len(call.Args) {
			next := call.Args[i+1]
			if strings.HasSuffix(next, ":5555/tcp") {
				hasADBPort = true
			}
			if strings.HasSuffix(next, ":5554/tcp") {
				hasConsolePort = true
			}
		}
		if a == "--name" && i+1 < len(call.Args) && strings.Contains(call.Args[i+1], "Pixel_API34_Phone") {
			hasName = true
		}
		if a == "--rm" {
			hasRm = true
		}
		if a == "registry.test/lava-android-emulator:api34" {
			hasImage = true
		}
		if a == "-e" && i+1 < len(call.Args) && call.Args[i+1] == "ANDROID_COLD_BOOT=true" {
			hasColdBoot = true
		}
	}
	if !hasKvm {
		t.Errorf("captured args missing --device %s (§6.X clause 1 KVM passthrough, KVM-present branch): %v", kvmDevicePath, call.Args)
	}
	if !hasADBPort {
		t.Errorf("captured args missing -p <hostPort>:5555/tcp ADB port forward: %v", call.Args)
	}
	if !hasConsolePort {
		t.Errorf("captured args missing -p <hostPort>:5554/tcp console port forward: %v", call.Args)
	}
	if !hasName {
		t.Errorf("captured args missing --name <containing AVD name>: %v", call.Args)
	}
	if !hasRm {
		t.Errorf("captured args missing --rm (auto-cleanup after stop): %v", call.Args)
	}
	if !hasImage {
		t.Errorf("captured args missing image reference: %v", call.Args)
	}
	if !hasColdBoot {
		t.Errorf("captured args missing -e ANDROID_COLD_BOOT=true (§6.I clause 6 cold-boot): %v", call.Args)
	}
}

// TestContainerized_Boot_PropagatesRuntimeFailure asserts the
// fail-loud posture: if `podman run` errors, BootResult.Started is
// false AND the error is returned (NOT silently swallowed). A bluff
// would be returning a "started" BootResult while logging a warning;
// per clause 6.J/6.B the caller MUST distinguish container "Up" from
// "started successfully", and Started=true is the canonical signal.
func TestContainerized_Boot_PropagatesRuntimeFailure(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"podman": {Out: []byte("Error: image not found\n"), Err: errors.New("exit 125")},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "nonexistent:tag",
		Executor:      fake,
	})
	result, err := c.Boot(context.Background(), AVD{Name: "test"}, true)
	if err == nil {
		t.Fatal("Boot must return error when runtime CLI fails")
	}
	if result.Started {
		t.Errorf("BootResult.Started must be false when runtime CLI failed")
	}
	if !strings.Contains(err.Error(), "image not found") {
		t.Errorf("error must surface runtime CLI output for diagnostics; got: %v", err)
	}
}

// TestContainerized_Teardown_InvokesRuntimeRmF asserts Teardown
// invokes `podman rm -f <containerName>`. Mutation rehearsal: drop
// the `-f` flag in production; this test fails with "captured args
// missing -f". The flag is load-bearing because the container may
// still be running (`--rm` from Boot cleans up on STOP, not on the
// matrix runner's abandoned-process scenarios).
func TestContainerized_Teardown_InvokesRuntimeRmF(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"podman": {Out: []byte("container-id\n"), Err: nil},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	// Drive Boot first so containerName is populated.
	if _, err := c.Boot(context.Background(), AVD{Name: "test-avd"}, true); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	containerNameAtBoot := c.ContainerName()

	if err := c.Teardown(context.Background(), 0); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	// Teardown should have cleared the name.
	if c.ContainerName() != "" {
		t.Errorf("ContainerName must be cleared after Teardown; got %q", c.ContainerName())
	}

	// Find the `podman rm -f <name>` call.
	var rmCall *fakeCall
	for i := range fake.calls {
		if fake.calls[i].Name == "podman" && len(fake.calls[i].Args) >= 1 && fake.calls[i].Args[0] == "rm" {
			rmCall = &fake.calls[i]
			break
		}
	}
	if rmCall == nil {
		t.Fatalf("Teardown did not invoke `podman rm`; calls: %+v", fake.calls)
	}
	hasF := false
	hasContainerName := false
	for _, a := range rmCall.Args {
		if a == "-f" {
			hasF = true
		}
		if a == containerNameAtBoot {
			hasContainerName = true
		}
	}
	if !hasF {
		t.Errorf("Teardown must use -f for force-removal; args: %v", rmCall.Args)
	}
	if !hasContainerName {
		t.Errorf("Teardown must target the container name from Boot (%q); args: %v",
			containerNameAtBoot, rmCall.Args)
	}
}

// TestContainerized_Teardown_NoOpWhenNotBooted asserts the
// idempotent-Teardown contract — calling Teardown on a fresh
// Containerized (no prior Boot) is a no-op SUCCESS, NOT an error.
// Callers defer Teardown defensively; the contract supports that.
func TestContainerized_Teardown_NoOpWhenNotBooted(t *testing.T) {
	fake := &fakeExecutor{}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	if err := c.Teardown(context.Background(), 0); err != nil {
		t.Fatalf("Teardown on fresh instance must be no-op SUCCESS, got: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("Teardown on fresh instance must not invoke the runtime; got %d calls", len(fake.calls))
	}
}

// TestContainerized_WaitForBoot_PollsGetpropUntilCompleted exercises
// the polling loop with a sequenced script. The 1st poll returns
// empty (boot not yet complete); the 2nd returns "1" (complete).
// The function returns nil error + a non-zero duration.
//
// Mutation rehearsal: change the comparison in WaitForBoot from
// `== "1"` to `!= "1"`. The test fails: "expected nil error, got
// WaitForBoot timed out…" because the loop never finds the success
// condition. The captured wire-observable is `adb shell getprop
// sys.boot_completed` returning "1" — that's the user-observable
// signal Android emits when boot completes.
func TestContainerized_WaitForBoot_PollsGetpropUntilCompleted(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555": {Out: []byte("connected to localhost:5555\n")},
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
		Image:         "any:tag",
		Executor:      fake,
	})
	d, err := c.WaitForBoot(context.Background(), 5555, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitForBoot returned error: %v", err)
	}
	if d <= 0 {
		t.Errorf("expected positive duration, got %v", d)
	}
}

// TestContainerized_WaitForBoot_FailsOnTimeout asserts the
// fail-loud-on-timeout posture: WaitForBoot MUST NOT report success
// without observing sys.boot_completed=1. Per clause 6.J/6.B
// (container "Up" is not application-healthy), a missing positive
// signal at the wire is a failure, not a "best effort" pass.
func TestContainerized_WaitForBoot_FailsOnTimeout(t *testing.T) {
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			"adb connect localhost:5555":                             {Out: []byte("connected\n")},
			"adb -s localhost:5555 shell getprop sys.boot_completed": {Out: []byte("\n")},
		},
	}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	_, err := c.WaitForBoot(context.Background(), 5555, 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForBoot must error on timeout; got nil error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error must mention timeout for diagnostics; got: %v", err)
	}
}

// TestContainerized_authorizeADB_CopiesBakedKeyAndSetsVendorKeys is the
// RC2 anti-bluff guard (2026-06-23 thinker.local blocker). The image
// bakes a fixed adb keypair the AVD authorises; the runner MUST copy the
// matching PRIVATE key out of the container and point host adb at it via
// ADB_VENDOR_KEYS, otherwise `adb connect` returns `offline` forever and
// WaitForBoot times out. The §6.J primary assertion is the wire-observable
// `podman cp <container>:/home/emulator/.android/adbkey <host>` command.
//
//	FALSIFIABILITY REHEARSAL: remove the c.authorizeADB(ctx) call from
//	WaitForBoot (or the `cp` Execute in authorizeADB) → no cp call is
//	recorded, this test fails ("expected podman cp of the baked adbkey"),
//	and the real thinker.local run reverts to `device offline` / timeout.
func TestContainerized_authorizeADB_CopiesBakedKeyAndSetsVendorKeys(t *testing.T) {
	fake := &fakeExecutor{scripts: map[string]fakeScript{}}
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
		Executor:      fake,
	})
	// Simulate a prior Boot having set the container name.
	c.containerName = "lava-emu-Pixel_8-123"

	// authorizeADB may return a non-nil error in this fake environment
	// (os.Chmod fails because the fake `cp` writes no real file) — the
	// load-bearing assertion is the COMMAND the runner issued, not the
	// fake-filesystem side effect.
	_ = c.authorizeADB(context.Background())

	// §6.J primary assertion: the baked private key was copied out of the
	// exact container at the exact image path.
	wantSrc := "lava-emu-Pixel_8-123:" + containerADBKeyPath
	foundCp := false
	for _, call := range fake.calls {
		if call.Name == "podman" && len(call.Args) >= 2 &&
			call.Args[0] == "cp" && call.Args[1] == wantSrc {
			foundCp = true
		}
	}
	if !foundCp {
		t.Errorf("expected podman cp of the baked adbkey (%q) so host adb can authorise the guest; calls=%v", wantSrc, fake.calls)
	}

	// ADB_VENDOR_KEYS MUST be set so the host adb client presents the
	// baked key on connect.
	if os.Getenv("ADB_VENDOR_KEYS") == "" {
		t.Errorf("ADB_VENDOR_KEYS must be set to the copied baked key path; got empty")
	}
}

// TestContainerized_Install_ChecksSuccessMarker asserts Install
// observes "Success" in `adb install` output before reporting
// success. Mutation rehearsal: drop the `bytes.Contains(out,
// "Success")` check. The test fails because the fake returns
// non-Success output but no shell error — the resulting nil error
// would silently mislead a caller into thinking install succeeded
// when it didn't (canonical §6.J primary bluff: green test, broken
// feature).
func TestContainerized_Install_ChecksSuccessMarker(t *testing.T) {
	t.Run("success when adb reports Success", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"adb -s localhost:5555 install -r /tmp/test.apk": {
					Out: []byte("Performing Streamed Install\nSuccess\n"),
				},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
		})
		if err := c.Install(context.Background(), 5555, "/tmp/test.apk"); err != nil {
			t.Errorf("Install must succeed on Success output; got: %v", err)
		}
	})
	t.Run("failure when adb does not report Success", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"adb -s localhost:5555 install -r /tmp/test.apk": {
					Out: []byte("Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]\n"),
				},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
		})
		err := c.Install(context.Background(), 5555, "/tmp/test.apk")
		if err == nil {
			t.Fatal("Install must error when adb did NOT report Success")
		}
		if !strings.Contains(err.Error(), "Success") {
			t.Errorf("error must surface why install was rejected; got: %v", err)
		}
	})
}

// TestContainerized_RunInstrumentation_SetsAndroidSerialAndChecksBuildSuccessful
// asserts the dual-signal pass criterion: gradle must exit 0 AND
// "BUILD SUCCESSFUL" must appear in output. Either failing flips
// passed=false. This guards against the 2026-04-29 "BUILD SUCCESSFUL
// is not enough" bluff vector — see parent CLAUDE.md §6.A.
func TestContainerized_RunInstrumentation_SetsAndroidSerialAndChecksBuildSuccessful(t *testing.T) {
	t.Run("passed=true when gradle exits 0 AND BUILD SUCCESSFUL present", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				// The shell-c expression includes ANDROID_SERIAL — match name only
				// (the key for shell -c is wide); rely on the call.Args inspection below.
				"/bin/sh": {Out: []byte("> Task :app:connectedDebugAndroidTest\nBUILD SUCCESSFUL in 12s\n")},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
		})
		output, passed, err := c.RunInstrumentation(
			context.Background(), 5555,
			"lava.app.challenges.Challenge01AppLaunchAndTrackerSelectionTest",
			60*time.Second,
		)
		if err != nil {
			t.Fatalf("RunInstrumentation: %v", err)
		}
		if !passed {
			t.Errorf("passed must be true when BUILD SUCCESSFUL is in output; got false")
		}
		if !strings.Contains(output, "BUILD SUCCESSFUL") {
			t.Errorf("output must surface gradle stdout for forensics")
		}
		// Verify ANDROID_SERIAL was set in the synthesized shell command.
		shCall := firstCallMatching(t, fake.calls, "/bin/sh")
		// shCall.Args is ["-c", "ANDROID_SERIAL=localhost:5555 './gradlew' ':app:...' ..."]
		if len(shCall.Args) < 2 || !strings.Contains(shCall.Args[1], "ANDROID_SERIAL=localhost:5555") {
			t.Errorf("shell command must set ANDROID_SERIAL=localhost:5555; got: %v", shCall.Args)
		}
		if !strings.Contains(shCall.Args[1], "lava.app.challenges.Challenge01AppLaunchAndTrackerSelectionTest") {
			t.Errorf("shell command must include the test class FQN; got: %v", shCall.Args)
		}
	})
	t.Run("passed=false when gradle exits 0 but BUILD SUCCESSFUL absent", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"/bin/sh": {Out: []byte("test output without success marker")},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
		})
		_, passed, err := c.RunInstrumentation(
			context.Background(), 5555, "any.test.Class", 60*time.Second,
		)
		if passed {
			t.Error("passed must be false when BUILD SUCCESSFUL is absent")
		}
		if err == nil {
			t.Error("err must be non-nil when BUILD SUCCESSFUL is absent")
		}
	})
	t.Run("passed=false when gradle exits non-zero", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"/bin/sh": {Out: []byte("BUILD FAILED\n"), Err: errors.New("exit 1")},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
		})
		_, passed, err := c.RunInstrumentation(
			context.Background(), 5555, "any.test.Class", 60*time.Second,
		)
		if passed {
			t.Error("passed must be false when gradle exits non-zero")
		}
		if err == nil {
			t.Error("err must be non-nil when gradle exits non-zero")
		}
	})
}

// TestContainerized_RunInstrumentation_HonorsGradleModule pins the
// §6.X-debt / decoupling fix: the CLI's --gradle-module flag (and the
// MatrixConfig.GradleModule field it threads into) MUST change the
// gradle task the emulator invokes. A consumer that ships its
// instrumentation under a non-"app" module (e.g. the consuming project's :api-app
// challenge classes) discovers ZERO tests when the module is hardwired
// to :app — a false-green by construction. This test drives the
// non-default module and asserts the synthesized shell command targets
// `:api-app:connectedDebugAndroidTest` and NOT `:app:`.
//
// Generic/decoupled: the test uses a neutral consumer module name; the
// production code MUST NOT special-case it.
func TestContainerized_RunInstrumentation_HonorsGradleModule(t *testing.T) {
	t.Run("non-default module substitutes the gradle task", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"/bin/sh": {Out: []byte("> Task :api-app:connectedDebugAndroidTest\nBUILD SUCCESSFUL in 9s\n")},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
			GradleModule:  "api-app",
		})
		_, _, err := c.RunInstrumentation(
			context.Background(), 5555, "some.consumer.ChallengeTest", 60*time.Second,
		)
		if err != nil {
			t.Fatalf("RunInstrumentation: %v", err)
		}
		shCall := firstCallMatching(t, fake.calls, "/bin/sh")
		if len(shCall.Args) < 2 {
			t.Fatalf("expected shell -c form; got args: %v", shCall.Args)
		}
		cmd := shCall.Args[1]
		if !strings.Contains(cmd, ":api-app:connectedDebugAndroidTest") {
			t.Errorf("gradle command must target :api-app:connectedDebugAndroidTest; got: %q", cmd)
		}
		if strings.Contains(cmd, ":app:connectedDebugAndroidTest") {
			t.Errorf("gradle command must NOT target the hardwired :app:connectedDebugAndroidTest; got: %q", cmd)
		}
	})
	t.Run("empty module preserves the :app default", func(t *testing.T) {
		fake := &fakeExecutor{
			scripts: map[string]fakeScript{
				"/bin/sh": {Out: []byte("> Task :app:connectedDebugAndroidTest\nBUILD SUCCESSFUL in 9s\n")},
			},
		}
		c, _ := NewContainerized(ContainerizedConfig{
			RuntimeBinary: "podman",
			Image:         "any:tag",
			Executor:      fake,
			// GradleModule omitted — defaults to "app".
		})
		_, _, err := c.RunInstrumentation(
			context.Background(), 5555, "some.consumer.ChallengeTest", 60*time.Second,
		)
		if err != nil {
			t.Fatalf("RunInstrumentation: %v", err)
		}
		shCall := firstCallMatching(t, fake.calls, "/bin/sh")
		if len(shCall.Args) < 2 || !strings.Contains(shCall.Args[1], ":app:connectedDebugAndroidTest") {
			t.Errorf("empty module must default to :app:connectedDebugAndroidTest; got: %v", shCall.Args)
		}
	})
}

// TestContainerized_satisfies_Emulator is the compile-time + runtime
// assertion that Containerized fits the Emulator interface — without
// it a future refactor of either side could silently break the matrix
// runner's ability to swap in Containerized. Compile-time check is
// at the bottom of containerized.go (`var _ Emulator = ...`); this
// test ensures the runtime contract is invokable through the
// interface, not just structurally typed.
func TestContainerized_satisfies_Emulator(t *testing.T) {
	c, _ := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         "any:tag",
	})
	var _ Emulator = c
}

// TestSanitizeContainerName asserts the helper rejects characters
// that podman/docker would reject in `--name`. Real-world AVDs
// include underscores, dashes, and (rarely) spaces — all need
// canonicalization before they reach the runtime CLI.
func TestSanitizeContainerName(t *testing.T) {
	cases := map[string]string{
		"Pixel_API34_Phone": "Pixel_API34_Phone", // alnum + underscore preserved
		"Pixel 9a":          "Pixel-9a",          // space → dash
		"My AVD (test)":     "My-AVD--test-",     // parens → dash
		"a.b.c":             "a-b-c",             // dots → dash
		"valid-name":        "valid-name",        // dashes preserved
	}
	for input, expected := range cases {
		t.Run(input, func(t *testing.T) {
			got := sanitizeContainerName(input)
			if got != expected {
				t.Errorf("sanitizeContainerName(%q) = %q, want %q", input, got, expected)
			}
		})
	}
}

// TestShellQuote_HandlesSingleQuotes asserts the canonical bash
// escape sequence is used. A wrongly-escaped path could let an AVD
// name carry shell-metachar exploitation into a gradle run — that's
// a security issue, not just a bug. The escape `'"'"'` is the
// POSIX-portable single-quote-in-single-quoted-string form.
func TestShellQuote_HandlesSingleQuotes(t *testing.T) {
	got := shellQuote("a'b")
	expected := `'a'"'"'b'`
	if got != expected {
		t.Errorf("shellQuote(\"a'b\") = %q, want %q", got, expected)
	}
}
