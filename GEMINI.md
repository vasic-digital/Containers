# GEMINI.md — containers

## INHERITED FROM constitution/GEMINI.md

**The inheritance below is conditional. Both cases are stated; neither is
assumed.**

When this module is consumed inside a project that includes the Helix
Constitution submodule, the rules in `constitution/GEMINI.md` — and in the
`constitution/Constitution.md` it references — are authoritative for every
topic not covered here. The module-local rules below extend them; they never
weaken or override them.

When this module is consumed standalone — cloned on its own, with no
constitution reachable in any parent — there is nothing to inherit, and **only
the module-local rules below apply**.

### Locating the base file: a resolver, never a path

`constitution/GEMINI.md` in the heading above is the **canonical name of the base
file**, written exactly as the constitution's own examples write it. It is not
a filesystem path relative to this module, and it must not be rewritten into
one:

- a consuming project may mount the constitution under more than one layout,
  and this module cannot know which one it got;
- the same commit of this module can be checked out at two different depths at
  the same time, so no single relative path is correct for both;
- a standalone clone has no constitution anywhere, so any hardcoded path would
  simply dangle.

Resolve it at run time with the constitution's own parent-walk resolver,
**`find_constitution.sh`**. It walks up the parent chain trying each layout the
constitution supports, follows
`git rev-parse --show-superproject-working-tree` out of nested submodules so it
works from any nested depth, and exits non-zero with an explicit error when no
constitution is reachable — which is precisely the standalone case above.

This file therefore hardcodes **no** parent-project path and **no**
depth-dependent path, keeping the module project-not-aware, decoupled and
reusable per §11.4.28(B). Agent tooling with a native file-import syntax must
not turn the heading into one: an `@constitution/GEMINI.md` import resolves
relative to *this* file, so inside a module it points at a path that does not
exist and silently resolves to nothing.

Canonical reference:
<https://github.com/HelixDevelopment/HelixConstitution>

## Module-local notes

This carrier is read by Gemini CLI.

See [`README.md`](README.md) for what this module is and how it is used.
Module-specific rules go below this line; they extend the inherited base rules
and never weaken them.

### What this module is

`digital.vasic.containers` is a generic, reusable Go module for container
orchestration, health checking, lifecycle management, and service discovery. It
provides a unified interface over Docker, Podman and Kubernetes runtimes, with
lazy booting, idle shutdown, semaphore-based parallelism control, resource
monitoring, and — on top of that local core — SSH-based distribution of
containers across remote hosts.

**Module path**: `digital.vasic.containers`
**Go version**: 1.24+ (`go.mod` is authoritative)
**Dependencies**: container runtime clients (Docker/Podman/K8s), Prometheus
client, otherwise minimal.

**Status stamp carried over verbatim from the pre-lockstep carrier body and NOT
re-verified by the carrier merge (§11.4.6):** version 2.1.0, "Production Ready",
last content update 22 February 2026.

### Acceptance demo for this module

```bash
# Real orchestration flow: boot a container via pkg/boot.BootManager and
# verify its health check passes against a live runtime (Docker/Podman).
# Run from this module's own root (wherever the consuming project checks out
# this submodule, e.g. `submodules/containers/` — never a hardcoded nested
# capitalized `Containers/` directory, which does not exist on a standalone
# clone of this repo).
GOMAXPROCS=2 nice -n 19 go test -tags=integration -count=1 -race -v ./tests/integration/...
```

Expect: PASS; exercises `runtime.AutoDetect`, `boot.BootManager.BootAll`, and
`health.HealthChecker` against a real container runtime per this module's
`README.md` Quick Start. Requires a rootless Docker/Podman runtime available to
the current user.

### Package responsibilities

