package brokertest

// Wave-20 CT-HARDEN-BROK-HARD guards.
//
// These are anti-bluff unit guards (§11.4.115 RED→GREEN, §11.4.27 no real
// infrastructure) for the brokertest hardening batch. They introduce the
// missing seam the package previously lacked: a fake runtime.ContainerRuntime
// (plus a swappable containerRunner) so the resolve / readiness / leak-cleanup
// paths are exercised deterministically WITHOUT starting a real broker
// container. The existing *_test.go files only cover the pure helpers or skip
// when no runtime is present; nothing here needs a container runtime.
//
// HONEST BOUNDARY (§11.4.107): each guard proves a piece of LOGIC —
//   BROK-1 the startup-timeout bound, BROK-2 the run-error cleanup wiring,
//   BROK-3 the PostgreSQL protocol re-verification, BROK-4 the exited-container
//   fast-fail — via the fake runtime + local loopback listeners. They do NOT
//   prove that a real NATS/etcd/Postgres/Redis broker died; that is the job of
//   the real-container integration tests (which t.Skip without a runtime).

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// --- fake runtime.ContainerRuntime seam -------------------------------------

// fakeRuntime implements the full runtime.ContainerRuntime interface. Only
// Status and Remove carry controllable behaviour (the resolve/leak paths only
// touch those); every other method is an inert stub. Remove calls are recorded
// so the BROK-2 cleanup wiring can be asserted.
type fakeRuntime struct {
	nameStr   string
	status    *runtime.ContainerStatus
	statusErr error
	removeErr error

	mu      sync.Mutex
	removed []string
}

func (f *fakeRuntime) Name() string {
	if f.nameStr != "" {
		return f.nameStr
	}
	return "podman"
}
func (f *fakeRuntime) Version(context.Context) (string, error) { return "fake-1.0", nil }
func (f *fakeRuntime) IsAvailable(context.Context) bool        { return true }
func (f *fakeRuntime) Start(context.Context, string, ...runtime.StartOption) error {
	return nil
}
func (f *fakeRuntime) Stop(context.Context, string, ...runtime.StopOption) error { return nil }

func (f *fakeRuntime) Remove(_ context.Context, id string, _ ...runtime.RemoveOption) error {
	f.mu.Lock()
	f.removed = append(f.removed, id)
	f.mu.Unlock()
	return f.removeErr
}

func (f *fakeRuntime) Status(context.Context, string) (*runtime.ContainerStatus, error) {
	return f.status, f.statusErr
}
func (f *fakeRuntime) List(context.Context, runtime.ListFilter) ([]runtime.ContainerInfo, error) {
	return nil, nil
}
func (f *fakeRuntime) Stats(context.Context, string) (*runtime.ContainerStats, error) {
	return nil, nil
}
func (f *fakeRuntime) Exec(context.Context, string, []string) (*runtime.ExecResult, error) {
	return nil, nil
}
func (f *fakeRuntime) Logs(context.Context, string, ...runtime.LogOption) (io.ReadCloser, error) {
	return nil, nil
}

func (f *fakeRuntime) removedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// runWithHangGuard runs fn on a goroutine and returns its error, but FAILS the
// test if fn does not return within budget. It is how a pre-fix infinite hang
// (BROK-1) is registered as a genuine `--- FAIL` rather than an infinite test.
func runWithHangGuard(t *testing.T, budget time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		t.Fatalf("HANG: call did not return within %v — startup bound missing (BROK-1)", budget)
		return nil // unreachable
	}
}

// --- loopback listener helpers (no container runtime) -----------------------

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func portOf(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	return port
}

// serveOnce accepts a single connection and hands it to handle.
func serveOnce(ln net.Listener, handle func(net.Conn)) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	handle(conn)
}

// serveForeverWith accepts every connection until ln closes, handing each to
// handle on its own goroutine. Used to model a bare forwarder that keeps
// accepting connections across the fallback's retry loop.
func serveForeverWith(ln net.Listener, handle func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			handle(c)
		}(conn)
	}
}

// holdOpen models a bare TCP forwarder: it accepts and drains bytes but NEVER
// answers the protocol — exactly the "port bound before the server serves"
// case. It returns when the peer closes (so no goroutine leaks).
func holdOpen(c net.Conn) { _, _ = io.Copy(io.Discard, c) }

