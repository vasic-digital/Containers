// containerized_userns_test.go — tests for the rootless-podman
// --userns=keep-id remap in buildContainerRunArgs.
//
// Forensic anchor (FACT): the HelixCode Android client was recorded by
// booting an Android emulator inside a ROOTLESS podman container. The
// container image's USER (uid 1000) maps to a host subuid that lacks the
// invoking host user's /dev/kvm ACL, so ProbeKVM failed with "This user
// doesn't have permissions to use KVM" and Boot never started.
// `--userns=keep-id` maps the container uid back to the invoking host
// user (who holds the /dev/kvm ACL), fixing rootless boot. This is a
// generic rootless-podman improvement (§11.4.161), NOT project-specific.
//
// Anti-bluff posture (§6.J/§6.L + §11.4.161): these tests assert on the
// actual `podman run` / `docker run` argument slice the production Boot
// path constructs. The euid probe is overridden through the package-level
// rootlessUID seam so both the rootless (non-zero euid) and rootful
// (euid 0) branches are exercised deterministically (§11.4.50) without
// changing the real process identity.
package emulator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// withRootlessUID temporarily redirects the package-level rootlessUID
// probe to return a fixed euid and restores it on cleanup. This is the
// seam that lets the test exercise both the rootless (non-root euid) and
// rootful (euid 0) branches without changing the real process identity.
func withRootlessUID(t *testing.T, uid int) {
	t.Helper()
	orig := rootlessUID
	rootlessUID = func() int { return uid }
	t.Cleanup(func() { rootlessUID = orig })
}

// argsContainUsernsKeepID reports whether the arg slice contains the
// "--userns=keep-id" flag (single token, podman's accepted form).
func argsContainUsernsKeepID(args []string) bool {
	for _, a := range args {
		if a == "--userns=keep-id" {
			return true
		}
	}
	return false
}

func TestBuildContainerRunArgs_RootlessPodman_AddsUsernsKeepID(t *testing.T) {
	// Rootless podman: invoking process is non-root (euid 1000) and the
	// runtime is podman. The container USER (uid 1000) must be remapped
	// back to the invoking host user so it inherits the /dev/kvm ACL.
	withRootlessUID(t, 1000)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	assert.True(t, argsContainUsernsKeepID(args),
		"under rootless podman, buildContainerRunArgs MUST include --userns=keep-id so the container uid inherits the invoking user's /dev/kvm ACL; got %v", args)
	// The rest of the launch wiring is unaffected by the userns decision.
	assert.Contains(t, args, "ANDROID_AVD_NAME=Pixel_8")
	assert.Contains(t, args, "6555:5555/tcp")
	assert.Contains(t, args, "lava/emulator:api35")
}

func TestBuildContainerRunArgs_RootlessPodman_PathBinary_AddsUsernsKeepID(t *testing.T) {
	// The runtime may be an absolute path (e.g. detected on PATH). The
	// basename must still be recognised as podman.
	withRootlessUID(t, 1000)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("/usr/bin/podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	assert.True(t, argsContainUsernsKeepID(args),
		"a path-form podman binary under rootless mode MUST still get --userns=keep-id; got %v", args)
}

func TestBuildContainerRunArgs_RootfulPodman_OmitsUsernsKeepID(t *testing.T) {
	// Rootful podman: invoking process IS root (euid 0). The container
	// uid already maps to the host root, which holds the /dev/kvm ACL, so
	// keep-id is unnecessary — omit it to keep rootful behaviour byte-for-
	// byte unchanged.
	withRootlessUID(t, 0)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	assert.False(t, argsContainUsernsKeepID(args),
		"under rootful podman (euid 0), buildContainerRunArgs MUST omit --userns=keep-id; got %v", args)
}

func TestBuildContainerRunArgs_Docker_OmitsUsernsKeepID(t *testing.T) {
	// Docker uses a root daemon and a different user-namespace model;
	// --userns=keep-id is a podman-rootless concept and does not apply.
	// Even with a non-root euid, the docker path must omit it.
	withRootlessUID(t, 1000)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("docker", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	assert.False(t, argsContainUsernsKeepID(args),
		"the docker runtime MUST omit --userns=keep-id (podman-rootless-only concept); got %v", args)
}