| Package | Path | Responsibility |
|---------|------|----------------|
| `runtime` | `pkg/runtime/` | Container runtime abstraction layer: `ContainerRuntime` interface with implementations for Docker, Podman, Kubernetes. Auto-detection of available runtime. Runtime-agnostic container operations (start, stop, inspect, remove). |
| `compose` | `pkg/compose/` | Docker Compose orchestration: `ComposeOrchestrator` interface for compose file operations (up, down, restart). Support for profiles and service filtering. Multi-file composition and variable substitution. |
| `health` | `pkg/health/` | Health checking dispatcher: Multiple strategies (TCP, HTTP, gRPC, custom script). Retry policies with exponential backoff. Configurable timeouts and thresholds. Health status aggregation. |
| `endpoint` | `pkg/endpoint/` | Service endpoint configuration: `Endpoint` struct (host, port, path, scheme). Builder pattern for endpoint construction. Endpoint validation and URL generation. |
| `lifecycle` | `pkg/lifecycle/` | Advanced lifecycle management: Lazy boot (on-demand container startup). Idle shutdown (resource optimization). Semaphore-based parallelism control. Graceful shutdown sequences. |
| `monitor` | `pkg/monitor/` | Resource monitoring: System resource tracking (CPU, memory, disk). Per-container resource usage. Cluster snapshots. Threshold-based alerting. Prometheus metrics export. |
| `event` | `pkg/event/` | Lifecycle event bus: Publish/subscribe for container lifecycle events (20 event types). Event types: `ContainerStarted`, `ContainerStopped`, `HealthCheckFailed`, etc. Hook system for custom actions. |
| `discovery` | `pkg/discovery/` | Service discovery: TCP port scanning for service detection. DNS-based discovery. mDNS support for local network. Multi-strategy discovery with fallback. |
| `logging` | `pkg/logging/` | Logging abstraction: Bring-your-own-logger interface. Adapters for popular loggers (logrus, zap, zerolog). Structured logging support. |
| `metrics` | `pkg/metrics/` | Metrics collection: Prometheus-compatible metrics. Container lifecycle metrics. Health check metrics. Resource utilization metrics. |
| `boot` | `pkg/boot/` | High-level orchestration: `BootManager` composing all packages. One-line service initialization. Coordinated health checking and lifecycle management. Configuration validation. Distributor integration for remote endpoints. |
| `orchestrator` | `pkg/orchestrator/` | Service orchestration: `DefaultOrchestrator` for auto-discovering and managing containerized services. Supports local and remote deployment. Auto-discovery of docker-compose files. Thread-safe service management. |
| `remote` | `pkg/remote/` | Remote host management: `RemoteExecutor` (SSH command execution with ControlMaster pooling), `HostManager` (host registry, resource probing), `RemoteRuntime` (ContainerRuntime over SSH), `RemoteComposeOrchestrator`. |
| `scheduler` | `pkg/scheduler/` | Resource-aware container scheduling: 5 strategies (resource_aware, round_robin, affinity, spread, bin_pack). `ResourceScorer` for weighted host scoring (CPU 40%, Memory 40%, Disk 10%, Network 10%). |
| `network` | `pkg/network/` | Cross-host networking: `TunnelManager` for SSH tunnels (local/remote forwarding), `PortAllocator` (thread-safe port range 20000-30000), `OverlayNetwork` for Docker overlay spanning hosts. |
| `volume` | `pkg/volume/` | Remote volume management: `VolumeManager` with 3 backends — SSHFS (real-time), NFS (shared export), rsync (periodic sync). Mount/unmount/sync operations. |
| `envconfig` | `pkg/envconfig/` | Environment configuration: `CONTAINERS_REMOTE_*` env var parsing, `.env` file loading, numbered host definitions (`HOST_N_NAME/ADDRESS/PORT/...`), template generation. |
| `distribution` | `pkg/distribution/` | Distribution orchestrator: `Distributor` composing scheduler + remote + network + volume. 7-phase workflow (probe → schedule → volumes → deploy → tunnels → health → events). Failover detection and rescheduling. |
| `ctop` | `pkg/ctop/` | Container monitoring: top/htop-style display for local and remote containers. `Collector` gathers container data. `Display` provides interactive TUI, snapshot, and JSON output. Sorting, filtering, color-coded resource usage. |
| `applesim` | `pkg/applesim/` | Apple iOS/iPadOS/tvOS/watchOS Simulator detection and lifecycle control (list, boot, install, launch, record, shutdown) by wrapping the host-native `xcrun simctl` toolchain. macOS-host-only (CoreSimulator cannot run inside a Linux container); sibling of `pkg/emulator` (Android-in-container) and `pkg/genymotion` (Android-in-VM). |
| `brokertest` | `pkg/brokertest/` | Provisions ephemeral message-broker containers (etcd, Postgres, ...) for integration testing, built entirely on the `pkg/runtime` abstraction for runtime selection and lifecycle; shells the runtime CLI only for the single "run a new container from an image" step the interface doesn't expose. |
| `cache` | `pkg/cache/` | Content-addressable store for image artifacts (qcow2 for VMs, Android system-images for emulators). SHA-256 verify-on-fetch rejects a downloaded image whose computed hash doesn't match its manifest entry. |
| `crossbuild` | `pkg/crossbuild/` | Cross-platform binary builds inside container-bound or QEMU-bound build environments (Wine + JDK/Gradle for Windows `.msi`, Linux containers for `.deb`/`.rpm`, Apple container backend). Callers submit a minimal `BuildRequest`; the host-platform decision is internal. |
| `cuttlefish` | `pkg/cuttlefish/` | Thin lifecycle wrapper (boot/health/teardown) for Google's Cuttlefish (`cvd`) virtual-Android device, run as a rootful + privileged container, exercising the real Android A/B `update_engine` + AVB/dm-verity + auto-rollback OTA apply path that the SDK emulator does not. |
| `egress` | `pkg/egress/` | VPN-host egress routing and diagnosis: establishes a dynamic SSH SOCKS5 tunnel (`ssh -D ... -N <vpnhost>`) to reach upstreams that are network-level blocked from the local/datacenter host, and verifies the routing actually changes the observed egress IP. |
| `emulator` | `pkg/emulator/` | Multi-target emulator orchestration for the container ecosystem; first supported target is Android (QEMU-backed via the SDK emulator), with non-Android/QEMU targets on the roadmap. |
| `genymotion` | `pkg/genymotion/` | Detection and lifecycle control of Genymotion Desktop Android virtual devices (VM-based via VirtualBox/QEMU/Hypervisor.framework) through the `gmtool` CLI, macOS + Linux portable. |
| `i18n` | `pkg/i18n/` | Defines the `Translator` contract for containers' user-facing error messages, status descriptions, and operator-visible text, so string literals can be externalized instead of hardcoded (the no-hardcoded-content mandate); imports nothing from any consuming project. |
| `lazyservice` | `pkg/lazyservice/` | `LazyOrchestrator`: dependency-ordered service startup, alternative-service fallback, stop/stop-all bookkeeping, status mapping, and filter queries (`ListServices`/`ListByCategory`/`ListFreeServices`). |
| `policy` | `pkg/policy/` | Resource-cap policy (`mem_limit`/`memswap_limit`/`pids_limit`/`oom_score_adj`) that keeps container stacks from exhausting host memory; the Go counterpart of `scripts/resource-policy/policy.yaml` (`VerifyAgainstYAML` keeps both in sync). |
| `remoteexec` | `pkg/remoteexec/` | Durable remote (or local) execution of long-running jobs (QA matrices, builds, deploys) that survive the launching SSH/login session ending, via `loginctl enable-linger` + `systemd-run --user --unit=X --collect`. |
| `serviceregistry` | `pkg/serviceregistry/` | Disk-persisted `ServiceRegistry`: register/discover services by name with host/port/protocol/health metadata, port allocation (`FindAvailablePort`), health tracking, and a process-wide `Global()` singleton. |
| `vm` | `pkg/vm/` | QEMU full-system virtual machine orchestration; sibling of `pkg/emulator` sharing `pkg/cache` for image artifacts. API mirrors `pkg/emulator` (Boot/WaitForReady/Upload/Run/Download/Teardown + `MatrixRunner`). |

