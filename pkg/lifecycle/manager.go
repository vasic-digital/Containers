package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/health"
)

// LifecycleManager controls the full lifecycle of registered
// services including lazy boot, idle shutdown, and concurrency
// limiting.
type LifecycleManager interface {
	// Register adds a service specification to the manager.
	Register(spec ServiceSpec) error

	// Start starts the named service and waits for it to become
	// healthy.
	Start(ctx context.Context, name string) error

	// Stop stops the named service.
	Stop(ctx context.Context, name string) error

	// Acquire obtains a lease on the service, starting it if
	// needed (lazy boot). The returned ReleaseFunc must be called
	// when the caller is finished.
	Acquire(ctx context.Context, name string) (ReleaseFunc, error)

	// Status returns the current lifecycle status of a service.
	Status(name string) (ServiceLifecycleStatus, error)

	// Shutdown gracefully stops all managed services.
	Shutdown(ctx context.Context) error
}

// acquireRaceHook, when non-nil, is invoked by Acquire in the window
// between the semaphore acquisition and the post-acquire state
// re-validation. Production leaves it nil (a zero-cost no-op); tests
// override it to deterministically interleave a concurrent Stop into the
// exact LIFE-1 race window (the dead-lease-on-torn-down-service interleave)
// without depending on the real-clock idle timer firing. Same nil-default
// package-seam idiom as pkg/vm/teardown.go's killByPortHook. Tests that
// override it MUST NOT use t.Parallel() — the swap-and-restore pattern is not
// safe against concurrent test functions racing the package-level var.
var acquireRaceHook func()

// startFollowerWaitHook, when non-nil, is invoked by a follower Start (one
// that observed state=="starting") immediately before it blocks on the
// leader's completion channel. Production leaves it nil; tests override it to
// deterministically sequence the LIFE-2 leader/follower coalescing interleave.
// Same nil-default idiom + non-parallel constraint as acquireRaceHook.
var startFollowerWaitHook func()

// startOp coalesces concurrent Start() calls onto a single in-flight boot.
// The leader (the caller that transitions a service stopped→starting)
// publishes a fresh *startOp; every follower that observes "starting"
// captures this pointer, blocks on done, and returns err — the leader's REAL
// boot outcome. op.err is written exactly once by the leader before
// close(op.done), so a follower's receive-then-read is race-free without an
// extra lock (LIFE-2 coalesced-Start fabricated-success fix).
type startOp struct {
	done chan struct{}
	err  error
}

// serviceEntry holds runtime state for a single managed service.
type serviceEntry struct {
	spec       ServiceSpec
	state      string
	healthy    bool
	lastStart  time.Time
	lastStop   time.Time
	lastAcq    time.Time
	semaphore  *ConcurrencySemaphore
	idleCtrl   *IdleShutdown
	lazyBooter *LazyBooter
	// activeLeases counts currently-held Acquire leases: incremented when a
	// lease is granted, decremented exactly once per release (guaranteed by
	// the sync.Once in the release closure). The idle-shutdown controller
	// consults it via its busy predicate so a service is never reclaimed
	// while a lease is still held (LIFE-(a) IDLE-vs-LEASE fix).
	activeLeases atomic.Int32
	// startOp is the in-flight boot's coalescing handle (LIFE-2). Non-nil
	// only while state=="starting"; written under mu. Followers that observe
	// "starting" capture it and block on it for the leader's real outcome.
	startOp *startOp
}

// DefaultManager is the standard LifecycleManager implementation.
// It uses a ComposeOrchestrator to start/stop containers and a
// HealthChecker to verify readiness.
type DefaultManager struct {
	mu           sync.Mutex
	services     map[string]*serviceEntry
	orchestrator compose.ComposeOrchestrator
	checker      health.HealthChecker
}

// NewDefaultManager creates a DefaultManager backed by the given
// orchestrator and health checker.
func NewDefaultManager(
	orch compose.ComposeOrchestrator,
	hc health.HealthChecker,
) *DefaultManager {
	return &DefaultManager{
		services:     make(map[string]*serviceEntry),
		orchestrator: orch,
		checker:      hc,
	}
}

