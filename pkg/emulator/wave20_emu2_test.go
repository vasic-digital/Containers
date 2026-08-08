package emulator

// Batch EMU-2 (Wave-20 DEEPER, §11.4.118 2nd-pass) — §11.4.115 RED→GREEN
// behavioral guard for EMU2-1 (MED, SECURITY argv flag-injection):
// buildContainerRunArgs placed `image` as a BARE trailing positional in the
// `podman/docker run` argv, with NO end-of-options "--" before it. `image`
// flows unsanitized from ContainerizedConfig.Image (a consumer vm-images.json
// manifest entry / --image CLI flag) straight through Containerized.Boot →
// buildContainerRunArgs; a crafted or typo'd reference beginning with '-'
// (e.g. "--privileged", "--network=host", "-v/:/host") is parsed by the
// container CLI as a RUNTIME FLAG rather than the image name → a
// privilege/mount/network-escalation vector. Fix: insert "--" before the
// image positional so the CLI can never interpret a hostile ref as a flag
// (worst case becomes an honest "no such image"). Direct mirror of
// pkg/crossbuild's XBUILD2-2 (container_runner.go / apple_container.go) which
// closed the SAME class in the crossbuild builders but never touched this
// sibling emulator container path.
//
// The existing buildContainerRunArgs coverage (containerized_test.go +
// containerized_kvm_test.go) asserts only membership ("--device" present, the
// image ref present, "-e ANDROID_COLD_BOOT=true" present) — none ever checked
// that the image positional is guarded by an end-of-options "--", the exact
// coverage gap that let the flag-injection ship.
//
// Bluff-Audit (§11.4.115 — surgical revert ACTUALLY applied + captured +
// reverted; conductor re-verifies):
//
//	Mutation (EMU2-1): removed the single-line `"--",` end-of-options guard
//	          inserted before `image` in buildContainerRunArgs, restoring the
//	          bare-trailing-positional shape of the defect.
//	Observed (surgical revert applied; `go test ./pkg/emulator/ -run
//	          '^TestWave20_EMU2_ImageRefFlagInjectionGuardedByEndOfOptions$'
//	          -count=1`):
//	            --- FAIL: TestWave20_EMU2_ImageRefFlagInjectionGuardedByEndOfOptions
//	              EMU2-1: the image positional "-v/:/host" MUST be immediately
//	              preceded by an end-of-options "--" ...; element before image
//	              is "ANDROID_COLD_BOOT=true"
//	          (with the guard removed the CLI would parse the leading-'-'
//	          image ref as a runtime flag — the escalation vector.)
//	Reverted: yes — re-inserted `"--",`, re-ran, GREEN.

import "testing"

// assertImageGuardedByEndOfOptions verifies the container-run argv places the
// image as a positional immediately preceded by an end-of-options "--" AND as
// the FINAL element (Boot appends no command), so a hostile leading-'-' image
// reference can never be parsed by the container CLI as a runtime flag.
func assertImageGuardedByEndOfOptions(t *testing.T, args []string, image string) {
	t.Helper()
	idx := -1
	for i, a := range args {
		if a == image {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("EMU2-1: image %q not present in container run args: %v", image, args)
	}
	if idx == 0 {
		t.Fatalf("EMU2-1: image %q is at index 0 with no preceding end-of-options guard: %v", image, args)
	}
	if args[idx-1] != "--" {
		t.Fatalf("EMU2-1: the image positional %q MUST be immediately preceded by an "+
			"end-of-options \"--\" so a crafted ref beginning with '-' cannot be parsed "+
			"as a runtime flag (argv flag-injection); element before image is %q; args=%v",
			image, args[idx-1], args)
	}
	if idx != len(args)-1 {
		t.Fatalf("EMU2-1: the image MUST be the final positional after \"--\" (Boot appends "+
			"no command); trailing args after image present: %v", args[idx:])
	}
}

// TestWave20_EMU2_ImageRefFlagInjectionGuardedByEndOfOptions — the
// `podman/docker run` argv buildContainerRunArgs produces MUST guard the image
// positional with an end-of-options "--" so an unsanitized, consumer-supplied
// image reference beginning with '-' cannot be interpreted as a runtime flag.
func TestWave20_EMU2_ImageRefFlagInjectionGuardedByEndOfOptions(t *testing.T) {
	avd := AVD{Name: "Pixel_API34_Phone", APILevel: 34, FormFactor: "phone"}

	// Case 1 — a benign image ref MUST still be "--"-guarded (defense in depth:
	// the guard is unconditional, not "only when the ref looks hostile").
	const benign = "ghcr.io/vasic-digital/lava-android-emulator:api34-phone"
	assertImageGuardedByEndOfOptions(t,
		buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, benign), benign)

	// Case 2 — the HOSTILE vectors: a reference beginning with '-'. Without the
	// "--" guard the CLI parses these as flags:
	//   "-v/:/host"      → bind-mounts host root into the container
	//   "--privileged"   → grants all capabilities
	//   "--network=host" → shares the host network namespace
	for _, hostile := range []string{"-v/:/host", "--privileged", "--network=host"} {
		// podman path (adds --userns=keep-id) AND docker path (does not) MUST
		// both guard identically.
		assertImageGuardedByEndOfOptions(t,
			buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, hostile), hostile)
		assertImageGuardedByEndOfOptions(t,
			buildContainerRunArgs("docker", "lava-emu-1", 6555, avd, false, hostile), hostile)
	}
}