// authenticationOK is a minimal PostgreSQL AuthenticationOk backend message:
// type byte 'R', Int32 length 8, Int32 code 0. A serving PostgreSQL answers a
// StartupMessage with a 'R' (Authentication*) message; the probe only inspects
// the first byte, so this is sufficient to model a protocol-speaking server.
var authenticationOK = []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}

// speakPG models a serving PostgreSQL: read (and discard) the StartupMessage,
// then answer with AuthenticationOk.
func speakPG(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
	_, _ = c.Read(make([]byte, 512))
	_, _ = c.Write(authenticationOK)
}

// ===========================================================================
// BROK-1 — internal startup-timeout bound (no infinite hang on deadline-less ctx)
// ===========================================================================

// TestStartupContext_BoundsDeadlinelessContext is the direct RED→GREEN on the
// BROK-1 fix: a caller ctx WITHOUT a deadline MUST gain one so the resolve /
// readiness loops cannot spin forever; a caller ctx that already has a deadline
// MUST be returned unchanged (the caller's bound wins).
func TestStartupContext_BoundsDeadlinelessContext(t *testing.T) {
	ctx, cancel := startupContext(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("startupContext did not bound a deadline-less context (BROK-1)")
	}

	parent, pcancel := context.WithTimeout(context.Background(), time.Hour)
	defer pcancel()
	got, gcancel := startupContext(parent, 50*time.Millisecond)
	defer gcancel()
	gotDL, ok1 := got.Deadline()
	parentDL, ok2 := parent.Deadline()
	if !ok1 || !ok2 || !gotDL.Equal(parentDL) {
		t.Fatal("startupContext overrode a caller-supplied deadline")
	}
}

// TestWaitReady_DeadlinelessStartupCtxDoesNotHang reproduces the task's BROK-1
// scenario: the port resolves but nothing ever listens, so waitReady dials a
// refused endpoint. Wrapped in startupContext, a deadline-less caller is bounded
// and MUST return an error instead of hanging. Pre-fix (startupContext reverted
// to a pass-through) this HANGS and the hang-guard turns it into a FAIL.
func TestWaitReady_DeadlinelessStartupCtxDoesNotHang(t *testing.T) {
	port := freeClosedPort(t) // nothing listening

	err := runWithHangGuard(t, 5*time.Second, func() error {
		phaseCtx, cancel := startupContext(context.Background(), 300*time.Millisecond)
		defer cancel()
		return waitReady(phaseCtx, port)
	})
	if err == nil {
		t.Fatal("waitReady returned nil for a port with nothing listening")
	}
}

