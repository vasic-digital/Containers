package ctop

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockExecutor struct {
	responses map[string][]byte
	errors    map[string]error
}

func (m *mockExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + args[0]
	if data, ok := m.responses[key]; ok {
		return data, nil
	}
	if err, ok := m.errors[key]; ok {
		return nil, err
	}
	return nil, nil
}

func TestNewCollectorWithExecutor(t *testing.T) {
	exec := &mockExecutor{}
	c := NewCollectorWithExecutor("docker", nil, exec)
	assert.NotNil(t, c)
	assert.Equal(t, "docker", c.runtime)
	assert.Equal(t, exec, c.executor)
}

func TestCollector_Collect_Local(t *testing.T) {
	psOutput := `[{"Id":"abc123def456ghi789","Names":["/test-container"],"Image":"nginx:latest","Created":1704067200,"State":"running","Status":"Up 5 days","StartedAt":1704067200,"Ports":[{"host_port":80,"protocol":"tcp"}]}]`
	statsOutput := `[{"cpu_percent":"1.5%","mem_usage":"50MB / 1GB","mem_percent":"5.0%","net_io":"1MB / 500kB","block_io":"10MB / 5MB","pids":"5"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}

	c := NewCollectorWithExecutor("podman", nil, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Equal(t, 1, list.Total)
	assert.Equal(t, 1, list.Running)
	assert.Equal(t, 0, list.Stopped)

	if len(list.Processes) > 0 {
		p := list.Processes[0]
		assert.Equal(t, "abc123def456", p.ID)
		assert.Equal(t, "test-container", p.Name)
		assert.Equal(t, "nginx:latest", p.Image)
		assert.Equal(t, "running", p.State)
		assert.Equal(t, "podman", p.Runtime)
		assert.Equal(t, "local", p.Location)
		assert.Equal(t, 1.5, p.CPUPercent)
		assert.Equal(t, 5.0, p.MemoryPercent)
		assert.Equal(t, 5, p.PIDs)
	}
}

func TestCollector_Collect_FallbackToDocker(t *testing.T) {
	psOutput := `{"Command":"redis-server","CreatedAt":"2024-01-01 00:00:00 +0000 UTC","ID":"xyz789abcd","Image":"redis:latest","Labels":"","Names":"docker-container","Ports":"","State":"running","Status":"Up"}`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"docker ps": []byte(psOutput),
		},
		errors: map[string]error{
			"podman ps": assert.AnError,
		},
	}

	c := NewCollectorWithExecutor("podman", nil, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Equal(t, 1, list.Total)
	assert.Equal(t, "docker-container", list.Processes[0].Name)
}

func TestContainerProcessList_Sort(t *testing.T) {
	list := &ContainerProcessList{
		Processes: []ContainerProcess{
			{Name: "c-container", CPUPercent: 30.0, MemoryUsage: 100},
			{Name: "a-container", CPUPercent: 50.0, MemoryUsage: 300},
			{Name: "b-container", CPUPercent: 10.0, MemoryUsage: 200},
		},
	}

	t.Run("SortByCPU_Desc", func(t *testing.T) {
		list.Sort(SortByCPU, SortDesc)
		assert.Equal(t, "a-container", list.Processes[0].Name)
		assert.Equal(t, "c-container", list.Processes[1].Name)
		assert.Equal(t, "b-container", list.Processes[2].Name)
	})

	t.Run("SortByCPU_Asc", func(t *testing.T) {
		list.Sort(SortByCPU, SortAsc)
		assert.Equal(t, "b-container", list.Processes[0].Name)
	})

	t.Run("SortByName_Asc", func(t *testing.T) {
		list.Sort(SortByName, SortAsc)
		assert.Equal(t, "c-container", list.Processes[0].Name)
	})

	t.Run("SortByName_Desc", func(t *testing.T) {
		list.Sort(SortByName, SortDesc)
		assert.Equal(t, "a-container", list.Processes[0].Name)
	})

	t.Run("SortByMemory_Desc", func(t *testing.T) {
		list.Sort(SortByMemory, SortDesc)
		assert.Equal(t, "a-container", list.Processes[0].Name)
		assert.Equal(t, 300, int(list.Processes[0].MemoryUsage))
	})
}

func TestContainerProcessList_Filter(t *testing.T) {
	list := &ContainerProcessList{
		Processes: []ContainerProcess{
			{Name: "web-server", Host: "local", State: "running"},
			{Name: "db-server", Host: "remote:thinker", State: "running"},
			{Name: "cache-server", Host: "local", State: "exited"},
			{Name: "api-server", Host: "remote:thinker", State: "running"},
		},
		Total: 4,
	}

	t.Run("FilterByHost", func(t *testing.T) {
		filteredList := &ContainerProcessList{
			Processes: append([]ContainerProcess{}, list.Processes...),
			Total:     4,
		}
		filteredList.Filter("thinker", "", true)
		assert.Equal(t, 2, filteredList.Total)
		for _, p := range filteredList.Processes {
			assert.Contains(t, p.Host, "thinker")
		}
	})

	t.Run("FilterByName", func(t *testing.T) {
		filteredList := &ContainerProcessList{
			Processes: append([]ContainerProcess{}, list.Processes...),
			Total:     4,
		}
		filteredList.Filter("", "server", true)
		assert.Equal(t, 4, filteredList.Total)
	})

	t.Run("HideStopped", func(t *testing.T) {
		filteredList := &ContainerProcessList{
			Processes: append([]ContainerProcess{}, list.Processes...),
			Total:     4,
		}
		filteredList.Filter("", "", false)
		assert.Equal(t, 3, filteredList.Total)
		for _, p := range filteredList.Processes {
			assert.Equal(t, "running", p.State)
		}
	})

	t.Run("CombinedFilters", func(t *testing.T) {
		filteredList := &ContainerProcessList{
			Processes: append([]ContainerProcess{}, list.Processes...),
			Total:     4,
		}
		filteredList.Filter("local", "web", false)
		assert.Equal(t, 1, filteredList.Total)
		assert.Equal(t, "web-server", filteredList.Processes[0].Name)
	})
}

func TestParseContainerList(t *testing.T) {
	t.Run("Array", func(t *testing.T) {
		data := []byte(`[
			{"Id":"abc123","Names":["/test1"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200},
			{"Id":"def456","Names":["/test2"],"Image":"redis","Created":1704067200,"State":"exited","Status":"Exited (0)"}
		]`)

		containers, err := parseContainerList(data, "podman", "local")
		require.NoError(t, err)
		assert.Len(t, containers, 2)
		assert.Equal(t, "abc123", containers[0].ID)
		assert.Equal(t, "test1", containers[0].Name)
		assert.Equal(t, "running", containers[0].State)
	})

	t.Run("SingleObject", func(t *testing.T) {
		data := []byte(`{"Id":"xyz789","Names":["/single"],"Image":"alpine","Created":1704067200,"State":"running","Status":"Up"}`)

		containers, err := parseContainerList(data, "docker", "local")
		require.NoError(t, err)
		assert.Len(t, containers, 1)
		assert.Equal(t, "xyz789", containers[0].ID)
	})

	t.Run("WithRemoteLocation", func(t *testing.T) {
		data := []byte(`[{"Id":"remote1","Names":["/remote-container"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up"}]`)

		containers, err := parseContainerList(data, "podman", "remote:thinker")
		require.NoError(t, err)
		assert.Len(t, containers, 1)
		assert.Equal(t, "remote:thinker", containers[0].Location)
		assert.Equal(t, "thinker", containers[0].Host)
	})
}

func TestParseContainerStats(t *testing.T) {
	data := []byte(`{"CPUPerc":"25.5%","MemUsage":"256MiB / 2GiB","MemPerc":"12.5%","NetIO":"10MiB / 5MiB","BlockIO":"100MiB / 50MiB","PIDs":"10"}`)

	stats := parseContainerStats(data)
	require.NotNil(t, stats)
	assert.Equal(t, 25.5, stats.CPUPercent)
	assert.Equal(t, 12.5, stats.MemoryPercent)
	assert.Equal(t, 10, stats.PIDs)
}

func TestHelperFunctions(t *testing.T) {
	t.Run("shortenID", func(t *testing.T) {
		assert.Equal(t, "abc123def456", shortenID("abc123def456"))
		assert.Equal(t, "abc123def456", shortenID("abc123def456789"))
		assert.Equal(t, "short", shortenID("short"))
	})

	t.Run("extractName", func(t *testing.T) {
		assert.Equal(t, "container-name", extractName([]string{"/container-name"}))
		assert.Equal(t, "name", extractName([]string{"name"}))
		assert.Equal(t, "", extractName([]string{}))
	})

	t.Run("parsePercent", func(t *testing.T) {
		assert.Equal(t, 25.5, parsePercent("25.5%"))
		assert.Equal(t, 100.0, parsePercent("100%"))
		assert.Equal(t, 0.0, parsePercent(""))
	})

	t.Run("parseSize", func(t *testing.T) {
		assert.Equal(t, uint64(1024), parseSize("1KiB"))
		assert.Equal(t, uint64(1024*1024), parseSize("1MiB"))
		assert.Equal(t, uint64(1024*1024*1024), parseSize("1GiB"))
		assert.Equal(t, uint64(500), parseSize("500B"))
	})

	t.Run("formatUptime", func(t *testing.T) {
		assert.Equal(t, "5m", formatUptime(5*time.Minute))
		assert.Equal(t, "1h30m", formatUptime(90*time.Minute))
		assert.Equal(t, "2d5h30m", formatUptime(53*time.Hour+30*time.Minute))
	})
}

func TestDefaultDisplayConfig(t *testing.T) {
	config := DefaultDisplayConfig()
	assert.Equal(t, SortByCPU, config.SortBy)
	assert.Equal(t, SortDesc, config.SortOrder)
	assert.False(t, config.ShowStopped)
	assert.Equal(t, 1000, config.RefreshRate)
	assert.False(t, config.NoColor)
	assert.False(t, config.Compact)
}

func TestDisplay_RenderSnapshot(t *testing.T) {
	psOutput := `[{"Id":"snap123","Names":["/snapshot-test"],"Image":"test:latest","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"10.0%","mem_usage":"100MB / 1GB","mem_percent":"10.0%","net_io":"1MB / 1MB","block_io":"10MB / 10MB","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}

	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	display := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	snapshot, err := display.RenderSnapshot(context.Background())
	require.NoError(t, err)
	assert.Contains(t, snapshot, "ctop")
	assert.Contains(t, snapshot, "snapshot-test")
}

func TestDisplay_RenderJSON(t *testing.T) {
	psOutput := `[{"Id":"json123","Names":["/json-test"],"Image":"test:latest","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"5.0%","mem_usage":"50MB / 1GB","mem_percent":"5.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}

	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	display := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	jsonOutput, err := display.RenderJSON(context.Background())
	require.NoError(t, err)
	assert.Contains(t, jsonOutput, `"total": 1`)
	assert.Contains(t, jsonOutput, `"running": 1`)
	assert.Contains(t, jsonOutput, "json-test")
}

func TestCollector_GetStats(t *testing.T) {
	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps": []byte(`[]`),
		},
	}

	c := NewCollectorWithExecutor("podman", nil, exec)

	_, err := c.Collect(context.Background())
	require.NoError(t, err)

	stats := c.GetStats()
	assert.Equal(t, 0, stats.TotalContainers)
	assert.Equal(t, 0, stats.LocalContainers)
	assert.Equal(t, 0, stats.RemoteContainers)
	assert.GreaterOrEqual(t, stats.HostCount, 0)
}