### Dependency graph

```
boot  --->  runtime
boot  --->  compose  --->  runtime
boot  --->  health  --->  endpoint
boot  --->  lifecycle  --->  runtime, event
boot  --->  monitor  --->  runtime, remote
boot  --->  discovery  --->  endpoint
boot  --->  event
boot  --->  logging
boot  --->  metrics
boot  --->  remote
boot  --->  scheduler  --->  remote

orchestrator  --->  compose
orchestrator  --->  remote
orchestrator  --->  health
orchestrator  --->  logging

distribution  --->  scheduler  --->  remote
distribution  --->  remote
distribution  --->  network  --->  remote
distribution  --->  volume  --->  remote
distribution  --->  runtime
distribution  --->  logging

envconfig  --->  remote

remote  --->  runtime (RemoteRuntime implements ContainerRuntime)
remote  --->  compose (RemoteComposeOrchestrator implements ComposeOrchestrator)

ctop  --->  remote
ctop  --->  envconfig
```

`runtime`, `endpoint`, and `logging` are leaf packages. `boot`, `orchestrator`,
`distribution`, and `ctop` are integration layers. `remote` is the foundation
for all distributed features. The graph is strictly acyclic — never import
`boot` from a sub-package.

### Key interfaces

- `runtime.ContainerRuntime` — Container operations (local and remote via RemoteRuntime)
- `compose.ComposeOrchestrator` — Compose file operations (local and remote)
- `health.HealthChecker` — Health check dispatch
- `lifecycle.LifecycleManager` — Service lifecycle with lazy boot
- `monitor.ResourceMonitor` — System/container resource monitoring
- `event.EventBus` — Publish/subscribe for lifecycle events (20 event types)
- `discovery.Discoverer` — Service discovery
- `logging.Logger` — Logging abstraction
- `metrics.MetricsCollector` — Metrics collection
- `remote.RemoteExecutor` — SSH command execution on remote hosts
- `remote.HostManager` — Remote host registry and resource probing
- `scheduler.Scheduler` — Resource-aware container placement (5 strategies)
- `network.TunnelManager` — SSH tunnel creation/management
- `volume.VolumeManager` — Remote volume mounting (SSHFS/NFS/rsync)
- `distribution.Distributor` — Unified distribution orchestrator