// TestResolveHostPort_DeadlinelessStartupCtxDoesNotHang drives the fake: a
// container reported running that NEVER publishes a port makes resolveHostPort
// loop. Wrapped in startupContext a deadline-less caller is bounded and MUST
// fail rather than hang (BROK-1). No real container (§11.4.27).
func TestResolveHostPort_DeadlinelessStartupCtxDoesNotHang(t *testing.T) {
	fake := &fakeRuntime{status: &runtime.ContainerStatus{State: runtime.StateRunning}}

	err := runWithHangGuard(t, 5*time.Second, func() error {
		phaseCtx, cancel := startupContext(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, e := resolveHostPort(phaseCtx, fake, "brokertest-nats-guard", "")
		return e
	})
	if err == nil {
		t.Fatal("resolveHostPort returned nil for a never-serving container")
	}
}

// ===========================================================================
// BROK-2 — container leak cleanup on the run-error path
// ===========================================================================

// TestRunNewContainer_RemovesContainerOnRunError proves the BROK-2 wiring: a
// `run -d` that fails after (potentially) creating the container MUST trigger a
// best-effort Remove of the deterministically-named container so nothing leaks
// (§11.4.14). Drives the fake runtime + a failing runner — no real container.
// Surgical revert of the removeContainer call on the error path drops the
// recorded Remove and this FAILs.
func TestRunNewContainer_RemovesContainerOnRunError(t *testing.T) {
	fake := &fakeRuntime{}
	failRun := func(context.Context, string, []string) ([]byte, error) {
		return []byte("Error: rootlessport listen tcp 127.0.0.1:4222: bind: address already in use"),
			errors.New("exit status 126")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const name = "brokertest-nats-leaktest-1"
	_, err := runNewContainer(ctx, fake, failRun, "podman", name, []string{"run", "-d"})
	if err == nil {
		t.Fatal("runNewContainer returned nil error when the run failed")
	}
	removed := fake.removedNames()
	if len(removed) != 1 || removed[0] != name {
		t.Fatalf("BROK-2: expected Remove(%q) on the run-error path, got %v", name, removed)
	}
}

// TestRunNewContainer_NoRemoveOnRunSuccess is the negative control: a successful
// run must NOT remove the just-created container. Proves the cleanup is scoped
// strictly to the error path.
func TestRunNewContainer_NoRemoveOnRunSuccess(t *testing.T) {
	fake := &fakeRuntime{}
	okRun := func(context.Context, string, []string) ([]byte, error) {
		return []byte("deadbeefcafe\n"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := runNewContainer(ctx, fake, okRun, "podman", "brokertest-nats-ok-1", nil); err != nil {
		t.Fatalf("runNewContainer errored on a successful run: %v", err)
	}
	if got := fake.removedNames(); len(got) != 0 {
		t.Fatalf("BROK-2 negative control: a successful run must not remove; got %v", got)
	}
}

// ===========================================================================
// BROK-3 — PostgreSQL fallback re-verifies at the protocol layer (no fixed sleep)
// ===========================================================================

// TestPgStartupProbe_AcceptsProtocolSpeakingServer is the golden-good half of
// the §11.4.107(10) analyzer self-validation: a server that answers a
// StartupMessage with an Authentication ('R') message is accepted.
func TestPgStartupProbe_AcceptsProtocolSpeakingServer(t *testing.T) {
	ln := mustListen(t)
	defer ln.Close()
	go serveOnce(ln, speakPG)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := pgStartupProbe(ctx, portOf(t, ln))
	if err != nil || !ok {
		t.Fatalf("pgStartupProbe rejected a protocol-speaking PostgreSQL server: ok=%v err=%v", ok, err)
	}
}

// TestPgStartupProbe_RejectsBareTCPAccept is the golden-bad half: a forwarder
// that ACCEPTS TCP but never speaks the protocol (the exact false-ready case
// BROK-3 targets) MUST be rejected.
func TestPgStartupProbe_RejectsBareTCPAccept(t *testing.T) {
	ln := mustListen(t)
	defer ln.Close()
	go serveOnce(ln, holdOpen)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	ok, err := pgStartupProbe(ctx, portOf(t, ln))
	if ok {
		t.Fatal("pgStartupProbe accepted a bare TCP endpoint that never spoke the PG protocol (BROK-3)")
	}
	if err == nil {
		t.Fatal("expected an error for a bare-accept endpoint")
	}
}

// TestPgStartupProbe_RejectsNonPGResponse is a second golden-bad: a server that
// answers with a non-PG first byte MUST be rejected.
func TestPgStartupProbe_RejectsNonPGResponse(t *testing.T) {
	ln := mustListen(t)
	defer ln.Close()
	go serveOnce(ln, func(c net.Conn) {
		_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, _ = c.Read(make([]byte, 512))
		_, _ = c.Write([]byte("XNOTPG\r\n"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := pgStartupProbe(ctx, portOf(t, ln))
	if ok || err == nil {
		t.Fatalf("pgStartupProbe accepted a non-PG first byte: ok=%v err=%v", ok, err)
	}
}

// TestWaitPostgresProtocolReady_RejectsBareTCPAccept is the wiring RED→GREEN for
// the BROK-3 fallback. The exec-unsupported fallback now re-probes at the
// protocol layer instead of "TCP accept + fixed 2s sleep + return nil", so a
// bare-accept endpoint MUST yield an error (a timeout), never a false-ready nil.
// Surgical revert of waitPostgresProtocolReady to the old fixed-sleep body makes
// this return nil (a false ready) and FAILs. The 3s ctx is deliberately longer
// than the old 2s sleep so the reverted body reaches its unconditional success.
func TestWaitPostgresProtocolReady_RejectsBareTCPAccept(t *testing.T) {
	ln := mustListen(t)
	defer ln.Close()
	go serveForeverWith(ln, holdOpen)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := waitPostgresProtocolReady(ctx, portOf(t, ln))
	if err == nil {
		t.Fatal("BROK-3: protocol fallback accepted a bare-TCP endpoint that never spoke PG (false ready)")
	}
	if time.Since(start) < 2500*time.Millisecond {
		t.Fatalf("fallback returned too early (%v); it is not re-probing / honoring ctx", time.Since(start))
	}
}

// TestWaitPostgresProtocolReady_AcceptsProtocolSpeakingServer is the positive
// side: a PG-speaking endpoint is accepted promptly by the fallback.
func TestWaitPostgresProtocolReady_AcceptsProtocolSpeakingServer(t *testing.T) {
	ln := mustListen(t)
	defer ln.Close()
	go serveForeverWith(ln, speakPG)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := waitPostgresProtocolReady(ctx, portOf(t, ln)); err != nil {
		t.Fatalf("protocol fallback rejected a PG-speaking server: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("fallback took too long (%v) to accept a serving PG endpoint", time.Since(start))
	}
}

// ===========================================================================
// BROK-4 — exited/dead container fast-fail
// ===========================================================================

// TestResolve_FailsFastOnExitedContainer drives the fake: the container is
// reported already Stopped/Dead (exited) while STILL carrying a port mapping.
// Pre-fix, resolve* handed back that dead container's port (or looped to the
// startup timeout); post-fix it fails fast with an "exited" error and returns
// NO usable port (BROK-4). Covers all four broker resolvers × both terminal
// states. Surgical revert of the exitedState check makes resolve return the
// port with a nil error and this FAILs. No real container (§11.4.27).
func TestResolve_FailsFastOnExitedContainer(t *testing.T) {
	resolvers := []struct {
		name string
		port string
		fn   func(context.Context, runtime.ContainerRuntime, string, string) (string, error)
	}{
		{"nats", natsClientPort, resolveHostPort},
		{"etcd", etcdClientPort, resolveEtcdHostPort},
		{"postgres", postgresPort, resolvePostgresHostPort},
		{"redis", redisPort, resolveRedisHostPort},
	}
	states := []runtime.ContainerState{runtime.StateStopped, runtime.StateDead}

	for _, r := range resolvers {
		for _, state := range states {
			fake := &fakeRuntime{status: &runtime.ContainerStatus{
				State:    state,
				ExitCode: 1,
				Ports:    []runtime.PortMapping{{ContainerPort: r.port, HostPort: "54321"}},
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			port, err := r.fn(ctx, fake, "brokertest-"+r.name+"-exited", "")
			cancel()

			if err == nil {
				t.Fatalf("%s/%s: resolve accepted an exited container's port %q — BROK-4 check missing",
					r.name, state, port)
			}
			if !strings.Contains(err.Error(), "exited") {
				t.Fatalf("%s/%s: expected an 'exited' error, got %v", r.name, state, err)
			}
			if port != "" {
				t.Fatalf("%s/%s: resolve returned a usable port %q for an exited container",
					r.name, state, port)
			}
		}
	}
}

// TestResolveHostPort_RunningContainerStillResolves is the negative control: a
// RUNNING container that publishes its port must still resolve — the BROK-4
// fast-fail must not over-trigger on healthy states.
func TestResolveHostPort_RunningContainerStillResolves(t *testing.T) {
	fake := &fakeRuntime{status: &runtime.ContainerStatus{
		State: runtime.StateRunning,
		Ports: []runtime.PortMapping{{ContainerPort: natsClientPort, HostPort: "45678"}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	port, err := resolveHostPort(ctx, fake, "brokertest-nats-live", "")
	if err != nil {
		t.Fatalf("resolveHostPort errored for a healthy running container: %v", err)
	}
	if port != "45678" {
		t.Fatalf("resolveHostPort = %q, want 45678", port)
	}
}

// Run satisfies the ContainerRuntime interface's ephemeral-run primitive. This
// fake does not exercise it; it returns an empty result rather than a nil one
// so a caller can never dereference nil on a nil error.
func (f *fakeRuntime) Run(
	_ context.Context, _ string, _ []string, _ ...runtime.RunOption,
) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
