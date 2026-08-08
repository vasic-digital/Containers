package brokertest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// etcdImage is the default single-node etcd image; v3.5.17 verified to pull and
// serve GET /health on its client port. NOTE (BROK-5): a patch tag like
// "v3.5.17" is stable at the registry today but is NOT a cryptographic guarantee
// of byte-for-byte reproducibility — only an immutable "@sha256:<digest>"
// reference is. Callers needing pinned reproducibility can pass
// WithImage("quay.io/coreos/etcd@sha256:...").
const etcdImage = "quay.io/coreos/etcd:v3.5.17"

// etcdClientPort is the in-container port etcd listens on for clients.
const etcdClientPort = "2379"

// etcdPeerPort is the in-container port etcd listens on for peers. Only used
// inside the single-node cluster; it is NOT published to the host.
const etcdPeerPort = "2380"

// etcdDefaultMemoryLimit caps the etcd container's memory. Per the module's
// host-session safety mandate (a real OOM incident) every container we own MUST
// declare an explicit memory limit. A single-node etcd idles well under this.
const etcdDefaultMemoryLimit = "256m"

// etcdNameCounter guarantees unique container names without relying on time or
// math/rand. Combined with the process PID it is unique across concurrent
// StartEtcd calls within a process and across processes on the host.
var etcdNameCounter uint64

// etcdConfig holds resolved options for StartEtcd.
type etcdConfig struct {
	image       string
	memoryLimit string
	hostPort    string // empty => let the runtime pick a free host port
}

func defaultEtcdConfig() *etcdConfig {
	return &etcdConfig{
		image:       etcdImage,
		memoryLimit: etcdDefaultMemoryLimit,
		hostPort:    "",
	}
}

// applyEtcdOptions translates the shared Option set onto an etcdConfig. It
// reuses the same WithImage / WithMemoryLimit / WithHostPort options as
// StartNATS by replaying them against a throwaway config seeded with sentinel
// values, so any field the caller actually set becomes distinguishable from one
// left untouched — regardless of whether the caller's value happens to equal a
// NATS default. WithJetStream is silently ignored for etcd.
//
// The sentinels are non-empty strings no real caller would pass; WithImage and
// WithMemoryLimit ignore empty input, so an empty sentinel could not be
// overwritten — hence the distinctive markers below.
func applyEtcdOptions(opts []Option) *etcdConfig {
	const (
		sentinelImage  = "\x00brokertest-unset-image\x00"
		sentinelMemory = "\x00brokertest-unset-memory\x00"
	)
	probe := &config{
		image:       sentinelImage,
		memoryLimit: sentinelMemory,
		hostPort:    "",
	}
	for _, o := range opts {
		o(probe)
	}
	cfg := defaultEtcdConfig()
	if probe.image != sentinelImage {
		cfg.image = probe.image
	}
	if probe.memoryLimit != sentinelMemory {
		cfg.memoryLimit = probe.memoryLimit
	}
	if probe.hostPort != "" {
		cfg.hostPort = probe.hostPort
	}
	return cfg
}