### Design patterns

- **Strategy**: ContainerRuntime (Docker/Podman/K8s), HealthChecker (TCP/HTTP/gRPC), Scheduler (5 strategies)
- **Observer**: EventBus for lifecycle events (20 event types)
- **Factory**: `runtime.AutoDetect()`, `health.NewDefaultChecker()`
- **Builder**: `endpoint.NewEndpoint().WithHost().WithPort().Build()`
- **Decorator**: RetryPolicy wraps HealthChecker, RemoteRuntime wraps ContainerRuntime
- **Functional Options**: `boot.WithRuntime()`, `distribution.WithScheduler()`, etc.
- **Proxy**: RemoteRuntime routes ContainerRuntime calls via SSH
- **Facade**: Distributor composes scheduler + remote + network + volume

### Composition: how the pieces combine

The adapter layer that a consuming project writes
(`internal/adapters/containers/adapter.go` in the reference integration) wires
the module together as follows:

```
the consuming project BootManager → Adapter.BootAll(endpoints)
         │
         ├── ContainerRuntime  (auto-detected: Docker / Podman / containerd)
         ├── ComposeOrchestrator  (compose file parse + up/down, local or remote)
         └── HealthChecker  (TCP / HTTP / gRPC, with retry)
                 │
                 ▼ (if CONTAINERS_REMOTE_ENABLED=true)
         DefaultDistributor
             │
             ├── Scheduler  (chooses host per container: resource_aware default)
             ├── RemoteRuntime = proxy(ContainerRuntime) over SSHExecutor
             ├── TunnelManager  (SSH port forwarding for cross-host networking)
             └── VolumeManager  (SSHFS / NFS / rsync)
```

Distributor receives a batch of container requirements, asks Scheduler which
host each should land on (local or a named remote), then either calls the local
runtime directly or wraps it in RemoteRuntime for SSH execution.

### Runtime detection logic

```go
// AutoDetect tries: Docker -> Podman -> Kubernetes (in that order)
func AutoDetect() (ContainerRuntime, error) {
    // 1. Try Docker
    if dockerAvailable() {
        return NewDockerRuntime()
    }

    // 2. Try Podman
    if podmanAvailable() {
        return NewPodmanRuntime()
    }

    // 3. Try Kubernetes
    if kubernetesAvailable() {
        return NewKubernetesRuntime()
    }

    return nil, ErrNoRuntimeAvailable
}
```

### Health check strategies

| Strategy | Use Case | Configuration |
|----------|----------|---------------|
| TCP | Database, cache, message queue | Host, port, timeout |
| HTTP | REST APIs, web services | URL, expected status code, timeout |
| gRPC | gRPC services with health check protocol | Host, port, service name |
| Custom | Custom health logic | Script path or function |

### Lifecycle states

```
UNSTARTED -> STARTING -> STARTED -> STOPPING -> STOPPED
                  |                     |
                  +---> FAILED <--------+
```

### Configuration example

```go
package main

import (
    "digital.vasic.containers/pkg/boot"
    "digital.vasic.containers/pkg/runtime"
    "digital.vasic.containers/pkg/logging"
)

func main() {
    // Auto-detect runtime
    rt, _ := runtime.AutoDetect()

    // Create logger
    logger := logging.NewDefaultLogger()

    // Create boot manager with functional options
    manager := boot.NewBootManager(
        boot.WithRuntime(rt),
        boot.WithLogger(logger),
        boot.WithHealthCheckRetries(3),
        boot.WithParallelStartup(true),
        boot.WithLazyBoot(true),
    )

    // Add services
    manager.AddService("postgresql", boot.ServiceConfig{
        ComposeFile: "docker-compose.yml",
        ServiceName: "postgres",
        HealthCheck: boot.TCPCheck("localhost", 5432),
        Required:    true,
    })

    // Start all services
    manager.Start(ctx)
}
```

### Mandatory container orchestration flow (inline)

This is the flow a consuming project's root carrier refers to when it declares
container orchestration a hard stop. The flow is:

1. **Build:** `make build` → the consuming project's own binary under `./bin/`
   (in the reference integration this is `./bin/helixagent`; the name belongs to
   the consumer, not to this module — §11.4.28 keeps it out of any code path here).