// Register adds a service specification. Returns an error if a
// service with the same name is already registered.
func (m *DefaultManager) Register(spec ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if spec.Name == "" {
		return fmt.Errorf("lifecycle: service name is required")
	}
	if _, exists := m.services[spec.Name]; exists {
		return fmt.Errorf(
			"lifecycle: service %q already registered", spec.Name,
		)
	}

	entry := &serviceEntry{
		spec:  spec,
		state: "stopped",
	}

	if spec.MaxConcurrent > 0 {
		entry.semaphore = NewConcurrencySemaphore(
			spec.MaxConcurrent,
		)
	}

	m.services[spec.Name] = entry
	return nil
}

// Start starts the named service via compose and waits for health.
func (m *DefaultManager) Start(
	ctx context.Context,
	name string,
) (retErr error) {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	// TOCTOU guard (constitution/Constitution.md §11.4.108 sibling-
	// primitive audit of the idle.go stale-fire race): the "not yet
	// running" check and the "starting" transition must happen inside
	// the SAME lock hold. Previously the lock was released between the
	// two, so two concurrent Start() calls that both observed the
	// service as not-running could both proceed past this point and
	// both execute the full boot sequence (double compose-up, double
	// health check, plus a second IdleShutdown controller that silently
	// orphans the first — its timer never Stop()'d, ticking in the
	// background and able to fire an unexpected Stop() later).
	//
	// Coalescing (LIFE-2): a caller that observes "running" returns nil
	// immediately (it genuinely is up). A caller that observes an in-flight
	// boot ("starting") becomes a FOLLOWER: it captures the leader's startOp,
	// blocks on op.done, and returns op.err — the leader's REAL boot outcome.
	// Previously a follower returned nil the instant it saw "starting",
	// fabricating success (§11.4.1) whenever the leader's boot then FAILED
	// (compose Up error / unhealthy → state back to "stopped").
	switch entry.state {
	case "running":
		m.mu.Unlock()
		return nil
	case "starting":
		op := entry.startOp
		m.mu.Unlock()
		if startFollowerWaitHook != nil {
			startFollowerWaitHook()
		}
		if op != nil {
			<-op.done
			return op.err
		}
		return nil
	}
	entry.state = "starting"

	// Leader: publish a fresh coalescing handle and, on EVERY exit path,
	// record this boot's outcome into it and wake all followers exactly once.
	// op is leader-private until close(op.done), so op.err needs no extra
	// lock — the close→receive edge synchronizes it to every follower.
	op := &startOp{done: make(chan struct{})}
	entry.startOp = op
	defer func() {
		// LIFE-2 leader-panic fix (constitution/Constitution.md §11.4.108
		// runtime-signature audit — a "fabricated success + wedged state" hazard
		// on the coalescing path). A panic in orchestrator.Up or checker.Check
		// (both external-interface impls) bypasses the normal return that would
		// have set the named retErr, so retErr is still nil during unwind. The
		// plain defer would then publish op.err = nil and close(op.done) — every
		// follower blocked on op.done wakes and returns nil, a FABRICATED SUCCESS
		// (§11.4.1) for a boot that never happened. WORSE: the panic also skips
		// every error-path `entry.state = "stopped"` reset, so state stays
		// "starting" FOREVER — each later Start takes the "starting" case, reads
		// the closed op, and also returns nil, permanently wedged (observable
		// when the panic is recovered upstream, e.g. the lazy path's
		// LazyBooter.EnsureStarted recover in lazy.go keeps the leader goroutine
		// alive). So on recover: (1) reset entry.state to "stopped" so a
		// subsequent Start genuinely re-attempts; (2) hand the followers a REAL
		// non-nil error describing the panicked boot; then (3) re-panic to
		// PRESERVE the leader's own crash/propagation semantics — the lazy path's
		// EnsureStarted recover converts it to an error for lazy callers, and a
		// direct Start caller keeps the pre-existing panic propagation (a plain
		// error-return here would silently swallow a real crash the leader's
		// caller previously saw). The lock is not held here (released before the
		// boot sequence), so m.mu.Lock is deadlock-free.
		if r := recover(); r != nil {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			retErr = fmt.Errorf(
				"lifecycle: start %q panicked: %v", name, r,
			)
			op.err = retErr
			close(op.done)
			panic(r)
		}
		op.err = retErr
		close(op.done)
	}()

	// Start dependencies first.
	deps := entry.spec.Dependencies
	m.mu.Unlock()

	for _, dep := range deps {
		if err := m.Start(ctx, dep); err != nil {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: dependency %q for %q: %w",
				dep, name, err,
			)
		}
	}

	// Start via compose.
	if m.orchestrator != nil && entry.spec.ComposeFile != "" {
		project := compose.ComposeProject{
			File:    entry.spec.ComposeFile,
			Profile: entry.spec.Profile,
		}
		if err := m.orchestrator.Up(ctx, project); err != nil {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: start %q: %w", name, err,
			)
		}
	}

	// Health check.
	if m.checker != nil {
		result := m.checker.Check(ctx, entry.spec.HealthTarget)
		m.mu.Lock()
		entry.healthy = result.Healthy
		m.mu.Unlock()
		// An unhealthy result must fail the start regardless of whether it
		// carries an error message. Gating on `result.Error != ""` too let an
		// unhealthy result with an empty message slip through and mark the
		// service "running" with a nil return — a fabricated success
		// (launch != working, §11.4.108-class bluff).
		if !result.Healthy {
			m.mu.Lock()
			entry.state = "stopped"
			m.mu.Unlock()
			msg := result.Error
			if msg == "" {
				msg = "unhealthy (no error detail)"
			}
			return fmt.Errorf(
				"lifecycle: health check %q: %s",
				name, msg,
			)
		}
	}

	// LIFE2-1 (constitution/Constitution.md §11.4.108 dead-service-reported-
	// running audit — a Stop-vs-in-flight-boot resurrection the first LIFE pass
	// missed): the boot sequence above (dependency recursion, orchestrator.Up,
	// health check) ran with m.mu RELEASED, so a concurrent Stop() could have
	// transitioned this service out of the "starting" boot THIS leader owns —
	// setting state to "stopping"/"stopped" AND running compose Down() to tear
	// the containers down. Committing "running" unconditionally here would then
	// overwrite that "stopped" back to "running", reporting a service whose
	// containers were already torn down as running (§11.4.108) and — on the
	// coalescing path — publishing op.err=nil so every blocked follower wakes
	// with a fabricated success (§11.4.1). Only commit "running" if this leader
	// STILL owns the in-flight boot (state=="starting" AND startOp==op). If a
	// concurrent Stop cancelled our own boot, honor the stop (settle to
	// "stopped", drop stale health) and fail this start; if a newer boot
	// superseded ours, do not touch the shared state the new leader owns.
	m.mu.Lock()
	if entry.startOp != op || entry.state != "starting" {
		if entry.startOp == op {
			entry.state = "stopped"
			entry.healthy = false
		}
		m.mu.Unlock()
		return fmt.Errorf(
			"lifecycle: start %q cancelled by concurrent stop", name,
		)
	}
	entry.state = "running"
	entry.lastStart = time.Now()
	m.mu.Unlock()

	// Start idle shutdown monitor if configured. Capture the freshly
	// created controller into a local variable and call Start() on
	// THAT local, rather than re-reading entry.idleCtrl after
	// unlocking — the same unguarded-field-read hazard the fix above
	// closes for Stop()/Acquire() applies here too (constitution/
	// Constitution.md §11.4.108 sibling-primitive audit).
	if entry.spec.IdleTimeout > 0 {
		m.mu.Lock()
		idleCtrl := NewIdleShutdown(
			entry.spec.IdleTimeout,
			func() {
				_ = m.Stop(context.Background(), name)
			},
		)
		entry.idleCtrl = idleCtrl
		m.mu.Unlock()
		// Do not fire idle shutdown while a lease is held (LIFE-(a)
		// IDLE-vs-LEASE fix). The predicate is a lock-free atomic load, safe
		// to call from fire() under is.mu. Set before Start() arms the timer.
		idleCtrl.setBusy(func() bool { return entry.activeLeases.Load() > 0 })
		idleCtrl.Start()
	}

	return nil
}

