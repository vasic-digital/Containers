package compose

// Wave-20 batch CT-HARDEN-COMP-HARD-2 guard tests (§11.4.115 GREEN-polarity
// committed guards; committed default = GUARD). Each guard reproduces its
// finding via the package's own fake-compose-script seams (§11.4.27, no real
// docker/podman) before proving the fix.
//
// Surgical-revert RED evidence (recorded in the batch report):
//   CO-1: revert isPodmanCompose to `isPodmanComposeCmd(cmd, args)` alone
//         (drop the `|| isPodmanBackedCmd(...)` term) in
//         NewDefaultOrchestrator -> TestCO1_PodmanShimDetection_* FAILs:
//         isPodmanCompose stays false, Status() returns 0 services instead
//         of 1, and Up(WithWait) errors (the fake script rejects --wait).
//   CO-2: revert filterStatusesByServices to a no-op (`return statuses`) ->
//         TestCO2_WaitForServices_ScopedToRequestedServices FAILs/times out
//         because the unrelated exited "db" service keeps servicesReady()
//         false forever.
//   CO-2 (timeout smell): revert waitForServices(ctx, project, cfg.WaitTimeout)
//         back to cfg.Timeout -> TestCO2_WaitTimeoutOption_* FAILs because
//         WithWaitTimeout(1) has no effect and the wait is bounded by
//         WithUpTimeout(8) instead.
//   CO-3: revert envOrDefault(...) calls back to the hardcoded literals ->
//         TestCO3_DefaultHelixServices_CredentialsFromEnv FAILs because the
//         env overrides are silently ignored.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CO-1: podman-docker compatibility-shim detection ---

