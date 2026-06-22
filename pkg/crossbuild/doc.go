// Package crossbuild orchestrates cross-platform binary builds inside
// container-bound or QEMU-bound build environments managed by this
// submodule. It is the generic, decoupled cross-build authority for
// every consumer of `digital.vasic.containers`.
//
// # Mandate
//
// Per the user's iter-54 mandate (2026-05-13):
//
//	"Solutions for use of Containers and Qemu MUST be all handled by
//	 Containers Submodule on generic reusable decoupled level!"
//
// Consumer projects MUST NOT bake their own
// container/QEMU wiring; they instead call into this package with a
// minimal `BuildRequest` and receive back a `BuildResult` describing
// where the produced artifact lives. The host-platform decision
// (container vs QEMU VM, Wine vs native Windows guest) is internal to
// this package and may evolve without breaking consumers.
//
// # Supported targets (iter-54 status)
//
// | Target               | Backend                        | Status              |
// |----------------------|--------------------------------|---------------------|
// | linux/amd64          | host-direct (no container)     | OPERATIONAL         |
// | linux/arm64          | host-direct (no container)     | OPERATIONAL         |
// | darwin/arm64         | host-direct (no container)     | OPERATIONAL         |
// | darwin/amd64         | host-direct (no container)     | OPERATIONAL         |
// | windows/amd64        | Wine-in-Linux-container        | SKELETON (iter-54)  |
// | windows/amd64 (QEMU) | QEMU Windows guest             | SKELETON (iter-54)  |
//
// SKELETON means the orchestration code + tests are in place; the
// container image itself (Wine-in-Linux) or the Windows QCOW2 disk
// (QEMU path) is operator-provisioned per documented procedure (see
// docs/crossbuild/windows-image-provisioning.md). The skeleton allows
// CI agents to verify the orchestration is wired correctly without
// owning the multi-GB Windows image artifact.
//
// # Anti-bluff posture
//
// Per CONST-035 / Article XI §11.9:
//
//   - Every public function in this package has a positive-evidence
//     test that exercises the orchestration end-to-end against an
//     INJECTED `Backend` interface, not by mocking the function under
//     test.
//   - The OPERATIONAL targets above also have a Challenge script that
//     actually produces a real-stack artifact and asserts its
//     existence + non-zero size + correct file magic on the host.
//   - SKELETON targets carry an explicit
//     "SKIP-OK: #crossbuild-windows-image-provisioning" marker on the
//     real-stack Challenge until the operator provisions the image.
//     The orchestration tests still PASS via fake backends — separate
//     concern.
//
// A green CI run means: "the orchestration code routes a BuildRequest
// to the right Backend and validates the BuildResult honestly". It
// does NOT mean "the Windows image exists on this host". Those are
// orthogonal claims.
package crossbuild