// Stop stops the named service via compose.
func (m *DefaultManager) Stop(
	ctx context.Context,
	name string,
) error {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	if entry.state == "stopped" {
		m.mu.Unlock()
		return nil
	}

	entry.state = "stopping"
	// entry.idleCtrl is written under m.mu by Start() (see above); read
	// it under the same lock here rather than after unlocking, closing
	// a data race between this read and a concurrent Start()'s write
	// (constitution/Constitution.md §11.4.108 sibling-primitive audit
	// of the idle.go stale-fire race — go test -race caught the
	// unguarded access).
	idleCtrl := entry.idleCtrl
	m.mu.Unlock()

	// Stop idle controller.
	if idleCtrl != nil {
		idleCtrl.Stop()
	}

	// Stop via compose.
	if m.orchestrator != nil && entry.spec.ComposeFile != "" {
		project := compose.ComposeProject{
			File:    entry.spec.ComposeFile,
			Profile: entry.spec.Profile,
		}
		if err := m.orchestrator.Down(ctx, project); err != nil {
			m.mu.Lock()
			entry.state = "running"
			m.mu.Unlock()
			return fmt.Errorf(
				"lifecycle: stop %q: %w", name, err,
			)
		}
	}

	m.mu.Lock()
	entry.state = "stopped"
	entry.healthy = false
	entry.lastStop = time.Now()
	// Reset the one-shot LazyBooter so a lazy service that was idle-stopped (or
	// stopped manually) is REVIVED on the next Acquire. Without this, the already-
	// fired sync.Once inside the LazyBooter returns its cached nil forever, so
	// Start is never re-invoked and Acquire hands back a live-looking lease on a
	// dead container (§11.4.108). A concurrent lazy Acquire copies its LazyBooter
	// pointer under m.mu before calling EnsureStarted, so nil-ing it here only
	// affects the NEXT Acquire.
	entry.lazyBooter = nil
	m.mu.Unlock()

	return nil
}

