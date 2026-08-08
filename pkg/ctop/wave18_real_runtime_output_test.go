package ctop

// Wave-18 CT-F1 — schema-tolerant runtime-output decoding.
//
// §11.4.115 RED-on-broken + §11.4.107(10): these fixtures are REAL captured
// output from `podman ps --format json` / `podman stats --format json` on the
// build host (rootless podman per §11.4.161), plus the documented Docker-Engine
// NDJSON shape (this host's `docker` is a podman shim, so the daemon NDJSON
// path is grounded in Docker's documented `--format json` output and marked as
// such — §11.4.6 honest boundary). The pre-fix decoder cannot parse ANY of
// them: `podman ps` returns State as a string and Created/StartedAt as unix
// ints (the struct expected a nested State object + time.Time), `docker ps`
// emits NDJSON (the struct expected a JSON array), and `podman stats` uses
// snake_case field names (the struct expected Docker PascalCase). Every
// assertion below FAILS against the pre-fix code and PASSES against the
// tolerant decoder.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real `podman ps -a --format json` (trimmed to the fields the decoder reads).
// State is a top-level STRING, Status a top-level STRING, Created/StartedAt
// unix INTS, Names an ARRAY, id key "Id", Ports snake_case objects.
const realPodmanPS = `[
  {
    "Id": "fc7e996657df4989aba0cb63e22d94b81c74b3e2af1d580988b94cc9cf142ddc",
    "Names": ["helix_sonarqube_db"],
    "Image": "docker.io/library/postgres:16",
    "Created": 1783192758,
    "CreatedAt": "5 days ago",
    "State": "running",
    "Status": "Up 5 days",
    "StartedAt": 1783192759,
    "Ports": [{"host_ip":"0.0.0.0","container_port":5432,"host_port":5432,"range":1,"protocol":"tcp"}],
    "Labels": {"com.docker.compose.service":"db"}
  }
]`

// Real `docker ps -a --format json` is NDJSON (one object per line, NOT an
// array), id key "ID", Names a comma STRING, State/Status top-level strings,
// Ports a STRING. (Docker Engine documented shape.)
const realDockerPSNDJSON = `{"Command":"nginx -g","CreatedAt":"2024-01-01 00:00:00 +0000 UTC","ID":"abc123def456","Image":"nginx:latest","Labels":"role=web","Names":"web-1","Ports":"0.0.0.0:8080->80/tcp","State":"running","Status":"Up 2 hours"}
{"Command":"redis-server","CreatedAt":"2024-01-01 00:00:00 +0000 UTC","ID":"def456abc789","Image":"redis:7","Labels":"","Names":"cache","Ports":"","State":"exited","Status":"Exited (0) 1 hour ago"}`

// Real `podman stats --no-stream --format json` — a JSON ARRAY with snake_case
// keys (id/name/cpu_percent/mem_usage/mem_percent/net_io/block_io/pids).
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

// Docker Engine `docker stats --no-stream --format json` — NDJSON, PascalCase.
const realDockerStatsNDJSON = `{"BlockIO":"4kB / 8kB","CPUPerc":"5.00%","Container":"abc123def456","ID":"abc123def456","MemPerc":"10.00%","MemUsage":"64MiB / 512MiB","Name":"web-1","NetIO":"1kB / 2kB","PIDs":"3"}`

func TestWave18_ParseContainerList_RealPodmanPS(t *testing.T) {
	got, err := parseContainerList([]byte(realPodmanPS), "podman", "local")
	require.NoError(t, err, "real podman ps array must decode")
	require.Len(t, got, 1)
	c := got[0]
	assert.Equal(t, "fc7e996657df", c.ID, "id from Id, shortened to 12")
	assert.Equal(t, "helix_sonarqube_db", c.Name, "name from Names[0]")
	assert.Equal(t, "docker.io/library/postgres:16", c.Image)
	assert.Equal(t, "running", c.State, "State is a top-level string, not State.Status")
	assert.Equal(t, "Up 5 days", c.Status, "Status is a top-level string")
	assert.False(t, c.StartedAt.IsZero(), "StartedAt from unix int")
	assert.Contains(t, c.Ports, "5432/tcp", "port from snake_case host_port/protocol")
}

func TestWave18_ParseContainerList_RealDockerNDJSON(t *testing.T) {
	got, err := parseContainerList([]byte(realDockerPSNDJSON), "docker", "local")
	require.NoError(t, err, "real docker NDJSON must decode")
	require.Len(t, got, 2)
	assert.Equal(t, "abc123def456", got[0].ID, "id from docker ID key")
	assert.Equal(t, "web-1", got[0].Name, "name from docker Names string")
	assert.Equal(t, "running", got[0].State)
	assert.Equal(t, "exited", got[1].State)
	assert.Contains(t, got[0].Ports, "8080/tcp", "HOST port from docker Ports string")
}

func TestWave18_ParseContainerStats_RealPodmanSnakeCase(t *testing.T) {
	got := parseContainerStats([]byte(realPodmanStats))
	require.NotNil(t, got, "real podman stats array must decode")
	assert.InDelta(t, 12.5, got.CPUPercent, 0.001, "cpu_percent snake_case")
	assert.Greater(t, got.MemoryUsage, uint64(0), "mem_usage snake_case")
	assert.InDelta(t, 25.0, got.MemoryPercent, 0.001, "mem_percent snake_case")
	assert.Equal(t, 7, got.PIDs, "pids snake_case")
}

func TestWave18_ParseContainerStats_RealDockerPascalCase(t *testing.T) {
	got := parseContainerStats([]byte(realDockerStatsNDJSON))
	require.NotNil(t, got, "real docker stats must decode")
	assert.InDelta(t, 5.0, got.CPUPercent, 0.001, "CPUPerc PascalCase")
	assert.Greater(t, got.MemoryUsage, uint64(0), "MemUsage PascalCase")
	assert.Equal(t, 3, got.PIDs, "PIDs PascalCase")
}