2. **Env load:** the consuming project reads this module's own `.env` file
   (wherever the consuming project checks out this submodule, e.g.
   `submodules/containers/.env` — never a hardcoded nested `Containers/.env`
   path) via `envconfig.LoadFromFile()`:
   - `CONTAINERS_REMOTE_ENABLED` (bool)
   - `CONTAINERS_REMOTE_HOST_N_*` (N = 1..100; loader stops at the first absent `_NAME`)
   - SSH pool, timeouts, scheduler strategy
3. **Adapter init** (`internal/adapters/containers/adapter.go`,
   `NewAdapterFromConfig`):
   - `runtime.AutoDetect()` picks the local container runtime.
   - If remote enabled: build `SSHExecutor` with ControlMaster pooling; create
     `HostManager`; register all remote hosts; create `Scheduler` (default
     strategy: `resource_aware`); construct `DefaultDistributor`.
4. **Service boot** (`BootManager.BootAll`):
   - Register endpoints (name, compose file, health check, remote flag).
   - For each endpoint with a compose file: `Adapter.ComposeUp()` → local
     compose or remote compose-via-SSH.
   - Remote compose: SCP compose file + build contexts to host,
     `docker compose -f <file> up -d`.
   - Health checker probes each service (TCP / HTTP). Required services failing
     = boot failure.
5. **Container distribution** (optional, on explicit request):
   - Caller supplies `[]ContainerRequirements` (name, image, CPU / mem / GPU,
     labels).
   - `Distributor.Distribute()` → `Scheduler.ScheduleBatch()` → probes hosts →
     assigns each container to the best host.
   - For each container: SSH `docker run -d` on assigned host, create tunnels,
     mount volumes.
   - Returns `DistributionSummary` (local count, remote count, failures).
6. **Health & monitoring (continuous):** periodic `HealthChecker.CheckAll()` +
   `HostManager.ProbeAll()` for re-balancing inputs.
7. **Shutdown:** `Adapter.Shutdown()` → `Distributor.Undistribute()` → close SSH
   tunnels, unmount volumes, `ComposeDown()` on each compose file.

**The correct workflow is `make build` → run the built binary (in the reference
integration, `./bin/helixagent`).** Never run
`docker compose up` / `podman-compose up` / `make test-infra-start` manually —
they bypass this flow and produce the "works on my machine" class of incident
that CONST-030 exists to prevent.

### Remote distribution

Remote hosts are configured via environment variables or `.env` files. See
`.env.example` for the full template.

**Env-var registration** (`pkg/envconfig/parser.go`): `CONTAINERS_REMOTE_HOST_N_*`
entries, N = 1..100. The loader iterates until a missing `_NAME` is hit.

```bash
# Enable remote distribution
CONTAINERS_REMOTE_ENABLED=true
CONTAINERS_REMOTE_SCHEDULER=resource_aware

# Define remote hosts (numbered 1, 2, 3, ...)
CONTAINERS_REMOTE_HOST_1_NAME=gpu-server-1
CONTAINERS_REMOTE_HOST_1_ADDRESS=192.168.1.100
CONTAINERS_REMOTE_HOST_1_PORT=22
CONTAINERS_REMOTE_HOST_1_USER=deploy
CONTAINERS_REMOTE_HOST_1_KEY=~/.ssh/id_rsa
CONTAINERS_REMOTE_HOST_1_RUNTIME=docker
CONTAINERS_REMOTE_HOST_1_LABELS=gpu=true,arch=amd64
```

Adding a host = append six env vars. No code change, N scales freely (this is
CONST-031).

**Deployment loop** (`pkg/distribution/distributor.go`): for each placement
decision, if `local` → `LocalRuntime.Start(image)`; else → SSH
`docker rm -f <name> 2>/dev/null || true` then
`docker run -d --name <name> <image>`, then `TunnelManager.CreateTunnel()`, then
`VolumeManager.Mount()`, then remote health check.

**SSH ControlMaster pooling** (`pkg/remote/connection_pool.go`): one socket per
`(user@host:port)` in `/tmp/containers-ssh-ctrl/`. `Acquire()` creates the
socket if missing and bumps a ref count; `Release()` decrements. Socket persists
for `ControlPersist` (default 5 min) after ref count hits zero — massive latency
reduction for rapid successive calls.

**Scheduler strategies** (`pkg/scheduler/strategies.go`): `resource_aware`
(default), `round_robin`, `affinity`, `spread`, `bin_pack`.

### Gotchas

1. **ControlMaster socket semantics:** the socket can outlive the last
   `Release()` by `ControlPersist`. If the network blips during that window,
   queued commands can hit a dead socket. Always `IsReachable()`-probe before
   assuming a host is live.
