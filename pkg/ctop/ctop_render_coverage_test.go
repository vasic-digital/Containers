package ctop

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDisplay_Render_NoColor exercises the render path with NoColor=true.
func TestDisplay_Render_NoColor(t *testing.T) {
	psOutput := `[{"Id":"render1","Names":["/render-test"],"Image":"nginx:latest","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"10.0%","mem_usage":"100MB / 1GB","mem_percent":"10.0%","net_io":"1MB / 1MB","block_io":"10MB / 10MB","pids":"2"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	config := DefaultDisplayConfig()
	config.NoColor = true
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, config, &buf)

	d.render(context.Background())

	output := buf.String()
	assert.Contains(t, output, "render-test")
}

// TestDisplay_Render_NoContainers exercises the "no containers found" branch.
func TestDisplay_Render_NoContainers(t *testing.T) {
	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps": []byte(`[]`),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	config := DefaultDisplayConfig()
	config.ShowStopped = true
	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, config, &buf)

	d.render(context.Background())

	output := buf.String()
	assert.Contains(t, output, "No containers found")
}

// TestDisplay_Render_WithFilter exercises filter during render.
func TestDisplay_Render_WithFilter(t *testing.T) {
	psOutput := `[{"Id":"flt1","Names":["/web-server"],"Image":"nginx:latest","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200},{"Id":"flt2","Names":["/db-server"],"Image":"postgres","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"5.0%","mem_usage":"50MB / 1GB","mem_percent":"5.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`

	exec := &mockExecutor{
		responses: map[string][]byte{
			"podman ps":    []byte(psOutput),
			"podman stats": []byte(statsOutput),
		},
	}
	c := NewCollectorWithExecutor("podman", nil, exec)

	var buf bytes.Buffer
	d := NewDisplayWithWriter(c, DefaultDisplayConfig(), &buf)
	d.SetFilterName("web")

	d.render(context.Background())

	output := buf.String()
	assert.Contains(t, output, "web-server")
}
