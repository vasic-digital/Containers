# Containers — Pre-Integration Materials

**Revision:** 1
**Last modified:** 2026-07-15T11:16:34Z
**Purpose:** Consolidated pre-integration materials (gate before any integration/deployment work).

> This document consolidates and cross-references the existing `docs/` set for
> the `digital.vasic.containers` Go module. Every statement below is grounded in
> the real repository (files cited by path). Where a fact could not be
> determined from the tree it is marked `UNKNOWN:`. Nothing here is invented.
> This module is a project-agnostic, reusable Go library (§11.4.28) and is
> described generically — it carries no consumer-project literals.

---

## 1. Purpose / What it is

`digital.vasic.containers` is a **generic, reusable Go module for container
orchestration, health checking, lifecycle management, and service discovery**,
supporting Docker, Podman, and Kubernetes runtimes (`README.md:13`, module path
`digital.vasic.containers` in `go.mod:1`).

It is a **library, not a service**: a consuming project imports its packages and
uses them to boot, health-check, monitor, and (optionally) distribute *other*
containerized infrastructure on demand. The canonical entry point is
`pkg/boot.BootManager`, which composes the runtime, compose-orchestration, and
health-check packages (`README.md:99-128` "Architecture"; `pkg/boot/manager.go`).

Consumption model per the module's own `CLAUDE.md`: the shared container
orchestration layer (§11.4.76) used with a rootless-first runtime preference
(§11.4.161 — see §5/§6 below). Its declared downstream siblings are `Challenges`,
`HelixLLM`, `HelixQA`; it has **no upstream own-org dependency**
(`helix-deps.yaml`: `deps: []`; `CLAUDE.md` "Integration Seams").

Deeper references (existing docs, reuse — do not duplicate):
- Architecture: [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) and top-level [`ARCHITECTURE.md`](../ARCHITECTURE.md)
- API surface: [`docs/API_REFERENCE.md`](API_REFERENCE.md)
- Usage: [`docs/USER_GUIDE.md`](USER_GUIDE.md)
- Remote distribution: [`docs/REMOTE_DISTRIBUTION.md`](REMOTE_DISTRIBUTION.md), [`docs/REMOTE_DEPLOYMENT.md`](REMOTE_DEPLOYMENT.md)
- Anti-bluff test posture: [`docs/ANTI_BLUFF.md`](ANTI_BLUFF.md), [`docs/test-coverage.md`](test-coverage.md)

---

## 2. Architecture overview

**Stack:** Go module `digital.vasic.containers`, `go 1.25.0` (`go.mod:1,3`).
Layout is a flat `pkg/*` library plus thin `cmd/*` CLIs. No project-specific
subtree (`helix-deps.yaml: language_specific_subtree: false`).

Real package inventory (from `ls pkg/`, cross-checked against `CLAUDE.md`
"Package Structure"):