2. **CommandTimeout vs. KeepAlive:** `CONTAINERS_REMOTE_COMMAND_TIMEOUT`
   (default 1800s) bounds the outer SSH command.
   `ServerAliveInterval`×`ServerAliveCountMax` = 30s × 10 = 5 min heartbeat
   tolerance. Never set `CommandTimeout` < `KeepAliveTotal`, or long compose
   builds with multi-GB image pulls will appear to hang and then die.
3. **Context cancellation in `ScheduleBatch`:** host probes run synchronously.
   If ctx cancels mid-probe, Scheduler uses whatever snapshots it has —
   placements may be suboptimal rather than failing. Use a realistic deadline.
4. **Build-context skip:** `RemoteComposeUp` SCPs build contexts to the remote
   host *except* when the context path matches the project root (via
   `filepath.Clean` comparison). `build: { context: . }` pointing at the
   consuming project's root is silently skipped so the whole multi-GB tree isn't
   shipped. This is intentional.
5. **Volume timing:** VolumeManager mounts volumes *after* container start. If a
   container needs the volume at bind-mount time (read-only config at
   entrypoint), it fails. Use retrying health checks or init containers that
   wait for the mount.
6. **No auto-failover:** a failed container is not moved to a backup host
   automatically. `Distribute()` is not idempotent; `Undistribute()` is. Call
   `Rebalance()` or the `Undistribute → Distribute` pair to retry.

### Thread safety notes

- **BootManager** is fully thread-safe. All public methods use `sync.RWMutex` for state protection.
- **Runtime implementations** use per-client locking for API calls.
- **HealthChecker** executes health checks concurrently but uses mutexes for result aggregation.
- **LifecycleManager** uses atomic operations for state transitions.
- **EventBus** (from `pkg/event/`) is thread-safe with internal locking.
- **MetricsCollector** uses `sync.Map` for concurrent metric updates.

### Best practices

#### 1. Always use auto-detection

```go
// Good
runtime, err := runtime.AutoDetect()

// Bad
runtime := runtime.NewDockerRuntime()  // Hardcoded
```

#### 2. Configure health checks for all services

```go
// Good
manager.AddService("redis", boot.ServiceConfig{
    HealthCheck: boot.TCPCheck("localhost", 6379),
})

// Bad - no health check
manager.AddService("redis", boot.ServiceConfig{})
```

#### 3. Mark critical services as required

```go
// Database is critical
manager.AddService("postgres", boot.ServiceConfig{
    Required: true,  // Fail fast if unavailable
})

// Optional service
manager.AddService("optional-cache", boot.ServiceConfig{
    Required: false,  // Continue if unavailable
})
```

#### 4. Use lazy boot for optional services

```go
manager := boot.NewBootManager(
    boot.WithLazyBoot(true),  // Start services on-demand
)
```

#### 5. Monitor resource usage

```go
monitor := monitor.NewResourceMonitor(runtime)
metrics := monitor.GetContainerMetrics("postgres")
if metrics.MemoryPercent > 90.0 {
    logger.Warn("High memory usage detected")
}
```

### Key implementation files

| File | Purpose |
|------|---------|
| `pkg/runtime/runtime.go` | ContainerRuntime interface and implementations |
| `pkg/runtime/docker.go` | Docker client implementation |
| `pkg/runtime/podman.go` | Podman client implementation |
| `pkg/runtime/kubernetes.go` | Kubernetes client implementation |
| `pkg/runtime/autodetect.go` | Runtime auto-detection logic |
| `pkg/compose/compose.go` | ComposeOrchestrator interface |
| `pkg/compose/docker_compose.go` | Docker Compose implementation |
| `pkg/health/health.go` | HealthChecker interface and dispatcher |
| `pkg/health/tcp.go` | TCP health check implementation |
| `pkg/health/http.go` | HTTP health check implementation |
| `pkg/health/grpc.go` | gRPC health check implementation |
| `pkg/lifecycle/lifecycle.go` | LifecycleManager interface |
| `pkg/lifecycle/lazy_boot.go` | Lazy boot implementation |
| `pkg/lifecycle/idle_shutdown.go` | Idle shutdown implementation |
| `pkg/boot/manager.go` | BootManager main orchestration logic |
| `pkg/boot/options.go` | BootManager functional options |
| `pkg/orchestrator/orchestrator.go` | ServiceOrchestrator for auto-discovery and management |
| `pkg/orchestrator/orchestrator_test.go` | Orchestrator unit tests |
| `pkg/ctop/types.go` | Ctop type definitions (ContainerProcess, DisplayConfig) |
| `pkg/ctop/collector.go` | Container data collection from local and remote hosts |
| `pkg/ctop/display.go` | Terminal UI display with sorting and filtering |
| `cmd/ctop/main.go` | Ctop CLI entry point |
| `go.mod` | Module definition and dependencies |
| `README.md` | User-facing documentation with quick start |
| the four agent carriers in this directory | AI coding assistant instructions (this file and its three siblings) |

