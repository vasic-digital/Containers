// containerized_kvm_test.go — tests for the §6.AH-debt conditional KVM
// passthrough in buildContainerRunArgs.
//
// Anti-bluff posture (§6.J/§6.L + §6.N containers variant): these tests
// assert on the actual `podman run` / `docker run` argument slice the
// production Boot path constructs — the user-visible behaviour being
// "the container is launched with hardware acceleration ONLY where the
// host can provide it, and falls back cleanly (no --device /dev/kvm)
// where it cannot (macOS podman VM)". The FALSIFIABILITY REHEARSAL is
// recorded in the commit body (Bluff-Audit stamp).
package emulator

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withKVMDevicePath temporarily redirects the package-level kvmDevicePath
// to a caller-supplied path and restores it on cleanup. This is the seam
// that lets the test exercise both the present and absent branches
// without a real /dev/kvm.
func withKVMDevicePath(t *testing.T, path string) {
	t.Helper()
	orig := kvmDevicePath
	kvmDevicePath = path
	t.Cleanup(func() { kvmDevicePath = orig })
}

// argsContainDeviceKVM reports whether the arg slice contains the
// adjacent pair "--device" <kvmDevicePath>.
func argsContainDeviceKVM(args []string, devicePath string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--device" && args[i+1] == devicePath {
			return true
		}
	}
	return false
}

func TestContainerized_KVMPresence_Present(t *testing.T) {
	// Point kvmDevicePath at a file that exists → kvmAvailable() true.
	present := t.TempDir() + "/kvm"
	require.NoError(t, os.WriteFile(present, nil, 0o644))
	withKVMDevicePath(t, present)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	// Primary assertion: hardware acceleration is requested on a host
	// that exposes the KVM device (Linux x86_64 gate path).
	assert.True(t, argsContainDeviceKVM(args, present),
		"when /dev/kvm exists, buildContainerRunArgs MUST include --device <kvm> for hardware acceleration; got %v", args)
	// The port-forward + AVD env wiring is present regardless of KVM.
	assert.Contains(t, args, "ANDROID_AVD_NAME=Pixel_8")
	assert.Contains(t, args, "6555:5555/tcp")
}

func TestContainerized_KVMPresence_Absent(t *testing.T) {
	// Point kvmDevicePath at a path that does NOT exist → kvmAvailable()
	// false. This is the macOS podman-VM reality (no /dev/kvm, HVF
	// unreachable from a Linux container) the §6.AH-debt fix addresses.
	absent := t.TempDir() + "/this-kvm-does-not-exist"
	withKVMDevicePath(t, absent)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}
	args := buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")

	// Primary assertion: NO --device /dev/kvm is injected when the
	// device is absent — otherwise `podman run` fails outright on macOS
	// ("error: stat /dev/kvm: no such file or directory"), which is the
	// exact boot-failure class §6.AH-debt exists to avoid. Falling back
	// to TCG software emulation requires the flag to be omitted.
	assert.False(t, argsContainDeviceKVM(args, absent),
		"when /dev/kvm is absent, buildContainerRunArgs MUST omit --device <kvm> so the container falls back to TCG; got %v", args)
	// The rest of the launch wiring is unaffected by the KVM decision.
	assert.Contains(t, args, "ANDROID_AVD_NAME=Pixel_8")
	assert.Contains(t, args, "6555:5555/tcp")
	assert.Contains(t, args, "lava/emulator:api35")
}

// TestContainerized_Userns_KeepIdForPodman is the RC1 anti-bluff guard
// (2026-06-23 thinker.local blocker). On rootless podman the emulator
// could not access /dev/kvm because the host grants it via a named-user
// ACL, not kvm-group membership; --userns=keep-id maps the container uid
// back to the host invoking uid so the ACL applies. This was PROVEN
// manually ("Boot completed in 29345 ms" with the flag; KVM-not-writable
// without it).
//
//	FALSIFIABILITY REHEARSAL: drop the `if runtimeBinary == "podman"`
//	--userns=keep-id append in buildContainerRunArgs → this test fails
//	("podman run MUST include --userns=keep-id ..."), and the real
//	thinker.local C00 run reverts to KVM-not-writable / boot failure.
func TestContainerized_Userns_KeepIdForPodman(t *testing.T) {
	// KVM present so the full Linux x86_64 gate-path args are built.
	present := t.TempDir() + "/kvm"
	require.NoError(t, os.WriteFile(present, nil, 0o644))
	withKVMDevicePath(t, present)

	avd := AVD{Name: "Pixel_8", APILevel: 35, FormFactor: "phone"}

	// podman MUST carry --userns=keep-id so the host /dev/kvm named-user
	// ACL applies inside the rootless container.
	podmanArgs := buildContainerRunArgs("podman", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")
	assert.Contains(t, podmanArgs, "--userns=keep-id",
		"podman run MUST include --userns=keep-id so the host named-user /dev/kvm ACL applies inside the rootless container; got %v", podmanArgs)

	// docker uses a different userns model and rejects --userns=keep-id;
	// it MUST NOT be injected for the docker runtime.
	dockerArgs := buildContainerRunArgs("docker", "lava-emu-1", 6555, avd, true, "lava/emulator:api35")
	assert.NotContains(t, dockerArgs, "--userns=keep-id",
		"docker run MUST NOT include --userns=keep-id (rejected by the docker daemon userns model); got %v", dockerArgs)
}