| Package (`pkg/…`) | Purpose (grounded) |
|---|---|
| `runtime` | Container-runtime abstraction + auto-detection (`runtime.go`, `detect.go`, `docker.go`, `podman.go`, `kubernetes.go`, `nerdctl.go`, `crio.go`, `lxd.go`) |
| `compose` | Compose orchestration (`orchestrator.go` `ComposeOrchestrator` interface: `Up`/`Down`/`Status`/`Logs`; grouping in `group.go`) |
| `health` | Health checking — TCP/HTTP/gRPC/Custom (`checker.go`, `types.go`, `tcp.go`, `http.go`, `grpc.go`, `custom.go`, `retry.go`, `gpu.go`, `helix_infra.go`) |
| `boot` | High-level `BootManager` composing runtime + compose + health (`manager.go`, `options.go`, `result.go`) |
| `endpoint` | Service-endpoint builder config (`README.md:49-62` `NewEndpoint().With…().Build()`) |
| `lifecycle` | Lazy boot, idle shutdown, concurrency semaphores (`README.md:118-121`) |
| `monitor` | System/container CPU/mem/disk + cluster snapshots |
| `event` | Publish/subscribe event bus (20 lifecycle event types — `README.md:88,107`) |
| `discovery` | Service discovery (TCP probe / DNS) |
| `logging` | Pluggable logger (slog adapter) |
| `metrics` | Prometheus-compatible metrics collector (`collector.go`, `prometheus.go`, `noop.go`) |
| `remote` | Remote host registry, SSH executor, connection pooling |
| `scheduler` | Resource-aware placement — 5 strategies (`resource_aware`, `round_robin`, `affinity`, `spread`, `bin_pack`) |
| `network` | SSH tunnel management + port allocation |
| `volume` | Remote volumes (SSHFS/NFS/rsync) |
| `envconfig` | `.env` / `CONTAINERS_REMOTE_*` env-var configuration |
| `distribution` | Distribution facade: schedule → deploy → failover |
| `orchestrator` | Multi-service boot ordering / auto-discovery |
| `serviceregistry` | Service registry + free-port finding (`registry.go`) |
| `cache`, `policy`, `i18n`, `lazyservice`, `brokertest` | Supporting packages (present in `pkg/`) |
| `applesim`, `genymotion`, `cuttlefish`, `emulator`, `vm`, `crossbuild`, `ctop` | Device-emulation / VM / cross-build / monitoring extensions (present in `pkg/`) |

**Composition (BootManager facade)** — from `README.md:99-128` +
`CLAUDE.md` "Composition":
```
boot.BootManager
├── compose.ComposeOrchestrator   (Up/Down/Status/Logs; local or remote)
├── health.HealthChecker          (TCP/HTTP/gRPC/Custom, with retry)
├── discovery.Discoverer
├── distribution.Distributor      (optional; SSH remote hosts)
├── event.EventBus                (20 event types)
├── metrics.MetricsCollector      (Prometheus)
└── logging.Logger                (pluggable)
```
`BootManager` is constructed via functional options (`pkg/boot/options.go`:
`WithRuntime`, `WithLogger`, `WithMetrics`, `WithEventBus`, `WithOrchestrator`,
`WithHealthChecker`, `WithProjectDir`, `WithDiscoverer`, `WithDistributor`,
`WithHostManager`, `WithScheduler`).

Design patterns (Strategy / Observer / Factory / Builder / Decorator /
Functional-Options / Proxy / Facade) are enumerated in `CLAUDE.md` "Design
Patterns". Diagram sources live in [`docs/diagrams/`](diagrams/)
(`architecture.mmd`, `boot_sequence.mmd`, `event_flow.mmd`, `lifecycle.mmd`).

**Rootless preference:** runtime auto-detection prefers Podman *because of its
rootless capabilities* — priority order **Podman → Docker → nerdctl → CRI-O →
LXD → Kubernetes** (`pkg/runtime/detect.go:13,25,68`). This aligns with the
rootless-container mandate (§11.4.161) referenced in this module's `CLAUDE.md`.

---

## 3. Dependencies

**Own-org submodule dependencies:** **NONE.** `helix-deps.yaml` declares
`deps: []` and states "Containers is a leaf Go submodule with ZERO own-org
submodule dependencies." There is no `.gitmodules` in the tree
(`ls`: no `.gitmodules`).

**External Go dependencies** (`go.mod`, direct `require`):
- `github.com/prometheus/client_golang v1.23.2`
- `github.com/prometheus/client_model v0.6.2`
- `github.com/stretchr/testify v1.11.1` (tests)
- `golang.org/x/crypto v0.52.0`
- `golang.org/x/term v0.43.0`
- `gopkg.in/yaml.v3 v3.0.1`

(Indirect deps: `beorn7/perks`, `cespare/xxhash/v2`, `prometheus/common`,
`prometheus/procfs`, `google.golang.org/protobuf`, etc. — see `go.mod` require
block.)