// Acquire obtains a lease on the named service. If the service is
// configured for lazy boot it will be started on first Acquire.
func (m *DefaultManager) Acquire(
	ctx context.Context,
	name string,
) (ReleaseFunc, error) {
	m.mu.Lock()
	entry, ok := m.services[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}
	m.mu.Unlock()

	// Lazy boot: start on first acquire if not running.
	if entry.spec.LazyBoot {
		m.mu.Lock()
		if entry.lazyBooter == nil {
			entry.lazyBooter = NewLazyBooter(func() error {
				return m.Start(ctx, name)
			})
		}
		lb := entry.lazyBooter
		m.mu.Unlock()

		if err := lb.EnsureStarted(); err != nil {
			// LIFE-4: a failed first boot leaves lb with a fired sync.Once +
			// cached err, so every LATER Acquire would otherwise return the
			// SAME stale error forever (Start never re-invoked) — a lazy
			// service poisoned for life, since Stop's booter-reset is
			// unreachable from the post-failed-boot "stopped" state (Stop
			// early-returns on "stopped"). Clear the poisoned booter (only if
			// it is still the one we used — a concurrent successful Acquire may
			// already have installed a fresh one) so the NEXT Acquire boots
			// again genuinely.
			m.mu.Lock()
			if entry.lazyBooter == lb {
				entry.lazyBooter = nil
			}
			m.mu.Unlock()
			return nil, fmt.Errorf(
				"lifecycle: lazy boot %q: %w", name, err,
			)
		}
	}

	// LIFE-1: assert the service is running AND count this lease inside the
	// SAME m.mu hold. Previously the non-lazy state gate was checked under the
	// lock, the lock released, and activeLeases incremented as a BARE atomic
	// afterwards — so a concurrent Stop (manual or idle-fired) could observe
	// busy()==0, tear the service down, and this Acquire would then hand back a
	// live-looking lease on a stopped service (§11.4.108 dead-lease). Counting
	// the lease while state=="running" closes the check-vs-count gap; the lazy
	// path re-checks here too (a Stop may have interleaved between
	// EnsureStarted and now).
	m.mu.Lock()
	if entry.state != "running" {
		m.mu.Unlock()
		return nil, fmt.Errorf(
			"lifecycle: service %q is not running", name,
		)
	}
	entry.activeLeases.Add(1)
	m.mu.Unlock()

	// Acquire semaphore slot.
	if entry.semaphore != nil {
		if err := entry.semaphore.Acquire(ctx); err != nil {
			// Roll back the lease counted above: it never materialized.
			entry.activeLeases.Add(-1)
			return nil, fmt.Errorf(
				"lifecycle: semaphore %q: %w", name, err,
			)
		}
	}

	// LIFE-1 race window: semaphore.Acquire can block, and Stop does NOT drain
	// the semaphore channel, so a concurrent Stop may have torn the service
	// down while we waited here. Re-validate state under m.mu and, on mismatch,
	// roll back BOTH the semaphore slot and the counted lease rather than hand
	// back a dead lease (§11.4.108). acquireRaceHook is a nil no-op in
	// production; tests drive a deterministic Stop into exactly this window.
	if acquireRaceHook != nil {
		acquireRaceHook()
	}
	m.mu.Lock()
	stillRunning := entry.state == "running"
	m.mu.Unlock()
	if !stillRunning {
		if entry.semaphore != nil {
			entry.semaphore.Release()
		}
		entry.activeLeases.Add(-1)
		return nil, fmt.Errorf(
			"lifecycle: service %q stopped during acquire", name,
		)
	}

	// Reset idle timer. entry.idleCtrl is written under m.mu by Start();
	// read it under the same lock rather than unguarded, closing a data
	// race with a concurrent Start() (constitution/Constitution.md
	// §11.4.108 sibling-primitive audit; go test -race caught the
	// unguarded access).
	m.mu.Lock()
	idleCtrl := entry.idleCtrl
	entry.lastAcq = time.Now()
	m.mu.Unlock()

	if idleCtrl != nil {
		idleCtrl.Touch()
	}

	// releaseOnce makes the returned ReleaseFunc idempotent AND
	// concurrency-safe (LIFE-(b) RELEASED-RACE fix): the previous plain
	// non-atomic `released bool` guard data-raced when the same ReleaseFunc
	// was invoked concurrently, double-releasing the semaphore. sync.Once
	// mirrors the tunnelEntry.waitOnce pattern in pkg/network/tunnel.go.
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			if entry.semaphore != nil {
				entry.semaphore.Release()
			}
			// Pair the Add(1) in Acquire — exactly one decrement per release.
			entry.activeLeases.Add(-1)
			m.mu.Lock()
			relIdleCtrl := entry.idleCtrl
			m.mu.Unlock()
			if relIdleCtrl != nil {
				relIdleCtrl.Touch()
			}
		})
	}, nil
}

