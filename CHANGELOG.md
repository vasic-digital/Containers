# Changelog

## [Unreleased]

### Added
- Apple `container` crossbuild backend (`pkg/crossbuild/apple_container.go`
  + `apple_container_run.go`): boots a real Linux micro-VM via Apple's
  `container` CLI on macOS / Apple-Silicon hosts and bind-mounts host
  directories into it, providing the same Linux-target capability the
  podman/docker `LinuxContainerBackend` provides on Linux. Selected on
  darwin/arm64 ahead of the podman/docker fallback. Anti-bluff evidence
  (host darwin/arm64, container CLI v1.0.0): `go test -tags=integration
  -run AppleContainer` PASS — `container uname -s -m` => `Linux aarch64`
  while the host is Darwin (unforgeable real-Linux-kernel-on-macOS proof);
  host-dir mount round-trips both ways; `challenges/scripts/apple_container_linux_challenge.sh`
  PASS, `--mutate` => exit 99 (stripping `--mount` breaks the round-trip).
  Project-decoupled (§11.4.28): no consumer-project references.
- GPU-aware scheduling: `HostResources.GPU`, `GPURequirement`,
  `StrategyGPUAffinity`, `GPUHealthCheck`, `ProbeGPU`. See
  `docs/gpu-scheduling.md`.
- `CONTAINERS_REMOTE_HOST_N_GPU_AUTOPROBE` env var.
