package brokertest

// Wave-20 DEEPER (§11.4.118) 2nd-pass hardening guard — BROK2-1.
//
// The first-pass BROK-4 fast-fail (an already-exited/dead container makes the
// resolve loop fail immediately instead of spinning to the 90s startup timeout)
// lives INSIDE the auto-port inspection loop. The pinned-host-port branch
// (`WithHostPort`) returned the caller's port with NO status check, so BROK-4
// never applied to it: a pinned-port caller whose container exited immediately
// would spin the downstream readiness loop all the way to the startup timeout.
// BROK2-1 (failIfExited, wired into all four resolvers' pinned branch) closes
// that gap.
//
// HONEST BOUNDARY (§11.4.107): these guards prove the LOGIC of the pinned-path
// fast-fail via the fake runtime.ContainerRuntime (§11.4.27 — no real broker
// container). They reuse the fakeRuntime seam introduced by the first-pass
// wave20_brokhard_test.go. They do NOT prove a real container exited — that is
// the job of the real-container integration tests (which t.Skip without a
// runtime).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// brok2Resolvers is the set of pinned-port resolvers BROK2-1 hardens. Kept
// local to this test; mirrors the resolver table in the BROK-4 guard.
var brok2Resolvers = []struct {
	name string
	fn   func(context.Context, runtime.ContainerRuntime, string, string) (string, error)
}{
	{"nats", resolveHostPort},
	{"etcd", resolveEtcdHostPort},
	{"postgres", resolvePostgresHostPort},
	{"redis", resolveRedisHostPort},
}

// TestWave20_BROK2_PinnedPortFailsFastOnExitedContainer is the RED→GREEN on the
// BROK2-1 fix. A pinned host port whose container has ALREADY exited/died MUST
// fast-fail with an "exited" error and hand back NO usable port — the same
// contract the auto-port path already honors. Covers all four resolvers × both
// terminal states. Surgical revert of a resolver's `failIfExited` anchor line
// makes that resolver return the pinned port with a nil error (the pre-fix
// behaviour) and this FAILs. No real container (§11.4.27).
func TestWave20_BROK2_PinnedPortFailsFastOnExitedContainer(t *testing.T) {
	const pinned = "15432" // caller-pinned host port (WithHostPort)
	states := []runtime.ContainerState{runtime.StateStopped, runtime.StateDead}

	for _, r := range brok2Resolvers {
		for _, state := range states {
			// The exited container still carries a port mapping to prove the
			// fast-fail does not depend on the mapping being absent — the pinned
			// port is what a pre-fix resolver would (wrongly) hand back.
			fake := &fakeRuntime{status: &runtime.ContainerStatus{
				State:    state,
				ExitCode: 1,
				Ports:    []runtime.PortMapping{{ContainerPort: "4222", HostPort: pinned}},
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			port, err := r.fn(ctx, fake, "brokertest-"+r.name+"-pinned-exited", pinned)
			cancel()

			if err == nil {
				t.Fatalf("%s/%s: pinned resolve accepted an exited container (returned %q) — BROK2-1 fast-fail missing",
					r.name, state, port)
			}
			if !strings.Contains(err.Error(), "exited") {
				t.Fatalf("%s/%s: expected an 'exited' error, got %v", r.name, state, err)
			}
			if port != "" {
				t.Fatalf("%s/%s: pinned resolve returned a usable port %q for an exited container",
					r.name, state, port)
			}
		}
	}
}

// TestWave20_BROK2_PinnedPortRunningContainerStillResolves is the negative
// control proving BROK2-1 is a STRICT superset: a pinned port whose container
// is RUNNING must still resolve to that exact pinned port (the fast-fail must
// not over-trigger on a healthy container).
func TestWave20_BROK2_PinnedPortRunningContainerStillResolves(t *testing.T) {
	const pinned = "15432"
	for _, r := range brok2Resolvers {
		fake := &fakeRuntime{status: &runtime.ContainerStatus{State: runtime.StateRunning}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		port, err := r.fn(ctx, fake, "brokertest-"+r.name+"-pinned-live", pinned)
		cancel()

		if err != nil {
			t.Fatalf("%s: pinned resolve errored for a healthy running container: %v", r.name, err)
		}
		if port != pinned {
			t.Fatalf("%s: pinned resolve = %q, want the pinned port %q", r.name, port, pinned)
		}
	}
}

// TestWave20_BROK2_PinnedPortStatusErrorTrustsPin is the second negative
// control proving BROK2-1 preserves prior behaviour on a status inspection
// ERROR: the caller's pin is still trusted (only a CONFIRMED exited container
// short-circuits), so a Status error must NOT block a pinned resolve.
func TestWave20_BROK2_PinnedPortStatusErrorTrustsPin(t *testing.T) {
	const pinned = "15432"
	for _, r := range brok2Resolvers {
		fake := &fakeRuntime{statusErr: errors.New("inspect: no such container (transient)")}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		port, err := r.fn(ctx, fake, "brokertest-"+r.name+"-pinned-statuserr", pinned)
		cancel()

		if err != nil {
			t.Fatalf("%s: pinned resolve errored on a transient Status error (pin must be trusted): %v", r.name, err)
		}
		if port != pinned {
			t.Fatalf("%s: pinned resolve = %q on Status error, want the trusted pinned port %q", r.name, port, pinned)
		}
	}
}
