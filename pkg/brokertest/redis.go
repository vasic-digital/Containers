package brokertest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// redisImage is the pinned single-node Redis image. The alpine variant keeps the
// pull and memory footprint tiny while still serving real RESP traffic. A pinned
// minor tag keeps test runs reproducible.
const redisImage = "docker.io/library/redis:7-alpine"

// redisPort is the in-container port Redis listens on for clients.
const redisPort = "6379"

// redisDefaultMemoryLimit caps the Redis container's memory. Per the module's
// host-session safety mandate (a real OOM incident) every container we own MUST
// declare an explicit memory limit. A single-node Redis idles well under this.
const redisDefaultMemoryLimit = "128m"

// redisNameCounter guarantees unique container names without relying on time or
// math/rand. Combined with the process PID it is unique across concurrent
// StartRedis calls within a process and across processes on the host.
var redisNameCounter uint64

// redisConfig holds resolved options for StartRedis.
type redisConfig struct {
	image       string
	memoryLimit string
	hostPort    string // empty => let the runtime pick a free host port
}

func defaultRedisConfig() *redisConfig {
	return &redisConfig{
		image:       redisImage,
		memoryLimit: redisDefaultMemoryLimit,
		hostPort:    "",
	}
}

// applyRedisOptions translates the shared Option set onto a redisConfig. It
// reuses the same WithImage / WithMemoryLimit / WithHostPort options as
// StartNATS by replaying them against a throwaway config seeded with sentinel
// values, so any field the caller actually set becomes distinguishable from one
// left untouched — regardless of whether the caller's value happens to equal a
// NATS default. WithJetStream is silently ignored for Redis.
//
// The sentinels are non-empty strings no real caller would pass; WithImage and
// WithMemoryLimit ignore empty input, so an empty sentinel could not be
// overwritten — hence the distinctive markers below.
func applyRedisOptions(opts []Option) *redisConfig {
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
	cfg := defaultRedisConfig()
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

// StartRedis launches an ephemeral single-node Redis and returns its client
// address ("127.0.0.1:<port>"), a stop func that removes the container, and an
// error. It selects a container runtime via pkg/runtime (podman/docker/nerdctl
// auto-detect). It honors ctx for the startup timeout: readiness is gated on a
// real RESP PING→PONG handshake against the published client port — proving the
// Redis server is actually serving commands, not merely that the TCP port is
// bound.
//
// The returned address is a bare host:port (the form go-redis and most clients
// take as Addr), NOT a redis:// URL — mirroring the host:port shape callers wire
// straight into a redis.Options{Addr: ...}.
//
// The returned stop function is always non-nil and is safe to call exactly once
// or many times (idempotent). Callers should `defer stop()` immediately after a
// nil-error return to guarantee cleanup even if a later assertion fails.
func StartRedis(ctx context.Context, opts ...Option) (addr string, stop func(), err error) {
	cfg := applyRedisOptions(opts)

	rt, err := runtime.AutoDetect(ctx)
	if err != nil {
		return "", noopStop, fmt.Errorf("brokertest: no container runtime: %w", err)
	}
	binary, ok := runtimeBinary(rt.Name())
	if !ok {
		return "", noopStop, fmt.Errorf(
			"brokertest: runtime %q does not support one-off image runs", rt.Name())
	}

	name := fmt.Sprintf("brokertest-redis-%d-%d", os.Getpid(), atomic.AddUint64(&redisNameCounter, 1))

	args := []string{
		"run", "-d",
		"--name", name,
		"--memory", cfg.memoryLimit,
		// Publish the client port to a host loopback port.
		"-p", hostPortSpec(cfg.hostPort) + redisPort,
		cfg.image,
		// Bound Redis's own maxmemory inside the container as a second belt to
		// the cgroup --memory cap, with an eviction policy so a test that floods
		// keys degrades gracefully rather than OOM-killing the container.
		"redis-server",
		"--maxmemory", "64mb",
		"--maxmemory-policy", "allkeys-lru",
	}

	// The one capability pkg/runtime's interface lacks: run a NEW container from
	// an image with published ports. Done via the detected runtime's CLI,
	// mirroring StartNATS / StartEtcd.
	runOut, runErr := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if runErr != nil {
		return "", noopStop, fmt.Errorf(
			"brokertest: %s run failed: %w: %s", binary, runErr, strings.TrimSpace(string(runOut)))
	}

	// From here on, ALL lifecycle goes back through pkg/runtime so the shared
	// abstraction owns teardown. Build the stop closure first so any later
	// failure path can still clean up.
	stop = makeStop(rt, name)

	hostPort, err := resolveRedisHostPort(ctx, rt, name, cfg.hostPort)
	if err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: resolve host port: %w", err)
	}

	addr = net.JoinHostPort("127.0.0.1", hostPort)
	if err := waitRedisReady(ctx, hostPort); err != nil {
		stop()
		return "", noopStop, fmt.Errorf("brokertest: redis not ready at %s: %w", addr, err)
	}

	return addr, stop, nil
}

// resolveRedisHostPort returns the host port mapped to the Redis client port.
// When the caller pinned a port we trust it; otherwise we ask pkg/runtime to
// inspect the container and report the runtime-assigned port.
func resolveRedisHostPort(
	ctx context.Context, rt runtime.ContainerRuntime, name, pinned string,
) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	var lastErr error
	for {
		st, err := rt.Status(ctx, name)
		if err == nil {
			if p := redisClientHostPort(st); p != "" {
				return p, nil
			}
			lastErr = fmt.Errorf("no host port published for container port %s", redisPort)
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

// redisClientHostPort extracts the host port mapped to the Redis client port
// from a container status reported by pkg/runtime.
func redisClientHostPort(st *runtime.ContainerStatus) string {
	if st == nil {
		return ""
	}
	for _, pm := range st.Ports {
		if pm.ContainerPort == redisPort && pm.HostPort != "" {
			return pm.HostPort
		}
	}
	return ""
}

// waitRedisReady blocks until a RESP PING against the host port yields a PONG,
// or ctx expires. Redis only answers PING with +PONG once it is actually
// accepting commands, so a positive PONG read is the sink-side evidence that the
// broker is operational — not merely that the TCP port is bound.
func waitRedisReady(ctx context.Context, hostPort string) error {
	addr := net.JoinHostPort("127.0.0.1", hostPort)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		ok, err := readsRedisPong(ctx, addr)
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

// readsRedisPong dials addr, sends an inline RESP PING, and returns true only if
// the server replies with a "+PONG" simple string. This is the positive,
// sink-side evidence that the Redis broker is operational, as required by the
// module's anti-bluff mandate. It also serves as a TCP-reachability probe
// because the write/read must first connect.
//
// It uses the inline-command form ("PING\r\n") which redis-server accepts on a
// fresh connection and answers with "+PONG\r\n", avoiding any client dependency
// beyond the standard library.
func readsRedisPong(ctx context.Context, addr string) (bool, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return false, err
	}

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return false, err
	}
	// Redis answers an inline PING with the simple string "+PONG\r\n".
	if strings.HasPrefix(string(buf[:n]), "+PONG") {
		return true, nil
	}
	return false, fmt.Errorf("unexpected PING reply %q", string(buf[:n]))
}
