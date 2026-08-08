package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/lifecycle"
)

// TestWave17_Lifecycle_LazyBoot_RebootsAfterIdleShutdown proves a lazy service
// with an idle timeout is REVIVED on the next Acquire after an idle shutdown,
// instead of handing back a live-looking lease on a dead (stopped) container.
//
// The idle timer calls Stop, which must reset the one-shot LazyBooter so the
// next Acquire re-runs Start. RED on the pre-fix source: Stop leaves the
// already-fired LazyBooter in place, so the lazy Acquire branch calls
// EnsureStarted() → returns the booter's cached nil → Start is NEVER
// re-invoked; the service stays "stopped" and upCalled stays 1, yet Acquire
// still returns a non-nil ReleaseFunc + nil error — a §11.4.108 dead-lease
// bluff (launch reported success on a service that is not running).
func TestWave17_Lifecycle_LazyBoot_RebootsAfterIdleShutdown(t *testing.T) {
	orch := &stubOrchestrator{}
	hc := &stubHealthChecker{healthy: true}
	m := lifecycle.NewDefaultManager(orch, hc)

	require.NoError(t, m.Register(lifecycle.ServiceSpec{
		Name:        "lazy",
		LazyBoot:    true,
		ComposeFile: "c.yml",
		IdleTimeout: 40 * time.Millisecond,
	}))

	ctx := context.Background()

	// First Acquire lazy-boots the service; release it so the idle timer elapses.
	rel, err := m.Acquire(ctx, "lazy")
	require.NoError(t, err)
	rel()
	require.Equal(t, 1, orch.upCalled, "first Acquire must lazy-boot exactly once")

	// Idle shutdown must fire after the release.
	require.Eventually(t, func() bool {
		s, _ := m.Status("lazy")
		return s.State == "stopped"
	}, 2*time.Second, 5*time.Millisecond,
		"idle timeout must stop the released lazy service")

	// The next Acquire must REVIVE the service, not lease a dead container.
	rel2, err := m.Acquire(ctx, "lazy")
	require.NoError(t, err)

	s, _ := m.Status("lazy")
	assert.Equal(t, "running", s.State,
		"Acquire after idle shutdown must reboot the lazy service, not lease a stopped one")
	assert.Equal(t, 2, orch.upCalled,
		"Acquire after idle shutdown must re-invoke Start (second boot)")

	rel2()
	_ = m.Shutdown(ctx) // deterministic teardown: stops the idle goroutine
}
