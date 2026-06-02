package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- HRD-081: podman-compose runtime detection ---

func TestIsPodmanComposeCmd(t *testing.T) {
	tests := []struct {
		name       string
		composeCmd string
		args       []string
		want       bool
	}{
		{
			name:       "standalone podman-compose",
			composeCmd: "podman-compose",
			args:       nil,
			want:       true,
		},
		{
			name:       "docker compose plugin",
			composeCmd: "docker",
			args:       []string{"compose"},
			want:       false,
		},
		{
			name:       "standalone docker-compose",
			composeCmd: "docker-compose",
			args:       nil,
			want:       false,
		},
		{
			// `podman compose` is the docker-compose-compatible provider
			// invoked via the podman CLI; it is NOT classified as the
			// standalone podman-compose tool.
			name:       "podman compose provider",
			composeCmd: "podman",
			args:       []string{"compose"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				isPodmanComposeCmd(tt.composeCmd, tt.args))
		})
	}
}

func TestNewOrchestrator_SetsPodmanComposeFlag(t *testing.T) {
	podman := NewOrchestrator("podman-compose", nil, "/tmp", nil)
	assert.True(t, podman.isPodmanCompose,
		"podman-compose orchestrator must set isPodmanCompose")

	docker := NewOrchestrator("docker", []string{"compose"}, "/tmp", nil)
	assert.False(t, docker.isPodmanCompose,
		"docker compose orchestrator must NOT set isPodmanCompose")
}

// --- HRD-081 (1): --wait flag handling per runtime ---