### Key files a developer touches

- `pkg/distribution/distributor.go` — placement + deployment orchestration.
- `pkg/scheduler/scheduler.go` + `strategies.go` — scheduling logic; add new strategies here.
- `pkg/remote/ssh_executor.go` — SSH execution, timeouts, streaming output.
- `pkg/remote/host_manager.go` — host registry; add host auto-discovery / state callbacks here.
- `pkg/envconfig/parser.go` — env-var loader; add new `CONTAINERS_REMOTE_*` variables here.
- `pkg/orchestrator/orchestrator.go` — multi-service boot ordering, rollback.
- The consuming project side: `internal/adapters/containers/adapter.go` — the single integration point.

### Integration seams

- **Upstream:** none (this module is foundational).
- **Downstream (sibling modules):** `Challenges`, `HelixLLM`, `HelixQA`.
- **Consuming-project consumers:** `internal/adapters/containers/adapter.go`, `internal/services/boot_manager.go`.
- **Hard external dependencies:** SSH client binaries, a container runtime on the local machine (Docker/Podman/etc.), SSH server + container runtime on each configured remote host, SSH network reachability.

### Build and test commands

```bash
# Build all packages
go build ./...

# Run all tests with race detection
go test ./... -count=1 -race

# Run unit tests only (short mode)
go test ./... -short

# Run integration tests (requires Docker/Podman)
go test -tags=integration ./...

# Run benchmarks
go test -bench=. ./tests/benchmark/

# Run a specific test
go test -v -run TestBootManager_Start ./pkg/boot/

# Format code
gofmt -w .

# Vet code
go vet ./...
```

### Code style

- Standard Go conventions, `gofmt` formatting
- Imports grouped: stdlib, third-party, internal (blank line separated)
- Line length ≤ 100 chars
- Naming: `camelCase` private, `PascalCase` exported, acronyms all-caps
- Errors: always check, wrap with `fmt.Errorf("...: %w", err)`
- Tests: table-driven, `testify`, naming `Test<Struct>_<Method>_<Scenario>`

### Commit conventions

Conventional Commits with package scope:

```
feat(runtime): add LXC runtime support
feat(health): add Redis health check strategy
feat(lifecycle): implement graceful shutdown with timeout
fix(boot): prevent race condition in parallel health checks
fix(compose): handle profile selection correctly
test(runtime): add Docker client integration tests
docs(containers): update API reference with lifecycle examples
refactor(health): extract retry logic to separate package
```

### Multi-agent coordination guide

#### Division of work

When multiple agents work on this module simultaneously, divide work by package
boundary:

1. **Runtime Agent** — Owns `pkg/runtime/`. Changes to runtime interface affect compose, lifecycle, and monitor packages. Must coordinate before modifying `ContainerRuntime` interface.
2. **Health Agent** — Owns `pkg/health/`. New health check strategies can be added independently. Changes to `HealthChecker` interface require boot package updates.
3. **Lifecycle Agent** — Owns `pkg/lifecycle/`. Complex lifecycle logic. Coordinates with runtime and event agents for state management.
4. **Boot Agent** — Owns `pkg/boot/`. Integration layer. Requires testing against all package combinations.
5. **Discovery Agent** — Owns `pkg/discovery/`. Independent service discovery logic. Can work in parallel with other agents.
6. **Monitor Agent** — Owns `pkg/monitor/`. Resource tracking. Can work independently but coordinates with runtime for container metrics.
7. **Orchestrator Agent** — Owns `pkg/orchestrator/`. Service orchestration with auto-discovery. Coordinates with compose and remote agents for deployment.
8. **Ctop Agent** — Owns `pkg/ctop/`. Container monitoring with top/htop-style display. Coordinates with remote agent for multi-host collection. Independent display logic.

Remote-distribution ownership continues the same list:

9. **Remote Agent** — Owns `pkg/remote/`. Foundation for all distributed features. Changes to `RemoteExecutor` or `HostManager` interfaces affect scheduler, network, volume, and distribution packages.
10. **Scheduler Agent** — Owns `pkg/scheduler/`. Strategy implementations are independent. Changes to `Scheduler` interface require distribution and boot updates.
11. **Network Agent** — Owns `pkg/network/`. Tunnel management and port allocation. Can work independently.
12. **Volume Agent** — Owns `pkg/volume/`. Volume backend implementations (SSHFS/NFS/rsync) are independent.
13. **Distribution Agent** — Owns `pkg/distribution/`. Top-level orchestrator. Requires testing against all remote packages.
14. **EnvConfig Agent** — Owns `pkg/envconfig/`. Environment parsing. Independent of other packages except `remote` types.

