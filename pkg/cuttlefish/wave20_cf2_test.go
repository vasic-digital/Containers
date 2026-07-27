package cuttlefish

// Batch CF2 (Wave-20 DEEPER, §11.4.118 2nd-pass) — §11.4.115 RED→GREEN
// behavioral guards for a fresh §11.4.118 discovery pass over pkg/cuttlefish
// AFTER the CF-HARD + §11.4.174 process-kill cluster already hardened it once.
//
// Both guards are GREEN against the fixed code and were proven RED by a
// surgical single-line revert of the corresponding fix (captured `--- FAIL`
// line recorded in the CF2 report block). Guards drive the pure
// buildContainerRunArgs builder + the injectable CommandExecutor seam — no real
// adb/podman/crosvm (§11.4.27).
//
//   CF2-1 (MED, §11.4.108) — Config.NetworkHost, documented "default true"
//     ("Cuttlefish requires it"), was never defaulted → a caller omitting it
//     launched WITHOUT `--network host` (cvd networking dead) while Launch
//     still reported Started=true. The SOURCE-says-default vs ARTIFACT-omits-
//     the-flag gap.
//   CF2-2 (MED, security/argv-injection) — buildContainerRunArgs appended the
//     unsanitized Config.Image as the trailing `podman/docker run` positional
//     with NO end-of-options "--" guard, so a leading-dash image ref is parsed
//     as a RUNTIME FLAG. The sibling container paths (pkg/emulator EMU2-1,
//     pkg/crossbuild XBUILD2-2) already carry this "--"; the Cuttlefish path
//     did not.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CF2-1 (MED, §11.4.108) — a Cuttlefish constructed WITHOUT explicitly setting
// Config.NetworkHost MUST still launch with `--network host` in the actual run
// command (the RUNTIME SIGNATURE — the emitted argv, not merely the struct
// field), because the container physically cannot make the cvd-ebr bridge / cvd
// networking without it and would report Started=true while never coming
// online.
//
// Bluff-Audit (YOUR run, recorded in the CF2 report):
//
//	Mutation: in NewCuttlefish, revert `cfg.NetworkHost = true` (inside the
//	          `if !cfg.NetworkHost {}` default block) to `cfg.NetworkHost =
//	          false` — a behavioral no-op that restores the pre-fix "never
//	          defaulted" state.
//	Observed: --- FAIL: TestWave20_CF2_HostNetworkingDefaultedIntoRunArgs
//	          CF2-1: a Cuttlefish launched WITHOUT explicitly setting
//	          NetworkHost MUST still emit `--network host` ... got [run -d
//	          --name cvd-default --rm --privileged --device ... -- cuttlefish:latest]
//	          (no `--network host` pair — the documented "default true" controlled
//	          nothing).
//	Reverted: yes (restored to `cfg.NetworkHost = true`).
func TestWave20_CF2_HostNetworkingDefaultedIntoRunArgs(t *testing.T) {
	withStatDevice(t, DefaultDevices()...) // all nodes present
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) {
		return []byte("container-id"), nil
	}}
	// NetworkHost intentionally OMITTED — it must default to true.
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: "cuttlefish:latest",
		Privileged: true, Executor: fe,
	})
	require.NoError(t, err)

	// The struct field is defaulted...
	assert.True(t, c.cfg.NetworkHost,
		"CF2-1: Config.NetworkHost MUST default to true (Cuttlefish requires --network host)")

	// ...AND the actual emitted run command carries the flag (runtime signature).
	_, err = c.Launch(context.Background())
	require.NoError(t, err)
	require.Len(t, fe.calls, 1)
	assert.Equal(t, "podman", fe.calls[0].Name)
	assert.True(t, argsContainAdjacent(fe.calls[0].Args, "--network", "host"),
		"CF2-1: a Cuttlefish launched WITHOUT explicitly setting NetworkHost MUST still emit `--network host` (documented default true); got %v",
		fe.calls[0].Args)
}

// CF2-2 (MED, security/argv flag-injection) — buildContainerRunArgs MUST place
// an end-of-options `--` immediately before the image positional so a
// leading-dash Config.Image (crafted or typo'd, e.g. "-v/:/host") is parsed as
// the IMAGE, never as a runtime flag (the privilege/mount/network-escalation
// vector). Mirrors pkg/emulator EMU2-1 + pkg/crossbuild XBUILD2-2.
//
// Bluff-Audit (YOUR run, recorded in the CF2 report):
//
//	Mutation: in buildContainerRunArgs, remove the `args = append(args, "--")`
//	          line (its pre-fix absence — image appended straight after the
//	          --group-add pairs).
//	Observed: --- FAIL: TestWave20_CF2_EndOfOptionsGuardBeforeImage
//	          CF2-2: buildContainerRunArgs MUST place `--` immediately before the
//	          image positional ... got [... --group-add kvm -v/:/host]
//	          (element before the image was "kvm", not "--"; the hostile image
//	          ref is exposed to the runtime's flag parser).
//	Reverted: yes.
func TestWave20_CF2_EndOfOptionsGuardBeforeImage(t *testing.T) {
	// A leading-dash image ref: absent the guard, `podman/docker run` parses
	// "-v/:/host" as a bind-mount flag (container escape), not the image.
	const hostileImage = "-v/:/host"
	args := buildContainerRunArgs("cvd-x", hostileImage,
		[]string{"/dev/kvm"}, []string{"kvm"}, true, true)

	// The image stays the FINAL positional (public contract preserved)...
	require.Equal(t, hostileImage, args[len(args)-1],
		"CF2-2: the image must remain the final positional arg; got %v", args)
	// ...shielded by an end-of-options "--" immediately before it.
	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, "--", args[len(args)-2],
		"CF2-2: buildContainerRunArgs MUST place `--` immediately before the image positional so a leading-dash image is parsed as the IMAGE, not a runtime flag; got %v",
		args)

	// Defense-in-depth: the guard reaches the REAL emitted command too (runtime
	// signature), not just the pure builder — same single-line anchor, so this
	// FAILs on the same revert.
	withStatDevice(t, "/dev/kvm")
	fe := &fakeExecutor{fn: func(_ string, _ []string) ([]byte, error) { return []byte("id"), nil }}
	c, err := NewCuttlefish(Config{
		RuntimeBinary: "podman", Image: hostileImage,
		Privileged: true, NetworkHost: true, Executor: fe,
	})
	require.NoError(t, err)
	_, err = c.Launch(context.Background())
	require.NoError(t, err)
	require.Len(t, fe.calls, 1)
	ra := fe.calls[0].Args
	require.Equal(t, hostileImage, ra[len(ra)-1])
	assert.Equal(t, "--", ra[len(ra)-2],
		"CF2-2: the emitted run command MUST carry `--` before the hostile image; got %v", ra)
}
