package boot

import (
	"context"
	"testing"

	"digital.vasic.containers/pkg/remote"
	"digital.vasic.containers/pkg/scheduler"
	"github.com/stretchr/testify/assert"
)

// mockDistributor implements the boot.Distributor interface.
type mockDistributor struct{}

func (m *mockDistributor) DistributeEndpoints(ctx context.Context, names []string) (int, error) {
	return 0, nil
}

// Undistribute satisfies the extended boot.Distributor interface (BOOT-3).
func (m *mockDistributor) Undistribute(_ context.Context) error { return nil }

// mockHostManagerForBoot implements remote.HostManager with all required methods.
type mockHostManagerForBoot struct{}

func (m *mockHostManagerForBoot) AddHost(host remote.RemoteHost) error            { return nil }
func (m *mockHostManagerForBoot) RemoveHost(name string) error                    { return nil }
func (m *mockHostManagerForBoot) GetHost(name string) (*remote.RemoteHost, error) { return nil, nil }
func (m *mockHostManagerForBoot) ListHosts() []remote.RemoteHost                  { return nil }
func (m *mockHostManagerForBoot) ProbeHost(ctx context.Context, name string) (*remote.HostResources, error) {
	return nil, nil
}
func (m *mockHostManagerForBoot) ProbeAll(ctx context.Context) map[string]*remote.HostResources {
	return nil
}
func (m *mockHostManagerForBoot) HostState(name string) remote.HostState { return "" }

// mockSchedulerForBoot implements scheduler.Scheduler.
type mockSchedulerForBoot struct{}

func (m *mockSchedulerForBoot) Schedule(ctx context.Context, req scheduler.ContainerRequirements) (*scheduler.PlacementDecision, error) {
	return nil, nil
}
func (m *mockSchedulerForBoot) ScheduleBatch(ctx context.Context, reqs []scheduler.ContainerRequirements) (*scheduler.PlacementPlan, error) {
	return nil, nil
}
func (m *mockSchedulerForBoot) Rebalance(ctx context.Context) (*scheduler.PlacementPlan, error) {
	return nil, nil
}

func TestWithDistributor(t *testing.T) {
	d := &mockDistributor{}
	bm := &BootManager{}
	opt := WithDistributor(d)
	opt(bm)
	assert.Equal(t, d, bm.distributor)
}

func TestWithHostManager(t *testing.T) {
	hm := &mockHostManagerForBoot{}
	bm := &BootManager{}
	opt := WithHostManager(hm)
	opt(bm)
	assert.Equal(t, hm, bm.hostManager)
}

func TestWithScheduler(t *testing.T) {
	s := &mockSchedulerForBoot{}
	bm := &BootManager{}
	opt := WithScheduler(s)
	opt(bm)
	assert.Equal(t, s, bm.scheduler)
}

func TestWithDistributor_Nil(t *testing.T) {
	bm := &BootManager{}
	opt := WithDistributor(nil)
	opt(bm)
	assert.Nil(t, bm.distributor)
}

func TestWithHostManager_Nil(t *testing.T) {
	bm := &BootManager{}
	opt := WithHostManager(nil)
	opt(bm)
	assert.Nil(t, bm.hostManager)
}

func TestWithScheduler_Nil(t *testing.T) {
	bm := &BootManager{}
	opt := WithScheduler(nil)
	opt(bm)
	assert.Nil(t, bm.scheduler)
}
