package runtime

// Wave-18b RT-F1 — real `podman stats --format json` output decoding.
//
// §11.4.115 RED-on-broken + §11.4.107(10): this fixture is the REAL shape
// `podman stats --no-stream --format json` emits on the build host (rootless
// podman per §11.4.161) — a JSON ARRAY of objects whose every field is a
// STRING, with combined `net_io`/`block_io` mappings, NOT the numeric shape
// (`"cpu_percent": 2.5`, separate `net_input`/`net_output`) the old
// `podmanStatsJSON` struct modelled. Against the pre-fix code the numeric
// struct unmarshal fails and the docker fallback then fails on the leading
// `[`, so `PodmanRuntime.Stats` returns an error for EVERY real podman
// container. Every assertion below FAILS against the pre-fix code and PASSES
// against the string-then-parse decoder.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realPodmanStats is captured `podman stats --no-stream --format json` output:
// a JSON array; every field a string; net_io/block_io combined "rx / tx".
const realPodmanStats = `[
 {
  "id": "fc7e996657df",
  "name": "helix_sonarqube_db",
  "cpu_percent": "12.50%",
  "avg_cpu": "9.00%",
  "mem_usage": "128MB / 512MB",
  "mem_percent": "25.00%",
  "net_io": "1kB / 2kB",
  "block_io": "4kB / 8kB",
  "pids": "7"
 }
]`

func TestWave18b_PodmanStats_RealStringArrayOutput(t *testing.T) {
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(realPodmanStats), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(exec)

	stats, err := p.Stats(context.Background(), "fc7e996657df")
	require.NoError(t, err, "real podman stats string-array output must decode")
	require.NotNil(t, stats)

	assert.InDelta(t, 12.5, stats.CPUPercent, 0.01, "cpu_percent is a string '12.50%'")
	assert.InDelta(t, 25.0, stats.MemoryPercent, 0.01, "mem_percent is a string '25.00%'")
	assert.Equal(t, 7, stats.PIDs, "pids is a string '7'")
	assert.Greater(t, stats.MemoryUsage, uint64(0), "mem_usage from combined 'used / limit'")
	assert.Greater(t, stats.MemoryLimit, uint64(0), "mem limit from combined 'used / limit'")
	assert.Greater(t, stats.NetworkRx, uint64(0), "net_io rx from combined 'rx / tx'")
	assert.Greater(t, stats.BlockRead, uint64(0), "block_io read from combined 'read / write'")
}