// captureUpArgs runs Up against a tiny shell script that records its argv,
// then returns the captured argument string. The script always exits 0 so
// Up's command succeeds; for the podman wait-fallback path the script also
// answers the follow-up `ps` poll with a ready service so waitForServices
// returns promptly.
func captureUpArgs(
	t *testing.T,
	composeCmd string,
	composeArgs []string,
	project ComposeProject,
	opts ...UpOption,
) string {
	t.Helper()

	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "up-args.txt")
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	// The script appends to up-args.txt only for the `up` invocation
	// (skips the `ps` poll), and emits a ready podman JSON record for any
	// `ps` invocation so the podman wait fallback completes immediately.
	script := `#!/bin/bash
case "$*" in
  *" ps "*|*" ps")
    echo '[{"Names":["svc_1"],"State":"running","Health":"healthy","ExitCode":0,"Labels":{"com.docker.compose.service":"svc"}}]'
    ;;
  *up*)
    echo "$@" > ` + argsFile + `
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, composeArgs, tmpDir, nil)
	// Force the runtime classification independent of the script name so we
	// exercise BOTH branches deterministically.
	o.isPodmanCompose = isPodmanComposeCmd(composeCmd, composeArgs)

	err := o.Up(context.Background(), project, opts...)
	require.NoError(t, err)

	data, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	return string(data)
}

func TestUp_DockerCompose_IncludesWaitFlag(t *testing.T) {
	args := captureUpArgs(t,
		"docker", []string{"compose"},
		ComposeProject{Name: "p", File: "compose.yml"},
		WithWait(true),
	)
	assert.Contains(t, args, "--wait",
		"docker compose up with WithWait(true) MUST emit --wait")
}

func TestUp_PodmanCompose_OmitsWaitFlag(t *testing.T) {
	args := captureUpArgs(t,
		"podman-compose", nil,
		ComposeProject{Name: "p", File: "compose.yml"},
		WithWait(true),
	)
	assert.NotContains(t, args, "--wait",
		"podman-compose up MUST NOT emit the unsupported --wait flag")
}

func TestUp_PodmanCompose_NoWait_OmitsWaitFlag(t *testing.T) {
	// Sanity: without WithWait, neither runtime emits --wait.
	args := captureUpArgs(t,
		"podman-compose", nil,
		ComposeProject{Name: "p", File: "compose.yml"},
	)
	assert.NotContains(t, args, "--wait")
}

// TestUp_PodmanCompose_WaitFallbackPolls proves the host-side wait fallback
// is actually exercised: the mock answers the first `ps` poll with a
// not-yet-ready (starting) service and the second with a ready one, so Up
// returns only after polling observed readiness.
func TestUp_PodmanCompose_WaitFallbackPolls(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "poll-count")
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
case "$*" in
  *" ps "*|*" ps")
    n=0
    [ -f ` + stateFile + ` ] && n=$(cat ` + stateFile + `)
    n=$((n+1))
    echo "$n" > ` + stateFile + `
    if [ "$n" -ge 2 ]; then
      echo '[{"Names":["svc_1"],"State":"running","Health":"healthy","ExitCode":0,"Labels":{"com.docker.compose.service":"svc"}}]'
    else
      echo '[{"Names":["svc_1"],"State":"running","Health":"starting","ExitCode":0,"Labels":{"com.docker.compose.service":"svc"}}]'
    fi
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)
	o.isPodmanCompose = true

	err := o.Up(context.Background(), ComposeProject{Name: "p"},
		WithWait(true), WithUpTimeout(10))
	require.NoError(t, err)

	data, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, string(data), "2",
		"wait fallback should poll until the service is healthy")
}

// --- HRD-081 (2): podman-compose ps JSON parsing ---

// representativePodmanPsJSON is a real `podman ps --format json` sample
// (captured 2026-06-02 from podman-compose 1.5.0 / podman 5.8.2 against a
// busybox compose project). The current docker-template Status() path
// returns ZERO services for output of this shape because podman's `--format`
// is a podman ps template ({{.Names}}, not {{.Name}}) — the bug HRD-081
// fixes.
const representativePodmanPsJSON = `[
  {
    "AutoRemove": false,
    "Command": ["sh","-c","sleep 600"],
    "Exited": false,
    "ExitCode": 0,
    "Id": "e8b3d4a512d9",
    "Image": "docker.io/library/busybox:latest",
    "Labels": {
      "com.docker.compose.project": "hrd081test",
      "com.docker.compose.service": "web",
      "io.podman.compose.service": "web"
    },
    "Names": ["hrd081test_web_1"],
    "Ports": [
      {"host_ip":"","container_port":80,"host_port":18080,"range":1,"protocol":"tcp"}
    ],
    "State": "running",
    "Status": "Up 18 seconds"
  },
  {
    "Exited": true,
    "ExitCode": 1,
    "Labels": {"com.docker.compose.service":"db"},
    "Names": ["hrd081test_db_1"],
    "Ports": null,
    "State": "exited",
    "Status": "Exited (1) 2 seconds ago"
  }
]`

func TestParsePodmanStatusJSON_Representative(t *testing.T) {
	statuses, err := parsePodmanStatusJSON(representativePodmanPsJSON)
	require.NoError(t, err)
	require.Len(t, statuses, 2,
		"representative podman ps json must parse to TWO services")

	web := statuses[0]
	assert.Equal(t, "web", web.Name,
		"name resolves from compose service label, not container name")
	assert.Equal(t, "running", web.State)
	assert.Equal(t, []string{"0.0.0.0:18080->80/tcp"}, web.Ports)
	assert.Equal(t, 0, web.ExitCode)

	db := statuses[1]
	assert.Equal(t, "db", db.Name)
	assert.Equal(t, "exited", db.State)
	assert.Equal(t, 1, db.ExitCode)
	assert.Nil(t, db.Ports)
}

// TestParsePodmanStatusJSON_NegativeOldParserMissesServices is the paired
// negative: feeding the representative podman JSON into the OLD docker
// pipe-template parser (parseStatusOutput) yields ZERO services — proving
// the old path silently lost running containers and the new JSON parser is
// load-bearing.
func TestParsePodmanStatusJSON_NegativeOldParserMissesServices(t *testing.T) {
	// The docker template parser splits on '|' and needs 5 fields; podman
	// JSON has none, so every "line" is dropped.
	oldResult := parseStatusOutput(representativePodmanPsJSON)
	assert.Empty(t, oldResult,
		"old docker-template parser MUST miss services in podman json "+
			"(this is the HRD-081 bug)")

	// The new parser recovers them.
	newResult, err := parsePodmanStatusJSON(representativePodmanPsJSON)
	require.NoError(t, err)
	assert.Len(t, newResult, 2,
		"new podman json parser MUST recover the services the old "+
			"parser missed")
}

func TestParsePodmanStatusJSON_EdgeCases(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		s, err := parsePodmanStatusJSON("")
		require.NoError(t, err)
		assert.Empty(t, s)
	})

	t.Run("whitespace only", func(t *testing.T) {
		s, err := parsePodmanStatusJSON("   \n  ")
		require.NoError(t, err)
		assert.Empty(t, s)
	})

	t.Run("empty json array", func(t *testing.T) {
		s, err := parsePodmanStatusJSON("[]")
		require.NoError(t, err)
		assert.Empty(t, s)
	})

	t.Run("malformed json errors", func(t *testing.T) {
		_, err := parsePodmanStatusJSON("not json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "podman ps json")
	})

	t.Run("falls back to container name when no label", func(t *testing.T) {
		s, err := parsePodmanStatusJSON(
			`[{"Names":["bare_ctr"],"State":"running"}]`)
		require.NoError(t, err)
		require.Len(t, s, 1)
		assert.Equal(t, "bare_ctr", s[0].Name)
	})

	t.Run("derives exited state from Exited flag", func(t *testing.T) {
		s, err := parsePodmanStatusJSON(
			`[{"Names":["c"],"State":"","Exited":true,"ExitCode":2,` +
				`"Labels":{"com.docker.compose.service":"svc"}}]`)
		require.NoError(t, err)
		require.Len(t, s, 1)
		assert.Equal(t, "exited", s[0].State)
		assert.Equal(t, 2, s[0].ExitCode)
	})
}

func TestPodmanPortStrings(t *testing.T) {
	tests := []struct {
		name  string
		ports []podmanPort
		want  []string
	}{
		{
			name:  "nil",
			ports: nil,
			want:  nil,
		},
		{
			name: "single tcp with empty host_ip defaults to 0.0.0.0",
			ports: []podmanPort{
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			},
			want: []string{"0.0.0.0:8080->80/tcp"},
		},
		{
			name: "explicit host ip + udp",
			ports: []podmanPort{
				{HostIP: "127.0.0.1", HostPort: 53,
					ContainerPort: 53, Protocol: "udp"},
			},
			want: []string{"127.0.0.1:53->53/udp"},
		},
		{
			name: "empty protocol defaults to tcp",
			ports: []podmanPort{
				{HostPort: 443, ContainerPort: 443},
			},
			want: []string{"0.0.0.0:443->443/tcp"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, podmanPortStrings(tt.ports))
		})
	}
}

// --- servicesReady / summarizeStatuses helpers ---

func TestServicesReady(t *testing.T) {
	tests := []struct {
		name     string
		statuses []ServiceStatus
		want     bool
	}{
		{
			name:     "empty is not ready",
			statuses: nil,
			want:     false,
		},
		{
			name: "running healthy is ready",
			statuses: []ServiceStatus{
				{Name: "a", State: "running", Health: "healthy"},
			},
			want: true,
		},
		{
			name: "running no-healthcheck is ready",
			statuses: []ServiceStatus{
				{Name: "a", State: "running", Health: ""},
			},
			want: true,
		},
		{
			name: "starting is not ready",
			statuses: []ServiceStatus{
				{Name: "a", State: "running", Health: "starting"},
			},
			want: false,
		},
		{
			name: "unhealthy is not ready",
			statuses: []ServiceStatus{
				{Name: "a", State: "running", Health: "unhealthy"},
			},
			want: false,
		},
		{
			name: "non-running is not ready",
			statuses: []ServiceStatus{
				{Name: "a", State: "exited"},
			},
			want: false,
		},
		{
			name: "one not-ready among many fails",
			statuses: []ServiceStatus{
				{Name: "a", State: "running", Health: "healthy"},
				{Name: "b", State: "running", Health: "starting"},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, servicesReady(tt.statuses))
		})
	}
}

// --- HRD-081: Status() routes to the JSON parser for podman-compose ---

func TestStatus_PodmanCompose_UsesJSONParser(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "ps-args.txt")
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
echo '[{"Names":["proj_web_1"],"State":"running","Health":"healthy","ExitCode":0,"Ports":[{"host_ip":"","container_port":80,"host_port":8080,"protocol":"tcp"}],"Labels":{"com.docker.compose.service":"web"}}]'
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)
	o.isPodmanCompose = true

	statuses, err := o.Status(context.Background(),
		ComposeProject{Name: "proj"})
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "web", statuses[0].Name)
	assert.Equal(t, "running", statuses[0].State)
	assert.Equal(t, []string{"0.0.0.0:8080->80/tcp"}, statuses[0].Ports)

	// Assert the podman JSON format flag was used, not the docker template.
	psArgs, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Contains(t, string(psArgs), "--format json",
		"podman Status() MUST request json, not the docker template")
	assert.NotContains(t, string(psArgs), "{{.Name}}",
		"podman Status() MUST NOT use the docker Go-template")
}

func TestStatus_DockerCompose_StillUsesTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "ps-args.txt")
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")

	script := `#!/bin/bash
echo "$@" > ` + argsFile + `
echo "web|running|healthy|0.0.0.0:8080->80/tcp|0"
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, []string{"compose"}, tmpDir, nil)
	require.False(t, o.isPodmanCompose)

	statuses, err := o.Status(context.Background(),
		ComposeProject{Name: "proj"})
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "web", statuses[0].Name)

	psArgs, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Contains(t, string(psArgs), "{{.Name}}",
		"docker Status() MUST keep using the Go-template format")
}
