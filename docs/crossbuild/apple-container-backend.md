# Apple `container` crossbuild backend

**Revision:** 1
**Last modified:** 2026-06-13T00:00:00Z
**Package:** `pkg/crossbuild` (`apple_container.go`, `apple_container_run.go`)
**Scope:** generic / project-agnostic. No consuming-project identifiers appear in the backend.

## What it is

`AppleContainerBackend` lets a macOS / Apple-Silicon host run an
arbitrary command inside a **Linux** container — and cross-build Linux
artifacts — by shelling out to Apple's `container` CLI
([github.com/apple/container](https://github.com/apple/container)).
Apple `container` boots **one lightweight virtual machine per
container** via the Virtualization framework with a minimal Linux
guest userspace, and consumes/produces standard OCI images, so
vanilla `alpine` / `ubuntu` / `debian` arm64 images run natively
(no Rosetta / x86 emulation).

It is the Apple-Silicon-native sibling of `LinuxContainerBackend`
(podman/docker). Both implement the same `crossbuild.Backend`
interface and route through the same `Selector`.

## Two entry points

| API | Purpose |
|---|---|
| `AppleContainerBackend` (`Backend`) | Cross-build a Linux artifact: mount source dir, run a build command, assert a non-zero artifact, copy it back to the host. Selected by the `Selector`. |
| `RunInLinuxContainer(ctx, LinuxRunRequest)` | Generic primitive: run an arbitrary shell command in a Linux container with an optional host-dir bind mount, capturing **stdout + stderr + exit code** verbatim. Does NOT assert an artifact — suitable for "run the test suite and tell me what it printed". |

## Capabilities (honest)

- `SupportsTargets`: `[{OS: linux, Arch: arm64}]` only. amd64 is
  intentionally **not** advertised — x86 Linux on Apple `container`
  depends on Rosetta, which has documented kernel-version regressions.
- `RequiresHostOS`: `["darwin"]` — Apple `container` is macOS-only.
  This is the restriction the `Selector` routes on; on a non-darwin
  host the backend is never chosen and the podman/docker
  `LinuxContainerBackend` serves the same target.
- `IsolatesEnvironment`: `true`.

## CLI it shells out to

```
container run --rm [--name <n>] \
  --mount type=virtiofs,source=<HOSTDIR>,target=/work/src \
  -w /work/src [-e K=V ...] <imageRef> sh -c '<command>'
```

If the installed `container` version rejects the explicit `--mount`
form, the runner falls back to the short `-v <HOSTDIR>:/work/src`
form (probed once, no guessing per §11.4.6). stdout/stderr are
captured into buffers and the exit code is read from
`ProcessState.ExitCode()` — the same mechanism the podman/docker
runner uses.

## Selecting it

Register the Apple-container backend **before** the podman/docker
`LinuxContainerBackend` so it wins on a darwin host for linux/arm64;
on a non-darwin host it is filtered out and the podman/docker backend
serves:

```go
sel := crossbuild.NewSelector()
sel.Register(crossbuild.NewAppleContainerBackend("")) // darwin/arm64 → preferred
sel.Register(crossbuild.NewLinuxContainerBackend("", "arm64")) // fallback
sel.Register(crossbuild.NewHostDirectBackend())
res := sel.Build(ctx, req) // req.Target = {linux, arm64}
```

Existing podman/docker selection is unchanged: the new backend only
matches `host=darwin` + `target=linux/arm64`.

## One-time host bootstrap (outside test execution)

```sh
brew install --cask container     # or the signed .pkg from GitHub Releases
container system start            # installs the default Linux kernel on first run
```

After that, `container run` / `container exec` are unprivileged (no
sudo). This is the one-time bootstrap — the backend and its tests are
fully self-driving thereafter.

## Anti-bluff evidence

- **Unit tests** (`apple_container_test.go`) inject a fake runner to
  assert the exact `container` argv (mount flag, env ordering, image,
  `sh -c`), host-OS gating, engine-absent + missing-image honest
  errors, zero-byte-artifact-is-bluff, exit-code propagation, stdout
  capture, and `Selector` routing. Fakes are permitted only here.
- **Integration test** (`tests/integration/apple_container_integration_test.go`,
  `-tags=integration`) runs the **real** engine. The unforgeable
  proof: the host `uname -s -m` is `Darwin arm64`, but the container's
  `uname -s -m` is `Linux aarch64` — asserting the latter proves a
  real Linux micro-VM booted. It also writes a sentinel on the host
  and reads it back through the mount (both directions). When the
  engine/kernel is not ready it SKIPs-with-reason (honest kernel-gap
  per §11.4.81), never a faked PASS.
- **Challenge** (`challenges/scripts/apple_container_linux_challenge.sh`)
  is the real-stack equivalent with a `--mutate` paired mutation:
  stripping the `--mount` flag breaks the round-trip (exit 99 =
  mutation-witnessed); if it did not, the gate would be flagged a
  bluff.

### Captured real run (this host, macOS 15.5 / arm64, `container` 1.0.0)

```
container uname -s -m => Linux aarch64 (host is Darwin arm64)
[PASS] real Linux aarch64 kernel ran inside Apple-container micro-VM (host is Darwin)
[PASS] host-dir mount round-trips both ways (host->guest read + guest->host write)
```

## Honest gaps

- Apple `container` is officially supported on macOS 26; it runs on
  macOS 15 within the limits this flow needs (host-dir mount +
  host→guest exec — not inter-container networking). Unreproducible-
  on-26 bugs may go unfixed upstream.
- The default Linux kernel (~570 MB) downloads on first `container
  system start`. Until it is present, a `container run` cannot boot —
  the tests and challenge detect this and SKIP honestly.

## Sources verified 2026-06-13

- <https://github.com/apple/container> — README: requirements, `.pkg` install, `container system start`, OCI images, lightweight per-container VMs.
- <https://github.com/apple/container/blob/main/docs/command-reference.md> — `run`/`exec`/`image`/`system` flags incl. `--mount`, `-v`, `-w`, `-e`, `--rm`, `--name`.
- <https://github.com/apple/container/blob/main/docs/technical-overview.md> — one-VM-per-container, Virtualization framework, OCI compatibility, macOS 15 networking limits.
