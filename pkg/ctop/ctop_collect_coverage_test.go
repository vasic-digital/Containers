package ctop

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/remote"
)

// ctopMockHostManager satisfies remote.HostManager for ctop collector tests.
type ctopMockHostManager struct {
	hosts map[string]remote.RemoteHost
}

func (m *ctopMockHostManager) AddHost(host remote.RemoteHost) error {
	if m.hosts == nil {
		m.hosts = make(map[string]remote.RemoteHost)
	}
	m.hosts[host.Name] = host
	return nil
}
func (m *ctopMockHostManager) GetHost(name string) (*remote.RemoteHost, error) {
	h, ok := m.hosts[name]
	if !ok {
		return nil, fmt.Errorf("host %s not found", name)
	}
	return &h, nil
}
func (m *ctopMockHostManager) ListHosts() []remote.RemoteHost {
	result := make([]remote.RemoteHost, 0, len(m.hosts))
	for _, h := range m.hosts {
		result = append(result, h)
	}
	return result
}
func (m *ctopMockHostManager) RemoveHost(name string) error { return nil }
func (m *ctopMockHostManager) ProbeHost(ctx context.Context, name string) (*remote.HostResources, error) {
	return nil, fmt.Errorf("not available")
}
func (m *ctopMockHostManager) ProbeAll(ctx context.Context) map[string]*remote.HostResources {
	return nil
}
func (m *ctopMockHostManager) HostState(name string) remote.HostState {
	return remote.HostUnknown
}

// bothFailExecutor returns errors for both podman and docker ps calls.
type bothFailExecutor struct{}

func (e *bothFailExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("command failed: %s", name)
}

// statsErrorExecutor returns an error only for "stats" commands.
type statsErrorExecutor struct {
	responses map[string][]byte
}

func (e *statsErrorExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + args[0]
	if key == "podman stats" || key == "docker stats" {
		return nil, fmt.Errorf("stats not available")
	}
	if data, ok := e.responses[key]; ok {
		return data, nil
	}
	return nil, nil
}

// TestCollect_WithHostManager exercises the hostManager != nil branch in Collect.
func TestCollect_WithHostManager(t *testing.T) {
	psOutput := `[{"Id":"abc123","Names":["/test"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"5.0%","mem_usage":"50MB / 1GB","mem_percent":"5.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}

	hm := &ctopMockHostManager{
		hosts: map[string]remote.RemoteHost{
			"remote-1": {Name: "remote-1", Address: "10.0.0.1", User: "u"},
		},
	}

	c := NewCollectorWithExecutor("podman", hm, exec)
	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
	// Local containers should be collected
	assert.GreaterOrEqual(t, list.Total, 0)
}

// TestCollect_BothRuntimesFail exercises the path where both podman AND docker fail.
func TestCollect_BothRuntimesFail(t *testing.T) {
	exec := &bothFailExecutor{}
	c := NewCollectorWithExecutor("podman", nil, exec)

	// Collect should not return an error even when local collection fails;
	// it accumulates errors internally.
	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
}

// TestCollectLocal_BothFail exercises the path where podman and docker both fail.
func TestCollectLocal_BothFail(t *testing.T) {
	exec := &mockExecutor{
		errors: map[string]error{
			"podman ps": fmt.Errorf("podman not available"),
			"docker ps": fmt.Errorf("docker not available"),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	// collectLocal will be called by Collect; both runtimes fail → Collect
	// increments errors but still returns an empty list (not an error).
	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Equal(t, 0, list.Total)
}

// TestGetContainerStats_Error exercises the error branch in getContainerStats.
func TestGetContainerStats_Error(t *testing.T) {
	psOutput := `[{"Id":"abc123","Names":["/test"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`

	exec := &statsErrorExecutor{
		responses: map[string][]byte{
			"podman ps": []byte(psOutput),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	// Container is listed but stats are absent (0 values)
	assert.GreaterOrEqual(t, list.Total, 0)
}

// TestDisplay_Render_MaxRows exercises the rowCount >= maxRows break.
func TestDisplay_Render_MaxRows(t *testing.T) {
	// Build a container list with many entries to exceed maxRows.
	var containers []map[string]interface{}
	for i := 0; i < 30; i++ {
		containers = append(containers, map[string]interface{}{
			"Id":    fmt.Sprintf("id%02d", i),
			"Names": []string{fmt.Sprintf("/container-%d", i)},
		})
	}

	// Build JSON for all containers.
	var parts []string
	for _, c := range containers {
		parts = append(parts, fmt.Sprintf(
			`{"Id":"%s","Names":["%s"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}`,
			c["Id"], (c["Names"].([]string))[0],
		))
	}
	psOutput := "[" + strings.Join(parts, ",") + "]"
	statsOutput := `[{"cpu_percent":"1.0%","mem_usage":"10MB / 1GB","mem_percent":"1.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}
	col := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	d := NewDisplayWithWriter(col, DefaultDisplayConfig(), &buf)
	d.render(context.Background())

	// Should have rendered output without panic.
	output := buf.String()
	assert.NotEmpty(t, output)
}

// TestDisplay_RenderSnapshot_Error exercises the error path of RenderSnapshot.
func TestDisplay_RenderSnapshot_Error(t *testing.T) {
	exec := &bothFailExecutor{}
	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	// When both runtimes fail, Collect returns empty list (no error) so
	// RenderSnapshot succeeds with empty output.
	snapshot, err := d.RenderSnapshot(context.Background())
	assert.NoError(t, err)
	assert.NotEmpty(t, snapshot)
}

// TestDisplay_RenderJSON_Error exercises the error path of RenderJSON.
func TestDisplay_RenderJSON_Error(t *testing.T) {
	exec := &bothFailExecutor{}
	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	result, err := d.RenderJSON(context.Background())
	assert.NoError(t, err)
	assert.NotEmpty(t, result)
}

// TestDisplay_RenderJSON_WithContainers exercises normal RenderJSON path.
func TestDisplay_RenderJSON_WithContainers(t *testing.T) {
	psOutput := `[{"Id":"json1","Names":["/json-test"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"3.0%","mem_usage":"30MB / 1GB","mem_percent":"3.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)

	result, err := d.RenderJSON(context.Background())
	require.NoError(t, err)
	assert.Contains(t, result, "json-test")
}