// Status returns the current lifecycle status of the named service.
func (m *DefaultManager) Status(
	name string,
) (ServiceLifecycleStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.services[name]
	if !ok {
		return ServiceLifecycleStatus{}, fmt.Errorf(
			"lifecycle: service %q not found", name,
		)
	}

	activeUsers := 0
	if entry.semaphore != nil {
		activeUsers = entry.semaphore.ActiveCount()
	}

	return ServiceLifecycleStatus{
		Name:         name,
		State:        entry.state,
		Healthy:      entry.healthy,
		ActiveUsers:  activeUsers,
		LastStarted:  entry.lastStart,
		LastStopped:  entry.lastStop,
		LastAcquired: entry.lastAcq,
	}, nil
}

// Shutdown gracefully stops all managed services. LIFE-3 doc-honesty: the
// services are stopped in UNSPECIFIED order (Go map iteration), NOT priority
// order — spec.Priority is declared but not yet wired to any ordering (see
// ServiceSpec.Priority). Start-time ordering IS honored for declared
// Dependencies via dependency recursion in Start; Shutdown performs no
// symmetric reverse-dependency (or priority) teardown ordering. The first
// Stop error encountered is returned, but every service is still attempted.
func (m *DefaultManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	names := make([]string, 0, len(m.services))
	for name := range m.services {
		names = append(names, name)
	}
	m.mu.Unlock()

	var firstErr error
	for _, name := range names {
		if err := m.Stop(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
