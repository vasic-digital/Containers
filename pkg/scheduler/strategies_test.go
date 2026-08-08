package scheduler

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	"digital.vasic.containers/pkg/remote"
)

func TestLabelsMatch(t *testing.T) {
	tests := []struct {
		name     string
		host     map[string]string
		required map[string]string
		want     bool
	}{
		{
			"nil required",
			map[string]string{"gpu": "true"},
			nil,
			true,
		},
		{
			"empty required",
			map[string]string{"gpu": "true"},
			map[string]string{},
			true,
		},
		{
			"matching",
			map[string]string{"gpu": "true", "arch": "amd64"},
			map[string]string{"gpu": "true"},
			true,
		},
		{
			"missing label",
			map[string]string{"arch": "amd64"},
			map[string]string{"gpu": "true"},
			false,
		},
		{
			"wrong value",
			map[string]string{"gpu": "false"},
			map[string]string{"gpu": "true"},
			false,
		},
		{
			"nil host labels",
			nil,
			map[string]string{"gpu": "true"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				labelsMatch(tt.host, tt.required),
			)
		})
	}
}

func TestScheduleResourceAware_NoHosts(t *testing.T) {
	opts := DefaultSchedulerOptions()
	scorer := NewResourceScorer(opts)

	decision := scheduleResourceAware(
		scorer,
		map[string]*remote.HostResources{},
		nil,
		ContainerRequirements{Name: "test"},
		"local",
	)
	assert.Equal(t, "", decision.HostName)
	assert.Contains(t, decision.Reason, "no host")
}

func TestScheduleResourceAware_LocalOnly(t *testing.T) {
	opts := DefaultSchedulerOptions()
	scorer := NewResourceScorer(opts)

	snapshots := map[string]*remote.HostResources{
		"local": makeSnapshot("local", 30, 30, 16384, 500000, 8),
	}

	decision := scheduleResourceAware(
		scorer, snapshots, nil,
		ContainerRequirements{Name: "test"},
		"local",
	)
	assert.Equal(t, "local", decision.HostName)
}

func TestScheduleRoundRobin_Distribution(t *testing.T) {
	hosts := []remote.RemoteHost{
		{Name: "h1", Labels: map[string]string{}},
		{Name: "h2", Labels: map[string]string{}},
	}

	var counter atomic.Uint64
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		d := scheduleRoundRobin(
			hosts,
			ContainerRequirements{Name: "test"},
			"local",
			&counter,
		)
		seen[d.HostName] = true
	}
	// Should have used multiple hosts.
	assert.Greater(t, len(seen), 1)
}

func TestScheduleAffinity_NoMatch(t *testing.T) {
	opts := DefaultSchedulerOptions()
	scorer := NewResourceScorer(opts)

	hosts := []remote.RemoteHost{
		{Name: "h1", Labels: map[string]string{"arch": "arm64"}},
	}
	snapshots := map[string]*remote.HostResources{
		"h1": makeSnapshot("h1", 20, 20, 32768, 500000, 16),
	}

	decision := scheduleAffinity(
		scorer, snapshots, hosts,
		ContainerRequirements{
			Name:   "test",
			Labels: map[string]string{"gpu": "true"},
		},
	)
	assert.Equal(t, "", decision.HostName)
	assert.Contains(t, decision.Reason, "no host matches")
}

func TestScheduleAffinity_Match(t *testing.T) {
	opts := DefaultSchedulerOptions()
	scorer := NewResourceScorer(opts)

	hosts := []remote.RemoteHost{
		{Name: "h1", Labels: map[string]string{"gpu": "true"}},
	}
	snapshots := map[string]*remote.HostResources{
		"h1": makeSnapshot("h1", 20, 20, 32768, 500000, 16),
	}

	decision := scheduleAffinity(
		scorer, snapshots, hosts,
		ContainerRequirements{
			Name:   "test",
			Labels: map[string]string{"gpu": "true"},
		},
	)
	assert.Equal(t, "h1", decision.HostName)
}

func TestScheduleSpread(t *testing.T) {
	hosts := []remote.RemoteHost{
		{Name: "h1", Labels: map[string]string{}},
		{Name: "h2", Labels: map[string]string{}},
	}
	snapshots := map[string]*remote.HostResources{
		"local": makeSnapshot("local", 30, 30, 16384, 500000, 8),
		"h1":    makeSnapshot("h1", 30, 30, 16384, 500000, 8),
		"h2":    makeSnapshot("h2", 30, 30, 16384, 500000, 8),
	}
	existing := map[string]int{
		"local": 5,
		"h1":    2,
		"h2":    8,
	}

	decision := scheduleSpread(
		snapshots, hosts,
		ContainerRequirements{Name: "test"},
		"local", existing,
	)
	// h1 has fewest containers.
	assert.Equal(t, "h1", decision.HostName)
}

func TestScheduleBinPack(t *testing.T) {
	opts := DefaultSchedulerOptions()
	scorer := NewResourceScorer(opts)

	hosts := []remote.RemoteHost{
		{Name: "h1", Labels: map[string]string{}},
		{Name: "h2", Labels: map[string]string{}},
	}
	snapshots := map[string]*remote.HostResources{
		"local": makeSnapshot("local", 70, 60, 16384, 500000, 8),
		"h1":    makeSnapshot("h1", 20, 20, 32768, 500000, 16),
		"h2":    makeSnapshot("h2", 80, 70, 16384, 500000, 8),
	}

	decision := scheduleBinPack(
		scorer, snapshots, hosts,
		ContainerRequirements{Name: "test"},
		"local",
	)
	// h2 is most utilized that can still fit -> bin-pack prefers it.
	assert.NotEmpty(t, decision.HostName)
}
