package remote

// CT-HARDEN-42 regression guard — RemoteRuntime.Status inspect template must
// GUARD the nil .State.Health pointer.
//
// Real runtime signature (§11.4.108) captured out-of-band on podman 5.7.1,
// postgres:17 with NO HEALTHCHECK:
//   - unguarded '{{.State.Health.Status}}'                    -> exit 125,
//       stderr "nil pointer evaluating *define.HealthCheckResults.Status"
//   - guarded  '{{if .State.Health}}{{.State.Health.Status}}{{end}}' -> exit 0,
//       empty health field, valid 7-field line
//   - running container WITH a healthcheck (guarded)          -> exit 0,
//       health="healthy" (no regression)
//
// docker/podman run Go text/template for `--format`, so a nil-pointer field
// access aborts the WHOLE inspect (non-zero exit) and Status() returns that
// error, reporting NOTHING for the (common) no-healthcheck container. This
// test drives the production template `statusInspectFormat` through the same
// text/template engine (§11.4.115 polarity: un-guarding the production const
// flips this test RED — the nil-pointer twin below documents the pre-fix form).
//
// Honest gap (§11.4.6): this is a text/template unit test, NOT a live
// RemoteRuntime.Status() over SSH; the live signature is the out-of-band
// podman capture above.

import (
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/runtime"
)

type cthard42Health struct {
	Status string
}

type cthard42State struct {
	Status     string
	Health     *cthard42Health
	StartedAt  string
	FinishedAt string
	ExitCode   int
}

type cthard42Container struct {
	Id    string
	Name  string
	State cthard42State
}

func renderInspect(format string, c cthard42Container) (string, error) {
	tmpl, err := template.New("inspect").Parse(format)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, c); err != nil {
		return "", err
	}
	return b.String(), nil
}

// unguardedStatusInspectFormat is the PRE-FIX (broken) template twin: a bare
// {{.State.Health.Status}} with no nil guard. Kept here so the test proves the
// defect is real (this twin aborts on a nil .State.Health) independent of the
// production const's current state.
const unguardedStatusInspectFormat = "{{.Id}}|{{.Name}}|{{.State.Status}}|" +
	"{{.State.Health.Status}}|" +
	"{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}"

func TestCTHarden42_Status_NoHealthcheck_TemplateGuard(t *testing.T) {
	noHC := cthard42Container{
		Id:   "abc123",
		Name: "/c",
		State: cthard42State{
			Status:     "created",
			Health:     nil,
			StartedAt:  "0001-01-01T00:00:00Z",
			FinishedAt: "0001-01-01T00:00:00Z",
		},
	}

	_, redErr := renderInspect(unguardedStatusInspectFormat, noHC)
	require.Error(t, redErr,
		"pre-fix unguarded template MUST fail on a nil .State.Health (RED reproduction)")
	require.Contains(t, redErr.Error(), "nil pointer evaluating",
		"RED must reproduce the runtime's nil-pointer template abort")

	out, greenErr := renderInspect(statusInspectFormat, noHC)
	require.NoError(t, greenErr,
		"guarded template MUST NOT fail on a nil .State.Health (GREEN)")
	st, perr := parseRemoteStatus(out)
	require.NoError(t, perr)
	assert.Equal(t, "abc123", st.ID)
	assert.Equal(t, "c", st.Name)
	assert.Equal(t, runtime.ContainerState("created"), st.State)
	assert.Equal(t, "", st.Health,
		"a no-healthcheck container must report empty health, not an error")
}

func TestCTHarden42_Status_WithHealthcheck_PreservesHealth(t *testing.T) {
	withHC := cthard42Container{
		Id:   "def456",
		Name: "/c",
		State: cthard42State{
			Status:     "running",
			Health:     &cthard42Health{Status: "healthy"},
			StartedAt:  "2026-07-11T00:00:00Z",
			FinishedAt: "0001-01-01T00:00:00Z",
		},
	}

	out, err := renderInspect(statusInspectFormat, withHC)
	require.NoError(t, err)
	st, perr := parseRemoteStatus(out)
	require.NoError(t, perr)
	assert.Equal(t, runtime.StateRunning, st.State)
	assert.Equal(t, "healthy", st.Health,
		"guard MUST preserve a real health status when a healthcheck exists")
}
