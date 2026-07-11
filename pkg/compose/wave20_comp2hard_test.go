package compose

// Wave-20 DEEPER (§11.4.118 loop-until-dry) 2nd-pass guard tests. The COMP /
// CO first pass hardened append-aliasing, timeout-bounded detection probes,
// podman-shim detection, and the service-scoped / WaitTimeout-distinct
// podman host-side wait. This file guards a defect that pass MISSED.
//
// COMP2-1 (§11.4.108 / hint "ctx-timeout ignored on a path beyond COMP-2/3"):
// CO-2 introduced WaitTimeout as the caller's wait-deadline and wired it to
// the podman host-side poll (waitForServices), but the docker NATIVE `--wait`
// path never emitted `--wait-timeout`, so WithWaitTimeout was silently dropped
// on docker and `up --wait` could block far past the caller's requested
// deadline. docker compose's `--wait-timeout <seconds>` is a real companion
// of `--wait` ("Maximum duration in seconds to wait for the project to be
// running|healthy" -- docs.docker.com/reference/cli/docker/compose/up,
// verified 2026-07-11).
//
// Surgical-revert RED evidence (YOUR run, recorded in the batch report):
//   COMP2-1: delete the single anchor line
//            `args = append(args, "--wait-timeout", strconv.Itoa(cfg.WaitTimeout))`
//            in Up -> TestWave20_COMP2_DockerWaitTimeoutEmitted FAILs (the
//            docker native path stops forwarding the caller's deadline);
//            build stays green (cfg.WaitTimeout still referenced by the guard
//            condition, strconv still used by the --timeout emitter). Restore
//            -> GREEN.
//
// Seam (§11.4.107 honest boundary / §11.4.27 no real docker): a fake compose
// script records the `up` argv; the guard asserts the emitted flags. The real
// Up -> projectArgs -> flag-assembly -> run path runs end-to-end; only the
// external compose binary is a stand-in. Reuses the hermetic captureUpArgs
// helper (podman_compose_test.go).

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWave20_COMP2_DockerWaitTimeoutEmitted proves the docker native `--wait`
// path forwards the caller's WithWaitTimeout as `--wait-timeout <n>` so the
// wait-deadline is honoured on docker, not silently dropped. captureUpArgs
// classifies ("docker",["compose"]) as NON-podman, exercising the docker
// native-wait branch of Up.
func TestWave20_COMP2_DockerWaitTimeoutEmitted(t *testing.T) {
	args := captureUpArgs(t,
		"docker", []string{"compose"},
		ComposeProject{Name: "p", File: "compose.yml"},
		WithWait(true), WithWaitTimeout(30),
	)

	assert.Contains(t, args, "--wait",
		"docker Up(WithWait) must still emit --wait")
	assert.Contains(t, args, "--wait-timeout 30",
		"COMP2-1: docker Up(WithWait, WithWaitTimeout(30)) MUST forward the "+
			"caller's wait-deadline as the native --wait-timeout flag -- "+
			"otherwise the deadline is silently dropped on the docker path "+
			"and `up --wait` can block far past it")
}

// TestWave20_COMP2_DockerWaitTimeoutOmittedWhenUnset is the paired negative:
// with no WithWaitTimeout the docker path emits --wait but NOT --wait-timeout,
// so docker's `--wait` keeps its own built-in default and we never emit a
// degenerate `--wait-timeout 0`.
func TestWave20_COMP2_DockerWaitTimeoutOmittedWhenUnset(t *testing.T) {
	args := captureUpArgs(t,
		"docker", []string{"compose"},
		ComposeProject{Name: "p", File: "compose.yml"},
		WithWait(true),
	)

	assert.Contains(t, args, "--wait",
		"docker Up(WithWait) must emit --wait")
	assert.NotContains(t, args, "--wait-timeout",
		"COMP2-1: with no WithWaitTimeout the docker path must NOT emit "+
			"--wait-timeout (no degenerate `--wait-timeout 0`)")
}

// TestWave20_COMP2_PodmanWaitTimeoutNotEmittedAsFlag proves the podman path is
// unaffected: WithWaitTimeout bounds its host-side poll (waitForServices), it
// is NOT forwarded as a compose flag podman-compose does not understand. The
// fake script answers the follow-up `ps` poll ready so Up returns promptly.
func TestWave20_COMP2_PodmanWaitTimeoutNotEmittedAsFlag(t *testing.T) {
	args := captureUpArgs(t,
		"podman-compose", nil,
		ComposeProject{Name: "p", File: "compose.yml"},
		WithWait(true), WithWaitTimeout(30),
	)

	assert.NotContains(t, args, "--wait",
		"podman-compose must not receive the docker-native --wait flag")
	assert.NotContains(t, args, "--wait-timeout",
		"COMP2-1: the podman path bounds the wait via its host-side poll, "+
			"never via a --wait-timeout compose flag podman-compose lacks")
}