// TestCO1_PodmanShimDetection_ClassifiesAsPodman reproduces the exact CO-1
// scenario: detectComposeCmd's FIRST candidate, {"docker", ["compose"]},
// answers the `version` probe successfully on a podman-docker
// compatibility-shim host, but the runtime is actually podman. PATH is
// restricted to a directory containing ONLY a fake "docker" script so
// detectComposeCmd's other candidates (docker-compose, podman-compose,
// podman) are unresolvable and the real auto-detect path
// (NewDefaultOrchestrator -> detectComposeCmd) is exercised end-to-end.
func TestCO1_PodmanShimDetection_ClassifiesAsPodman(t *testing.T) {
	tmpDir := t.TempDir()
	dockerPath := filepath.Join(tmpDir, "docker")
	upArgsFile := filepath.Join(tmpDir, "up-args.txt")

	// The script mirrors real podman-docker shim behavior captured on this
	// host (2026-07-11): `docker version` / `docker compose version` exit 0
	// after printing "Emulate Docker CLI using podman. ..." plus
	// podman/podman-compose version lines. `ps` always answers with podman
	// JSON (regardless of requested --format) and `up` rejects the
	// docker-native --wait flag with exit 2, mirroring real podman-compose.
	script := `#!/bin/bash
case "$*" in
  *"version"*)
    echo "Emulate Docker CLI using podman. Create /etc/containers/nodocker to quiet msg." >&2
    echo "podman version 5.7.1"
    echo "podman-compose version 1.5.0"
    exit 0
    ;;
  *"--wait"*)
    exit 2
    ;;
  *"up"*)
    echo "$@" >> ` + upArgsFile + `
    exit 0
    ;;
  *"ps"*)
    echo '[{"Names":["proj_web_1"],"State":"running","Health":"healthy","ExitCode":0,"Labels":{"com.docker.compose.service":"web"}}]'
    exit 0
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(dockerPath, []byte(script), 0o755))

	// Restrict PATH to ONLY the fake "docker" so the real production
	// auto-detect path (NewDefaultOrchestrator -> detectComposeCmd) lands on
	// exactly the {"docker",["compose"]} candidate detectComposeCmd tries
	// FIRST -- the real repro path, not a synthetic bypass.
	t.Setenv("PATH", tmpDir)

	o, err := NewDefaultOrchestrator(tmpDir, nil)
	require.NoError(t, err)
	require.Equal(t, "docker", o.composeCmd)
	require.Equal(t, []string{"compose"}, o.composeArgs)

	assert.True(t, o.isPodmanCompose,
		"CO-1: a podman-docker compatibility-shim host resolves to "+
			"composeCmd=\"docker\" but is genuinely backed by podman -- "+
			"isPodmanCompose MUST classify it as podman so the podman-safe "+
			"Status()/Up() paths engage")

	project := ComposeProject{Name: "proj"}

	// (a) Status() must recover the running service through the podman JSON
	// parser -- pre-fix it silently returns empty via the docker
	// Go-template path (the HRD-081-class bug reached through the
	// undetected shim).
	statuses, err := o.Status(context.Background(), project)
	require.NoError(t, err)
	require.Len(t, statuses, 1,
		"CO-1(a): Status() must recover the running service via the podman "+
			"JSON parser, not silently return empty via the docker "+
			"Go-template path")
	assert.Equal(t, "web", statuses[0].Name)
	assert.Equal(t, "running", statuses[0].State)

	// (b) Up(WithWait) must NOT append the docker-native --wait flag -- the
	// fake script rejects it with exit 2 (mirroring real podman-compose);
	// pre-fix this hard-fails instead of falling back to host-side polling.
	err = o.Up(context.Background(), project, WithWait(true))
	require.NoError(t, err,
		"CO-1(b): Up(WithWait) must not pass the docker-native --wait flag "+
			"to a podman-backed runtime")

	data, err := os.ReadFile(upArgsFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "--wait")
}

// TestCO1_IsPodmanBackedCmd_SkipsAlreadyClassifiedCommands proves the probe
// is scoped: it never re-probes "podman" or "podman-compose" (already
// classified by isPodmanComposeCmd), so it cannot flip the deliberate
// docker-compose-compatible classification of the native `podman compose`
// provider.
func TestCO1_IsPodmanBackedCmd_SkipsAlreadyClassifiedCommands(t *testing.T) {
	assert.False(t, isPodmanBackedCmd("podman", []string{"compose"}, time.Second))
	assert.False(t, isPodmanBackedCmd("podman-compose", nil, time.Second))
}

// TestCO1_IsPodmanBackedCmd_NoBannerIsNotPodman is the negative control: a
// command whose version output contains no podman marker is NOT classified
// as podman-backed.
func TestCO1_IsPodmanBackedCmd_NoBannerIsNotPodman(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "docker")
	script := "#!/bin/sh\necho 'Docker Compose version v2.29.0'\nexit 0\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	assert.False(t, isPodmanBackedCmd(scriptPath, []string{"compose"}, 2*time.Second),
		"a genuine docker compose version banner must not be classified as podman-backed")
}

// --- CO-2: waitForServices must scope to the requested services ---

// TestCO2_WaitForServices_ScopedToRequestedServices proves Up({Services:
// [web]}, WithWait) succeeds once "web" is ready even though an UNRELATED
// service ("db") in the same compose project is exited. Pre-fix,
// waitForServices required every reported service (including "db") to be
// ready, so this would time out despite "web" being fully healthy.
// WithUpTimeout(3) bounds a pre-fix failure to a few seconds; post-fix it
// has no effect since the scoped wait succeeds on the first poll.
func TestCO2_WaitForServices_ScopedToRequestedServices(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")
	upArgsFile := filepath.Join(tmpDir, "up-args.txt")

	script := `#!/bin/bash
case "$*" in
  *"up"*)
    echo "$@" >> ` + upArgsFile + `
    exit 0
    ;;
  *"ps"*)
    echo '[{"Names":["proj_web_1"],"State":"running","Health":"healthy","ExitCode":0,"Labels":{"com.docker.compose.service":"web"}},{"Names":["proj_db_1"],"State":"exited","Health":"","ExitCode":1,"Labels":{"com.docker.compose.service":"db"}}]'
    exit 0
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)
	o.isPodmanCompose = true // exercise the host-side wait fallback

	project := ComposeProject{Name: "proj", Services: []string{"web"}}

	start := time.Now()
	err := o.Up(context.Background(), project, WithWait(true), WithUpTimeout(3))
	require.NoError(t, err,
		"CO-2: Up({Services:[web]}, WithWait) must succeed once the "+
			"REQUESTED service is ready, even though an unrelated service "+
			"(db) in the same compose project is exited/unhealthy")
	assert.Less(t, time.Since(start), 3*time.Second,
		"CO-2: waitForServices must not block on the unrelated db service")
}

// TestFilterStatusesByServices unit-tests the scoping helper directly.
func TestFilterStatusesByServices(t *testing.T) {
	statuses := []ServiceStatus{
		{Name: "web", State: "running", Health: "healthy"},
		{Name: "db", State: "exited", Health: ""},
	}

	t.Run("empty filter returns everything", func(t *testing.T) {
		assert.Equal(t, statuses, filterStatusesByServices(statuses, nil))
	})

	t.Run("scopes to requested subset", func(t *testing.T) {
		got := filterStatusesByServices(statuses, []string{"web"})
		require.Len(t, got, 1)
		assert.Equal(t, "web", got[0].Name)
	})

	t.Run("requested service not yet reported yields empty, not all", func(t *testing.T) {
		got := filterStatusesByServices(statuses, []string{"nonexistent"})
		assert.Empty(t, got)
	})
}