// StartEtcd launches an ephemeral single-node etcd and returns its client
// endpoint ("http://127.0.0.1:<port>"), a stop func that removes the container,
// and an error. It selects a container runtime via pkg/runtime
// (podman/docker/nerdctl auto-detect). It honors ctx for the startup timeout:
// readiness is gated on a real HTTP GET to the published client port's /health
// endpoint that confirms etcd reports {"health":"true"} — not merely that the
// TCP port is bound.
//
// The returned stop function is always non-nil and is safe to call exactly once
// or many times (idempotent). Callers should `defer stop()` immediately after a
// nil-error return to guarantee cleanup even if a later assertion fails.
func StartEtcd(ctx context.Context, opts ...Option) (endpoint string, stop func(), err error) {
	cfg := applyEtcdOptions(opts)

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return "", noopStop, fmt.Errorf("brokertest: no container runtime: %w", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		return "", noopStop, fmt.Errorf(
			"brokertest: runtime %q does not support one-off image runs", rt.Name())
	}

	name := fmt.Sprintf("brokertest-etcd-%d-%d", os.Getpid(), atomic.AddUint64(&etcdNameCounter, 1))

	args := []string{
		"run", "-d",
		"--name", name,
		"--memory", cfg.memoryLimit,
		// Publish only the client port to a host loopback port. The peer port
		// stays container-internal for the single-node cluster.
		"-p", hostPortSpec(cfg.hostPort) + etcdClientPort,
		cfg.image,
		"etcd", "--name", "n0",
		"--listen-client-urls", "http://0.0.0.0:" + etcdClientPort,
		"--advertise-client-urls", "http://127.0.0.1:" + etcdClientPort,
		"--listen-peer-urls", "http://0.0.0.0:" + etcdPeerPort,
		"--initial-cluster", "n0=http://0.0.0.0:" + etcdPeerPort,
		"--initial-advertise-peer-urls", "http://0.0.0.0:" + etcdPeerPort,
	}

	// The one capability pkg/runtime's interface lacks: run a NEW container from
	// an image with published ports. Done via the detected runtime's CLI,
	// mirroring StartNATS. On a run failure runNewContainer best-effort removes
	// any container the failed run may have created before returning (BROK-2).
	runOut, runErr := runNewContainer(ctx, rt, execRun, binary, name, args)
	if runErr != nil {
		return "", noopStop, fmt.Errorf(
			"brokertest: %s run failed: %w: %s", binary, runErr, strings.TrimSpace(string(runOut)))
	}

	// From here on, ALL lifecycle goes back through pkg/runtime so the shared
	// abstraction owns teardown. Build the stop closure first so any later
	// failure path can still clean up.
	stop = makeStop(rt, name)

	// Bound the resolve + readiness phase so a container that starts but never
	// serves fails with a clear timeout instead of hanging on a deadline-less
	// ctx (BROK-1, §11.4.108). A caller-supplied deadline is respected as-is.
	phaseCtx, cancelPhase := startupContext(ctx, defaultStartupTimeout)
	defer cancelPhase()

	hostPort, err := resolveEtcdHostPort(phaseCtx, rt, name, cfg.hostPort)
	if err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: resolve host port: %w", err)
	}

	endpoint = fmt.Sprintf("http://127.0.0.1:%s", hostPort)
	if err := waitEtcdReady(phaseCtx, hostPort); err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: etcd not ready at %s: %w", endpoint, err)
	}

	return endpoint, stop, nil
}

// resolveEtcdHostPort returns the host port mapped to the etcd client port.
// When the caller pinned a port we trust it; otherwise we ask pkg/runtime to
// inspect the container and report the runtime-assigned port.
func resolveEtcdHostPort(
	ctx context.Context, rt runtime.ContainerRuntime, name, pinned string,
) (string, error) {
	if pinned != "" {
		if err := failIfExited(ctx, rt, name); err != nil { // BROK2-1
			return "", err
		}
		return pinned, nil
	}
	var lastErr error
	for {
		st, err := rt.Status(ctx, name)
		if err == nil {
			// Fail fast if the container has already exited/died (BROK-4).
			if exitedState(st) {
				return "", fmt.Errorf(
					"container %s exited before becoming ready (state=%s, exit_code=%d)",
					name, st.State, st.ExitCode)
			}
			if p := etcdClientHostPort(st); p != "" {
				return p, nil
			}
			lastErr = fmt.Errorf("no host port published for container port %s", etcdClientPort)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// etcdClientHostPort extracts the host port mapped to the etcd client port from
// a container status reported by pkg/runtime.
func etcdClientHostPort(st *runtime.ContainerStatus) string {
	if st == nil {
		return ""
	}
	for _, pm := range st.Ports {
		if pm.ContainerPort == etcdClientPort && pm.HostPort != "" {
			return pm.HostPort
		}
	}
	return ""
}

// waitEtcdReady blocks until GET http://127.0.0.1:<hostPort>/health reports
// {"health":"true"}, or ctx expires. etcd only serves a healthy /health once it
// is actually accepting client requests, so a positive health read is sink-side
// evidence the broker is operational — not merely that the port is bound.
func waitEtcdReady(ctx context.Context, hostPort string) error {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		ok, err := readsEtcdHealth(ctx, hostPort)
		if ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// readsEtcdHealth issues GET /health against the host port and returns true only
// if etcd responds 200 with a body whose "health" field is "true". This is the
// positive, sink-side evidence that the etcd broker is operational, as required
// by the module's anti-bluff mandate. It also serves as a TCP-reachability probe
// because the HTTP client must first connect.
func readsEtcdHealth(ctx context.Context, hostPort string) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", hostPort)
	url := "http://" + addr + "/health"

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// etcd returns {"health":"true","reason":""}. Match on the health field
	// being true without pulling in a JSON dependency beyond stdlib; a simple
	// substring check on the canonical token is sufficient and robust.
	if strings.Contains(string(body), `"health":"true"`) {
		return true, nil
	}
	return false, fmt.Errorf("etcd not healthy: %s", strings.TrimSpace(string(body)))
}