#### Coordination rules

- **Runtime interface changes** require all agents to update. The `ContainerRuntime` interface is the shared contract.
- **Health checker** and **discovery** packages are independent and can be modified in parallel.
- **Boot package** integrates all packages. Any interface change in sub-packages requires corresponding boot updates.
- **Lifecycle** and **event** packages are tightly coupled. Coordinate changes to event types and lifecycle states.
- **Test isolation**: Each package has its own `_test.go` files. Boot tests import all packages for integration scenarios.
- **No circular dependencies**: The dependency graph is strictly acyclic. Never import `boot` from sub-packages.

#### Safe parallel changes

These changes can be made simultaneously without coordination:

- Adding a new runtime implementation (e.g. LXC) to `pkg/runtime/`
- Adding a new health check strategy to `pkg/health/`
- Adding new discovery mechanisms to `pkg/discovery/`
- Adding new event types to `pkg/event/`
- Adding new scheduling strategies to `pkg/scheduler/`
- Adding new volume backends to `pkg/volume/`
- Adding new tests to any package
- Updating documentation

#### Changes requiring coordination

- Modifying the `ContainerRuntime` interface (affects `remote.RemoteRuntime`)
- Changing `HealthChecker` interface signature
- Modifying `RemoteExecutor` interface (affects scheduler, network, volume, distribution)
- Modifying `HostManager` interface (affects scheduler, distribution, boot)
- Modifying `Scheduler` interface (affects distribution, boot)
- Modifying lifecycle state machine
- Adding new configuration fields to `boot.Config`
- Changing event types used across packages
- Modifying metrics schema

### §6.R — No-hardcoding mandate (inherited 2026-05-06, per §6.F)

No connection address, port, header field name, credential, key, salt, secret,
schedule, algorithm parameter, or domain literal in tracked source code. Every
such value MUST come from `.env` (gitignored), generated config, runtime env
var, or mounted file. When this module is consumed inside a project whose root
carrier states §6.R, that statement is the base rule and this section extends
it. This module MAY add stricter rules but MUST NOT relax them.

### Governance carriers in this module

Read [`CONSTITUTION.md`](CONSTITUTION.md) in this directory in full before doing
any work here: it is this module's own constitution, it extends the Helix
Constitution named in the pointer heading at the top of this file, and it binds
every agent family equally.

This module carries four agent carriers — one per agent family — whose bodies
are identical below the per-agent header, per §11.4.157(B). A governance or
module-documentation edit lands in **all four** or in none. The per-agent
variance is confined to the title, the `## INHERITED FROM constitution/<NAME>.md`
heading and the sentences that name the base file, and the "This carrier is read
by ..." line. Verify lockstep from this module's root with the normalising
recipe below — it selects the carriers by the reader line they all carry, so the
recipe itself never names one of them:

```bash
grep -l '^This carrier is read by ' ./*.md | while read -r f; do
  b="$(basename "$f" .md)"
  sed -e "s/${b}/@@SELF@@/g" -e 's|^This carrier is read by .*$|@@READER@@|' "$f" \
    | sha256sum | cut -d' ' -f1
done | sort -u | wc -l
```

The answer must be `1`. Because the recipe substitutes each carrier's **own**
name only, no sentence in the shared body may name a *different* carrier file —
write "the four agent carriers in this directory" instead. That constraint is
the reason this section spells out no filenames.

### Inherited universal covenant anchors (from HelixConstitution)

This module inherits the full universal covenant from the Helix Constitution
(<https://github.com/HelixDevelopment/HelixConstitution>). The anchors below apply
directly — they are UNIVERSAL and project-agnostic (no consuming-project context,
per §11.4.28), so this decoupled module carries them by reference to the
constitution submodule's `Constitution.md`:

- §11.4.66 — Blocker-resolution interactive-clarification mandate. When a blocker
  cannot be resolved from evidence, ask a precise clarifying question rather than
  guess. [full → constitution `Constitution.md` §11.4.66]
- §11.4.67 — Shell-script target-shell-parseability mandate. Every shell script
  MUST parse clean under its declared target shell (`sh -n` / `bash -n`).
  [full → constitution `Constitution.md` §11.4.67]
- §11.4.69 — Universal sink-side positive-evidence taxonomy. Every user-visible
  feature PASS cites a captured-evidence artefact matching the taxonomy shape.
  [full → constitution `Constitution.md` §11.4.69]
- §11.4.85 — Stress + chaos test mandate. Every fix ships full-automation stress
  AND chaos tests with captured-evidence proofs. [full → constitution
  `Constitution.md` §11.4.85]