**Runtime (host) requirements** (from `CLAUDE.md` "Integration Seams — Hard
external dependencies" + `pkg/runtime/detect.go`):
- A **container runtime** on the local machine — one of Podman / Docker /
  nerdctl / CRI-O / LXD / Kubernetes (Podman preferred, rootless).
- For remote distribution: SSH client binaries locally, and an SSH server +
  container runtime on each configured remote host, with network reachability.
- `podman-compose` / `docker compose` for compose operations. UNKNOWN: the
  compose CLI binary name is selected by the runtime/`compose` package at
  invocation time; the module does not vendor a compose binary.

---

## 4. Deploy / Distribution design

**Distribution slice — this is a Go LIBRARY, consumed as a dependency; it is not
itself deployed as a standalone service.**

- **Primary distribution:** imported as a Go module (`go get
  digital.vasic.containers`, `README.md:17-19`). The consuming project imports
  `pkg/boot`, `pkg/runtime`, `pkg/health`, etc. and drives them from its own
  process. The single integration seam on the consumer side is an adapter
  (`CLAUDE.md`: `internal/adapters/containers/adapter.go`), described
  generically — the module carries no consumer literal.
- **On-demand infra boot:** the library's job is to boot *other* containerized
  infrastructure on demand. Consumer defines `endpoint.ServiceEndpoint`s (host,
  port, health type, compose file, service name), then calls
  `boot.BootManager.BootAll(ctx)` which brings up each compose project and
  health-checks it (`README.md:38-77`; `pkg/boot/manager.go:BootAll`).
- **Remote distribution (optional):** when `CONTAINERS_REMOTE_ENABLED=true`, a
  `distribution.Distributor` schedules `[]ContainerRequirements` across
  registered remote hosts via SSH (`docker run -d` per placement), creates SSH
  tunnels and mounts remote volumes, and returns a `DistributionSummary`
  (`README.md:130-172`; `CLAUDE.md` "Remote Distribution"; full guide
  [`docs/REMOTE_DISTRIBUTION.md`](REMOTE_DISTRIBUTION.md) /
  [`docs/REMOTE_DEPLOYMENT.md`](REMOTE_DEPLOYMENT.md)).
- **CLIs (`cmd/*`):** thin entry points also ship
  (`cmd/boot`, `cmd/deploy-stack`, `cmd/distributed-build`, `cmd/distributed-test`,
  `cmd/ctop`, `cmd/emulator-*`, `cmd/vm-matrix`, `cmd/applesim`, `cmd/genymotion`,
  `cmd/ota-device-emu-boot`). These are library-driven binaries, not a hosted
  service.
- **Env-driven scaling:** remote hosts are registered by appending
  `CONTAINERS_REMOTE_HOST_N_*` env vars (N = 1..100) — no code change
  (`CLAUDE.md` "Remote Distribution"; `.env.example` keys:
  `CONTAINERS_REMOTE_ENABLED`, `CONTAINERS_REMOTE_SCHEDULER`,
  `CONTAINERS_REMOTE_PORT_RANGE_START/END`, `CONTAINERS_REMOTE_VOLUME_TYPE`,
  `CONTAINERS_REMOTE_SSH_CONTROL_MASTER/PERSIST`, `CONTAINERS_REMOTE_COMMAND_TIMEOUT`, …).

Cross-references: [`docs/REMOTE_DISTRIBUTION.md`](REMOTE_DISTRIBUTION.md),
[`docs/RESOURCE_LIMITS.md`](RESOURCE_LIMITS.md),
[`docs/HOST_POWER_MANAGEMENT.md`](HOST_POWER_MANAGEMENT.md),
[`docs/gpu-scheduling.md`](gpu-scheduling.md).

---

## 5. Ports

**`UNKNOWN: this is a library that orchestrates other infra; it exposes no
persistent listen port of its own.`**

Grounded detail:
- **No persistent server / `ListenAndServe`.** A tree-wide scan for
  `net.Listen` / `http.ListenAndServe` / `http.Serve` over `pkg/` + `cmd/`
  (excluding tests) found only **transient free-port probes** — bind then
  immediately `Close()`:
  - `pkg/network/port_allocator.go:111` `isPortAvailable` (bind-then-close).
  - `pkg/serviceregistry/registry.go:269` `isPortAvailable` /
    `FindAvailablePort` (bind-then-close).
  - `pkg/emulator/containerized.go:603` `pickFreeTCPPort` (bind ephemeral
    `127.0.0.1:0`, close, hand the port to an emulator runtime CLI).
  None of these keep a socket open to serve requests.
- **Metrics are collected, not served.** `pkg/metrics/prometheus.go` contains
  **no** `ListenAndServe` / `promhttp` handler — the module registers a
  Prometheus collector; serving `/metrics` on an HTTP port is the **consumer's**
  responsibility. (grep of `pkg/metrics/prometheus.go` for a server: none.)
- **Ports it *cares about* belong to the orchestrated infra, not to itself:**
  the consumer declares each service's port via
  `endpoint.NewEndpoint().WithPort("5432")…` / `health.HealthTarget.Port`
  (`README.md:49-62`; `pkg/health/types.go:28`). Those are *outbound* targets
  the health checker dials (TCP/HTTP/gRPC), not listeners the library opens.
- **Remote SSH:** default SSH port `22` is a *consumer-configured* remote-host
  attribute (`CONTAINERS_REMOTE_HOST_N_PORT`, `CLAUDE.md` "Remote Distribution"),
  used as an outbound SSH client target — again not a listener of this module.

Net: **the library owns no service port**; it probes free ports transiently and
dials/health-checks the ports of the infrastructure it orchestrates.

---

## 6. Health

The health-check API **is a first-class package** — `pkg/health`.

- **Interface** (`pkg/health/checker.go:12-19`):
  ```go
  type HealthChecker interface {
      Check(ctx, target HealthTarget) *HealthResult
      CheckAll(ctx, targets []HealthTarget) []*HealthResult
  }
  ```
- **Mechanisms** (`pkg/health/types.go:11-20`): `HealthTCP` (`"tcp"`),
  `HealthHTTP` (`"http"`), `HealthGRPC` (`"grpc"`), `HealthCustom` (`"custom"`),
  with per-target `Timeout` and `Required` flags.
- **Default dispatcher** (`pkg/health/checker.go:30-38`): `NewDefaultChecker()`
  pre-registers TCP/HTTP/gRPC check funcs; `Register(type, func)` adds/replaces a
  mechanism (e.g. `HealthCustom`). `CheckAll` runs targets **concurrently** and
  preserves input order (`checker.go:78-95`).
- **Extra checkers present:** `pkg/health/http.go`, `tcp.go`, `grpc.go`,
  `custom.go`, `retry.go` (RetryPolicy decorator), `gpu.go` (GPU health),
  `helix_infra.go` (infra-specific health helper).
- **BootManager integration:** `boot.BootManager.HealthCheckAll(...)`
  (`pkg/boot/manager.go:291`) probes registered endpoints; required services
  failing = boot failure (`CLAUDE.md` "Mandatory Container Orchestration Flow"
  step 4/6).

Health results (`pkg/health/types.go:56-70`) carry `Healthy`, `Duration`,
`Error`, `Timestamp`, and a `Details` map — the captured-evidence surface a
consumer's anti-bluff gate can assert on.

Cross-reference: [`docs/ANTI_BLUFF.md`](ANTI_BLUFF.md),
[`docs/test-coverage.md`](test-coverage.md).

---

## 7. How it boots (the boot API)

A consumer boots infrastructure on demand as follows (grounded in
`README.md:38-77`, `pkg/boot/manager.go`, `pkg/boot/options.go`,
`CLAUDE.md` "Mandatory Container Orchestration Flow"):

1. **Detect runtime:** `rt, _ := runtime.AutoDetect(ctx)` — picks the first
   available runtime, **Podman first (rootless), then Docker → nerdctl → CRI-O →
   LXD → Kubernetes** (`pkg/runtime/detect.go:25-49`). Fails with
   "tried podman, docker, nerdctl, cri-o, lxd, kubernetes" if none present.
2. **Declare endpoints:** build a `map[string]endpoint.ServiceEndpoint` via the
   builder — `NewEndpoint().WithHost().WithPort().WithHealthType().WithRequired().
   WithComposeFile().WithServiceName().Build()` (`README.md:49-62`).
3. **Construct the manager:** `mgr := boot.NewBootManager(endpoints,
   boot.WithRuntime(rt), boot.WithLogger(...), …)` (functional options in
   `pkg/boot/options.go`; optional `WithMetrics`, `WithEventBus`,
   `WithDistributor`, `WithHostManager`, `WithScheduler`, `WithProjectDir`).
4. **Boot everything:** `summary, err := mgr.BootAll(ctx)`
   (`pkg/boot/manager.go:64`) — for each endpoint with a compose file it calls
   the `ComposeOrchestrator.Up` (`pkg/compose/orchestrator.go:20`) (local, or
   remote-via-SSH when a distributor is wired), then health-checks each service.
   Returns a boot summary (`Started` / `Failed`, `README.md:75-77`).
5. **(Optional) distribute containers:** with remote enabled, hand
   `[]scheduler.ContainerRequirements` to `distribution.Distributor.Distribute`
   → `Scheduler.ScheduleBatch` places each container on the best host → SSH
   `docker run -d` + tunnels + volumes (`README.md:130-172`; `CLAUDE.md` steps
   5-6).
6. **Continuous health/monitoring:** periodic `HealthChecker.CheckAll` +
   `HostManager.ProbeAll` (`CLAUDE.md` step 6).
7. **Shutdown:** `mgr.Shutdown(ctx)` (`pkg/boot/manager.go:330`) →
   `Distributor.Undistribute` + `ComposeDown` per compose file (`CLAUDE.md`
   step 7).

Acceptance demo (from this module's `CLAUDE.md`): a real orchestration flow that
boots a container via `pkg/boot.BootManager` and verifies its health check
against a live rootless Docker/Podman runtime —
`go test -tags=integration -count=1 -race -v ./tests/integration/...`.

Boot-sequence diagram source: [`docs/diagrams/boot_sequence.mmd`](diagrams/boot_sequence.mmd).

---

## 8. Materials status (verify pass)

Verdict per material. **HAS-VERIFIED** = the fact was read directly from a real
repo file (cited). No numbers were invented; unverifiable items are `UNKNOWN:`.

| # | Material | Verdict | Evidence (real repo) |
|---|---|---|---|
| 1 | Purpose / what it is | **HAS-VERIFIED** | `README.md:13`, `go.mod:1`, `helix-deps.yaml`, `CLAUDE.md` Overview |
| 2 | Stack (Go 1.25.0) | **HAS-VERIFIED** | `go.mod:1,3` (note: `CLAUDE.md` says "1.24+"; `go.mod` is authoritative at `go 1.25.0`) |
| 3 | Package inventory (`pkg/*`) | **HAS-VERIFIED** | `ls pkg/` + `CLAUDE.md` "Package Structure" |
| 4 | Boot API (`BootManager`, options, `BootAll`/`Shutdown`) | **HAS-VERIFIED** | `pkg/boot/manager.go:41,64,291,330`, `pkg/boot/options.go` |
| 5 | Compose interface (`Up`/`Down`/`Status`/`Logs`) | **HAS-VERIFIED** | `pkg/compose/orchestrator.go:20-38` |
| 6 | Health API (TCP/HTTP/gRPC/Custom) | **HAS-VERIFIED** | `pkg/health/checker.go:12-45`, `pkg/health/types.go:11-70` |
| 7 | Runtime auto-detect + rootless Podman preference | **HAS-VERIFIED** | `pkg/runtime/detect.go:13,25,49,68`, `pkg/runtime/podman.go:12-24` |
| 8 | Dependencies (own-org NONE; external libs) | **HAS-VERIFIED** | `helix-deps.yaml` (`deps: []`), `go.mod` require block, no `.gitmodules` |
| 9 | Distribution/deploy design (library + on-demand infra + remote SSH) | **HAS-VERIFIED** | `README.md:17-19,130-213`, `CLAUDE.md` flow, `.env.example` keys |
| 10 | Ports (no own listen port) | **HAS-VERIFIED (as UNKNOWN-honest)** | `net.Listen` scan → only bind-then-close probes (`pkg/network/port_allocator.go:111`, `pkg/serviceregistry/registry.go:269`, `pkg/emulator/containerized.go:603`); no server in `pkg/metrics/prometheus.go` |
| 11 | Health (first-class `pkg/health`) | **HAS-VERIFIED** | `pkg/health/*` |
| 12 | Boot flow (7-step) | **HAS-VERIFIED** | `README.md:38-77`, `CLAUDE.md` "Mandatory Container Orchestration Flow", `pkg/boot/manager.go` |

**Open `UNKNOWN:` items** (could not be determined from the tree, stated
honestly — not gap-filled with guesses):
- `UNKNOWN:` the exact compose CLI binary name invoked at runtime
  (`podman-compose` vs `docker compose`) — selected by the runtime/`compose`
  package per detected runtime; not a vendored binary.
- `UNKNOWN:` this library exposes no persistent listen port of its own; port
  ownership belongs to the orchestrated infrastructure the consumer declares
  (§5).

**Overall verdict:** the pre-integration material set for this module is
**HAS-VERIFIED** — every load-bearing claim was read from a real file in the
repository and cited. The only non-`HAS-VERIFIED` entries are the two honest
`UNKNOWN:` boundaries above, which are properties of the library's nature (it is
a dependency that orchestrates other infra), not documentation gaps.

---

### Consolidated existing-docs index (for reuse, not duplication)

| Doc | Path |
|---|---|
| Architecture | [`docs/ARCHITECTURE.md`](ARCHITECTURE.md), [`ARCHITECTURE.md`](../ARCHITECTURE.md) |
| API reference | [`docs/API_REFERENCE.md`](API_REFERENCE.md) |
| User guide | [`docs/USER_GUIDE.md`](USER_GUIDE.md) |
| Remote distribution | [`docs/REMOTE_DISTRIBUTION.md`](REMOTE_DISTRIBUTION.md) |
| Remote deployment | [`docs/REMOTE_DEPLOYMENT.md`](REMOTE_DEPLOYMENT.md) |
| Resource limits | [`docs/RESOURCE_LIMITS.md`](RESOURCE_LIMITS.md) |
| GPU scheduling | [`docs/gpu-scheduling.md`](gpu-scheduling.md) |
| Host power management | [`docs/HOST_POWER_MANAGEMENT.md`](HOST_POWER_MANAGEMENT.md) |
| OTA device emulation | [`docs/ota-device-emulation.md`](ota-device-emulation.md) |
| Cross-build | [`docs/crossbuild/`](crossbuild/) |
| Anti-bluff posture | [`docs/ANTI_BLUFF.md`](ANTI_BLUFF.md) |
| Test coverage | [`docs/test-coverage.md`](test-coverage.md) |
| SQL definitions | [`docs/sql-definitions.md`](sql-definitions.md) |
| Behavior anchors | [`docs/behavior-anchors.md`](behavior-anchors.md) |
| Contributing | [`docs/CONTRIBUTING.md`](CONTRIBUTING.md) |
| Diagrams (mermaid) | [`docs/diagrams/`](diagrams/) |
