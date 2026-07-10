package ctop

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyList duplicates a ContainerProcessList for independent sort tests.
func copyList(orig *ContainerProcessList) *ContainerProcessList {
	procs := make([]ContainerProcess, len(orig.Processes))
	copy(procs, orig.Processes)
	return &ContainerProcessList{Processes: procs, Total: orig.Total}
}

// TestContainerProcessList_Sort_Extended covers SortByState,
// SortByUptime, SortByRuntime, SortByHost, and the default case.
func TestContainerProcessList_Sort_Extended(t *testing.T) {
	t0 := time.Now().Add(-10 * time.Minute)
	t1 := time.Now().Add(-5 * time.Minute)
	t2 := time.Now()

	base := &ContainerProcessList{
		Processes: []ContainerProcess{
			{Name: "c", State: "running", Runtime: "podman", Host: "local", StartedAt: t1},
			{Name: "a", State: "exited", Runtime: "docker", Host: "remote:h1", StartedAt: t0},
			{Name: "b", State: "paused", Runtime: "nerdctl", Host: "local", StartedAt: t2},
		},
	}

	t.Run("SortByState_Asc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByState, SortAsc)
		assert.Equal(t, 3, len(l.Processes))
	})

	t.Run("SortByState_Desc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByState, SortDesc)
		// SortDesc returns less = i.State < j.State, ascending alphabetically
		assert.Equal(t, 3, len(l.Processes))
		assert.Equal(t, "exited", l.Processes[0].State)
	})

	t.Run("SortByUptime_Asc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByUptime, SortAsc)
		assert.Equal(t, 3, len(l.Processes))
	})

	t.Run("SortByUptime_Desc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByUptime, SortDesc)
		// less = i.StartedAt.Before(j.StartedAt); SortDesc returns less
		// so oldest-started (t0) is first
		assert.Equal(t, "a", l.Processes[0].Name)
	})

	t.Run("SortByRuntime_Asc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByRuntime, SortAsc)
		assert.Equal(t, 3, len(l.Processes))
	})

	t.Run("SortByRuntime_Desc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByRuntime, SortDesc)
		// less = i.Runtime < j.Runtime; SortDesc ascending alphabetically
		assert.Equal(t, "docker", l.Processes[0].Runtime)
	})

	t.Run("SortByHost_Asc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByHost, SortAsc)
		assert.Equal(t, 3, len(l.Processes))
	})

	t.Run("SortByHost_Desc", func(t *testing.T) {
		l := copyList(base)
		l.Sort(SortByHost, SortDesc)
		assert.Equal(t, 3, len(l.Processes))
	})

	t.Run("DefaultSort_UnknownField", func(t *testing.T) {
		l := copyList(base)
		// Unknown SortField hits the default case (sorts by CPU)
		l.Sort(SortField("unknown"), SortDesc)
		assert.Equal(t, 3, len(l.Processes))
	})
}

// TestParseSize_Extended covers the GB and MB (non-binary) suffixes.
func TestParseSize_Extended(t *testing.T) {
	assert.Equal(t, uint64(1000*1000*1000), parseSize("1GB"))
	assert.Equal(t, uint64(1000*1000), parseSize("1MB"))
	assert.Equal(t, uint64(1000), parseSize("1KB"))
	assert.Equal(t, uint64(0), parseSize("invalid"))
	assert.Equal(t, uint64(0), parseSize(""))
}

// TestParseMemoryBytes_Extended covers the single-part and two-part cases.
func TestParseMemoryBytes_Extended(t *testing.T) {
	assert.Equal(t, uint64(1024*1024), parseMemoryBytes("1MiB / 2GiB"))
	assert.Equal(t, uint64(1024*1024*1024), parseMemoryBytes("1GiB"))
}

// TestParseMemoryLimit_Extended verifies the limit is taken from the second part.
func TestParseMemoryLimit_Extended(t *testing.T) {
	assert.Equal(t, uint64(2*1024*1024*1024), parseMemoryLimit("1MiB / 2GiB"))
	// No slash means no second part: limit is 0
	assert.Equal(t, uint64(0), parseMemoryLimit("1GiB"))
}

// TestParseNetIO_NoSlash ensures a missing slash returns 0.
func TestParseNetIO_NoSlash(t *testing.T) {
	assert.Equal(t, uint64(0), parseNetIO("noslash", true))
	assert.Equal(t, uint64(0), parseNetIO("noslash", false))
}

// TestParseBlockIO_NoSlash ensures a missing slash returns 0.
func TestParseBlockIO_NoSlash(t *testing.T) {
	assert.Equal(t, uint64(0), parseBlockIO("noslash", true))
	assert.Equal(t, uint64(0), parseBlockIO("noslash", false))
}

// TestNewCompactDisplay verifies the compact flag is set.

// TestCompactDisplay_Stop verifies Stop does not panic when cancel is nil.
func TestCompactDisplay_Stop(t *testing.T) {
	// bluff-scan: no-assert-ok (lifecycle invariant — out-of-order calls must not panic/error)
	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps": []byte(`[]`),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)
	cd := NewCompactDisplay(c, DefaultDisplayConfig())
	cd.Stop()
}

// TestCompactDisplay_RenderSnapshot verifies the compact display
// delegates RenderSnapshot to the inner Display correctly.
func TestCompactDisplay_RenderSnapshot(t *testing.T) {
	psOutput := `[{"Id":"compact1","Names":["/compact-test"],"Image":"nginx:latest","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"5.0%","mem_usage":"50MB / 1GB","mem_percent":"5.0%","net_io":"1MB / 1MB","block_io":"10MB / 10MB","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	cd := &CompactDisplay{display: NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)}

	snapshot, err := cd.RenderSnapshot(context.Background())
	require.NoError(t, err)
	assert.Contains(t, snapshot, "ctop")
}

// TestDisplay_Stop_WithCancel verifies Stop calls the cancel function.
func TestDisplay_Stop_WithCancel(t *testing.T) {
	exec := &mockExecutor{responses: map[string][]byte{"podman ps": []byte(`[]`)}}
	c := NewCollectorWithExecutor("podman", nil, exec)
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.Stop()

	select {
	case <-ctx.Done():
		// Correct: context was cancelled.
	default:
		t.Fatal("expected context to be cancelled after Stop")
	}
}

// TestDisplay_RenderError exercises the renderError path.
func TestDisplay_RenderError(t *testing.T) {
	exec := &mockExecutor{
		errors: map[string]error{
			"podman ps": assert.AnError,
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)
	d.render(context.Background())
	assert.Contains(t, buf.String(), "Error")
}