// TestCO2_WaitTimeoutOption_BoundsHostSideWaitIndependentOfUpTimeout proves
// WithWaitTimeout -- not WithUpTimeout -- bounds the host-side health-poll
// deadline. The service never becomes ready (stuck in "starting"), so Up()
// must time out at ~1s (WithWaitTimeout(1)), not ~8s (WithUpTimeout(8)).
func TestCO2_WaitTimeoutOption_BoundsHostSideWaitIndependentOfUpTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock-compose.sh")
	script := `#!/bin/bash
case "$*" in
  *"up"*) exit 0 ;;
  *"ps"*)
    echo '[{"Names":["proj_web_1"],"State":"running","Health":"starting","Labels":{"com.docker.compose.service":"web"}}]'
    exit 0
    ;;
esac
exit 0
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	o := NewOrchestrator(scriptPath, nil, tmpDir, nil)
	o.isPodmanCompose = true

	project := ComposeProject{Name: "proj"}

	start := time.Now()
	err := o.Up(context.Background(), project,
		WithWait(true), WithUpTimeout(8), WithWaitTimeout(1))
	require.Error(t, err, "service never becomes ready -> Up() must time out")
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 5*time.Second,
		"CO-2: WithWaitTimeout(1) must bound the host-side wait -- it must "+
			"not be conflated with the unrelated WithUpTimeout(8) value")
}

// --- CO-3: DefaultHelixServices() credentials must be env-overridable ---

func mustFindHelixService(t *testing.T, services []HelixService, name string) HelixService {
	t.Helper()
	for _, s := range services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("service %q not found in DefaultHelixServices()", name)
	return HelixService{}
}

// TestCO3_DefaultHelixServices_CredentialsFromEnv proves every credential
// DefaultHelixServices() embeds can be overridden via the environment
// (§6.R). Pre-fix these values were tracked-source literals and this test
// FAILs because the env vars are silently ignored.
func TestCO3_DefaultHelixServices_CredentialsFromEnv(t *testing.T) {
	t.Setenv("HELIX_COMPOSE_POSTGRES_USER", "custom_user")
	t.Setenv("HELIX_COMPOSE_POSTGRES_PASSWORD", "custom_pass")
	t.Setenv("HELIX_COMPOSE_POSTGRES_DB", "custom_db")
	t.Setenv("HELIX_COMPOSE_GRAFANA_ADMIN_USER", "custom_admin")
	t.Setenv("HELIX_COMPOSE_GRAFANA_ADMIN_PASSWORD", "custom_admin_pass")
	t.Setenv("HELIX_COMPOSE_VAULT_DEV_ROOT_TOKEN", "custom-vault-token")

	services := DefaultHelixServices()

	pgPrimary := mustFindHelixService(t, services, "postgres-primary")
	assert.Equal(t, "custom_user", pgPrimary.Env["POSTGRES_USER"])
	assert.Equal(t, "custom_pass", pgPrimary.Env["POSTGRES_PASSWORD"])
	assert.Equal(t, "custom_db", pgPrimary.Env["POSTGRES_DB"])

	pgReplica := mustFindHelixService(t, services, "postgres-replica")
	assert.Equal(t, "custom_user", pgReplica.Env["POSTGRES_USER"])
	assert.Equal(t, "custom_pass", pgReplica.Env["POSTGRES_PASSWORD"])
	assert.Equal(t, "custom_db", pgReplica.Env["POSTGRES_DB"])

	grafana := mustFindHelixService(t, services, "grafana")
	assert.Equal(t, "custom_admin", grafana.Env["GF_SECURITY_ADMIN_USER"])
	assert.Equal(t, "custom_admin_pass", grafana.Env["GF_SECURITY_ADMIN_PASSWORD"])

	vault := mustFindHelixService(t, services, "vault")
	assert.Equal(t, "custom-vault-token", vault.Env["VAULT_DEV_ROOT_TOKEN_ID"])
	assert.Equal(t, "0.0.0.0:8200", vault.Env["VAULT_DEV_LISTEN_ADDRESS"],
		"the non-secret bind address must be unaffected")
}

// TestCO3_DefaultHelixServices_FallsBackToDevDefaultsWhenUnset proves the
// documented dev-only fallback defaults still apply when the environment is
// unset, so this is a non-breaking change for every existing caller.
func TestCO3_DefaultHelixServices_FallsBackToDevDefaultsWhenUnset(t *testing.T) {
	services := DefaultHelixServices()

	pg := mustFindHelixService(t, services, "postgres-primary")
	assert.Equal(t, "helix", pg.Env["POSTGRES_USER"])
	assert.Equal(t, "helix", pg.Env["POSTGRES_PASSWORD"])
	assert.Equal(t, "helix", pg.Env["POSTGRES_DB"])

	grafana := mustFindHelixService(t, services, "grafana")
	assert.Equal(t, "admin", grafana.Env["GF_SECURITY_ADMIN_USER"])
	assert.Equal(t, "admin", grafana.Env["GF_SECURITY_ADMIN_PASSWORD"])

	vault := mustFindHelixService(t, services, "vault")
	assert.Equal(t, "helix-root-token", vault.Env["VAULT_DEV_ROOT_TOKEN_ID"])
}
