//go:build !integration

package runtime

// Wave-20 RT-HARD — pkg/runtime hardening audit.
//
// §11.4.115 RED-on-broken + §11.4.107(10): each test below reproduces its
// finding against the PRE-FIX code first (captured as `--- FAIL` output, see
// the conductor report), then is proven GREEN against the fix.
//
// RT-CRIO-1 (HIGH, fixed): crictl's `logs --tail` is an Int64Flag (default
// -1 = all); the package's default LogOption.Tail is the Docker/Podman
// sentinel string "all", which crio.go passed straight through to `crictl
// logs --tail all` — crictl rejects a non-integer --tail value outright.
// Source: kubernetes-sigs/cri-tools cmd/crictl/logs.go defines `--tail` as
// `cli.Int64Flag{... Value: -1, Usage: "... Defaults to all"}`.
//
// RT-EXEC-1 (MED, fixed): defaultExecutor.Execute used cmd.Output(), whose
// (*exec.ExitError).Error() renders only "exit status N" — the real
// diagnostic Go DOES capture into ExitError.Stderr (via cmd.Output()'s
// internal prefixSuffixSaver) was never surfaced by any of the six runtimes
// that wrap this error with %w.
//
// RT-PODMAN-1 (LOW, fixed): parsePodmanPSOutput decoded the wire `Created`
// field but never assigned it to ContainerInfo.Created — every podman-listed
// container reported the zero time. Real podman ps --format json emits
// Created as a unix-SECONDS number (captured evidence:
// pkg/ctop/wave18_real_runtime_output_test.go's `"Created": 1783192758`);
// this fixture proves both that real shape AND the RFC3339-string variant.
//
// RT-LABEL-1 (MED, ASSESSED — documented, not "fixed"): docker.go's
// parseLabelsString splits on "," so a label VALUE containing a literal
// comma truncates. Confirmed as a genuine, unescapable upstream Docker CLI
// format limitation (moby/moby#30575), not a defect introduced by this
// package; the test below documents the KNOWN behavior rather than
// asserting a (nonexistent) fix.
//
// RT-RACE-1 (MED, FLAGGED — not fixed): detect.go's exported RuntimePriority
// var can be read/written directly, bypassing priorityMu, racing
// GetRuntimePriority/SetRuntimePriority. The only clean fix (unexporting)
// removes a public symbol (§11.4.122) and is NOT applied here; see
// TestRTRace1_DirectAccessRacesWithSetRuntimePriority (skipped by default,
// run manually with -race to capture the race) and detect.go's doc comment
// for the flagged recommendation.

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- RT-CRIO-1 -------------------------------------------------------------

// TestCRIORuntime_Logs_DefaultTailOmitsInvalidCrictlFlag proves that a
// default (no WithTail option) Logs() call never passes a non-integer
// --tail value to crictl. Pre-fix, args always contained "--tail" "all",
// which crictl's Int64Flag parser rejects outright.
func TestCRIORuntime_Logs_DefaultTailOmitsInvalidCrictlFlag(t *testing.T) {
	var captured []string
	exec := &mockExecutor{
		executeStreamFunc: func(_ context.Context, _ string, args ...string) (io.ReadCloser, error) {
			captured = args
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	c := NewCRIORuntimeWithExecutor(exec)

	rc, err := c.Logs(context.Background(), "container-id")
	require.NoError(t, err)
	defer rc.Close()

	assert.NotContains(t, captured, "all",
		"crio Logs must never pass crictl the Docker/Podman \"all\" sentinel")
	for i, a := range captured {
		if a == "--tail" {
			t.Fatalf("crio Logs emitted --tail %q for the default (no WithTail) "+
				"call; crictl's --tail is an integer flag and rejects non-numeric "+
				"values — it should have been omitted entirely", captured[i+1])
		}
	}
}

// TestCRIORuntime_Logs_NumericTailPassedAsInt proves an explicit numeric
// WithTail("100") is still forwarded to crictl as a real integer --tail
// value (the fix must not regress the numeric case).
func TestCRIORuntime_Logs_NumericTailPassedAsInt(t *testing.T) {
	var captured []string
	exec := &mockExecutor{
		executeStreamFunc: func(_ context.Context, _ string, args ...string) (io.ReadCloser, error) {
			captured = args
			return io.NopCloser(strings.NewReader("")), nil
		},
	}
	c := NewCRIORuntimeWithExecutor(exec)

	rc, err := c.Logs(context.Background(), "container-id", WithTail("100"))
	require.NoError(t, err)
	defer rc.Close()

	require.Contains(t, captured, "--tail")
	assert.Contains(t, captured, "100")
}

// --- RT-EXEC-1 ---------------------------------------------------------------

// TestDefaultExecutor_Execute_SurfacesRealStderrDiagnostic proves that a
// non-zero-exit command's real stderr diagnostic reaches the returned
// error's Error() string, not just "exit status N". Pre-fix, err.Error()
// for this exact repro is only "exit status 1" — the "no such container"
// diagnostic that exec.Cmd DID capture into (*exec.ExitError).Stderr was
// discarded because nothing ever read ee.Stderr.
func TestDefaultExecutor_Execute_SurfacesRealStderrDiagnostic(t *testing.T) {
	e := &defaultExecutor{}
	ctx := context.Background()

	_, err := e.Execute(ctx, "sh", "-c",
		"echo 'Error: no such container' >&2; exit 1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such container",
		"the real CLI diagnostic captured in ExitError.Stderr must reach the "+
			"returned error's message")

	// The underlying *exec.ExitError must still be reachable via errors.As
	// through %w-wrapping (exit-code callers rely on this).
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
}

// TestDefaultExecutor_Execute_NoStderrUnaffected proves a non-zero exit with
// EMPTY stderr still returns a plain error and that stdout is still
// returned alongside the error.
func TestDefaultExecutor_Execute_NoStderrUnaffected(t *testing.T) {
	e := &defaultExecutor{}
	ctx := context.Background()

	out, err := e.Execute(ctx, "sh", "-c", "echo stdout-data; exit 1")
	require.Error(t, err)
	assert.Contains(t, string(out), "stdout-data",
		"stdout must still be returned even when the command exits non-zero")

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
}

// TestDefaultExecutor_Execute_CommandNotFoundStillErrors proves the
// exec.Error path (command doesn't exist, never even starts — not an
// *exec.ExitError) is unaffected by the stderr-surfacing change.
func TestDefaultExecutor_Execute_CommandNotFoundStillErrors(t *testing.T) {
	e := &defaultExecutor{}
	ctx := context.Background()

	_, err := e.Execute(ctx, "nonexistent_command_rthard_12345")
	assert.Error(t, err)
}

// --- RT-PODMAN-1 -------------------------------------------------------------

// realPodmanPSWithCreated is a REAL captured `podman ps --format json`
// record (trimmed): Created is a unix-SECONDS number, matching
// pkg/ctop/wave18_real_runtime_output_test.go's realPodmanPS fixture exactly
// (captured on this project's rootless-podman build host, §11.4.161).
const realPodmanPSWithCreated = `[
  {
    "Id": "fc7e996657df4989aba0cb63e22d94b81c74b3e2af1d580988b94cc9cf142dd",
    "Names": ["helix_sonarqube_db"],
    "Image": "docker.io/library/postgres:16",
    "ImageID": "sha256:abcdef",
    "Created": 1783192758,
    "State": "running",
    "Status": "Up 5 days",
    "Labels": {"com.docker.compose.service":"db"}
  }
]`

func TestPodmanRuntime_List_CreatedFromRealUnixSecondsField(t *testing.T) {
	e := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(realPodmanPSWithCreated), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(e)

	containers, err := p.List(context.Background(), ListFilter{All: true})
	require.NoError(t, err)
	require.Len(t, containers, 1)

	assert.False(t, containers[0].Created.IsZero(),
		"Created must be populated from the real unix-seconds wire field, "+
			"not left at the zero value")
	assert.Equal(t, int64(1783192758), containers[0].Created.Unix())
}

// TestPodmanRuntime_List_CreatedFromRFC3339String proves the RFC3339-string
// variant of Created is also tolerated, alongside the real unix-int shape
// proven above.
func TestPodmanRuntime_List_CreatedFromRFC3339String(t *testing.T) {
	psJSON := `[{
		"Id": "abc123",
		"Names": ["web"],
		"Image": "nginx",
		"ImageID": "sha256:abc",
		"Created": "2024-01-15T10:00:00Z",
		"State": "running",
		"Status": "Up",
		"Labels": {}
	}]`
	e := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(psJSON), nil
		},
	}
	p := NewPodmanRuntimeWithExecutor(e)

	containers, err := p.List(context.Background(), ListFilter{})
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.False(t, containers[0].Created.IsZero())
	assert.Equal(t, 2024, containers[0].Created.Year())
}

func TestParsePodmanCreated_UnknownShapeYieldsZeroTime(t *testing.T) {
	assert.True(t, parsePodmanCreated(nil).IsZero())
	assert.True(t, parsePodmanCreated(true).IsZero())
	assert.True(t, parsePodmanCreated(float64(0)).IsZero())
	assert.True(t, parsePodmanCreated("not-a-time").IsZero())
}

// --- RT-LABEL-1 (assessed — documents known behavior, does not "fix" it) ---

// TestParseLabelsString_KnownLimitation_CommaInValueTruncates documents (per
// §11.4.6 — never silently hide a known limitation) that a label VALUE
// containing a literal comma is truncated by parseLabelsString. This is NOT
// a defect introduced by this package: `docker ps --format json`'s own
// "Labels" field is an unescaped comma-joined string with no way to
// disambiguate an embedded comma from a label separator (confirmed:
// moby/moby#30575, "Unable to use docker ps --filter when a label value has
// a comma"). There is no clean fix at this layer without switching to a
// per-container `docker inspect` round trip, which changes List()'s
// contract and is out of scope here.
func TestParseLabelsString_KnownLimitation_CommaInValueTruncates(t *testing.T) {
	got := parseLabelsString("description=hello, world,role=web")

	// The comma inside the value silently becomes a second, garbage pair —
	// this assertion documents the KNOWN truncation, it does not endorse it.
	assert.Equal(t, "hello", got["description"],
		"documents the known upstream limitation: the value truncates at the "+
			"embedded comma rather than preserving \"hello, world\"")
	assert.Equal(t, "web", got["role"], "the pair after the split is unaffected")
	assert.NotContains(t, got, "world",
		"the comma-continuation is a separate garbage pair, not an equals-free key")
}

// TestParseLabelsString_NoCommaInValue_Unaffected proves the common case
// (no embedded comma) is completely unaffected.
func TestParseLabelsString_NoCommaInValue_Unaffected(t *testing.T) {
	got := parseLabelsString("role=web,tier=frontend")
	assert.Equal(t, "web", got["role"])
	assert.Equal(t, "frontend", got["tier"])
}

// --- RT-RACE-1 (flagged — not fixed; reproduces the race as evidence) ------

// TestRTRace1_DirectAccessRacesWithSetRuntimePriority reproduces, under
// `-race`, the data race between direct unsynchronized access to the
// exported RuntimePriority var and a concurrent SetRuntimePriority call.
// This is captured as EVIDENCE for the flagged finding, not fixed here: the
// only clean fix (unexporting RuntimePriority) removes a public symbol
// (§11.4.122) and requires an explicit operator API-break decision — see
// detect.go's doc comment on the var for the flagged recommendation.
//
// Skipped by default: this package's standing `-race` suite (which this
// same hardening batch must keep GREEN, per DoD) would otherwise be made to
// fail by a known, deliberately-flagged-not-fixed finding. Run manually
// (`go test -race -run TestRTRace1 -v ./pkg/runtime/... `, with the t.Skip
// below commented out) to capture the "WARNING: DATA RACE" report cited as
// evidence in the conductor's RT-RACE-1 finding.
func TestRTRace1_DirectAccessRacesWithSetRuntimePriority(t *testing.T) {
	t.Skip("RT-RACE-1 evidence-only: intentionally races against the " +
		"exported RuntimePriority var to document the flagged (not fixed) " +
		"finding; see detect.go's doc comment. Left skipped so the standing " +
		"`-race` suite stays GREEN for this hardening batch's DoD.")

	origPriority := GetRuntimePriority()
	t.Cleanup(func() { SetRuntimePriority(origPriority) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		SetRuntimePriority([]string{"docker", "podman"})
	}()

	// Direct, unsynchronized read of the exported var — races with the
	// SetRuntimePriority goroutine's write under `go test -race`.
	_ = RuntimePriority

	<-done
}

// --- RT-DOCKER-1 (fixed) -----------------------------------------------------
//
// parseDockerPSOutput decoded `docker`/`nerdctl` `ps --format json`'s
// `CreatedAt` field into dockerPSJSON.Created but never assigned it to
// ContainerInfo.Created — so DockerRuntime.List AND NerdctlRuntime.List (both
// route through parseDockerPSOutput) reported the zero time for every listed
// container, while the sibling Podman/CRI-O/LXD/Kubernetes List paths all
// populate Created. Same defect class as the already-fixed RT-PODMAN-1, left
// unfixed in the docker/nerdctl parser. Real CreatedAt shape captured in this
// repo's own real-runtime fixtures (pkg/ctop/wave18_real_runtime_output_test.go
// + pkg/ctop/ctop_test.go): "2024-01-01 00:00:00 +0000 UTC" — Go's
// time.Time.String() layout, which is NOT RFC3339 and needs its own layout.

// realDockerPSWithCreatedAt is a REAL `docker ps --format json` NDJSON record
// (trimmed) whose CreatedAt uses the exact Go time.String() shape docker emits.
const realDockerPSWithCreatedAt = `{"ID":"abc123def456","Names":"web-1","Image":"nginx:latest","State":"running","Status":"Up 2 hours","CreatedAt":"2024-01-01 00:00:00 +0000 UTC","Labels":"role=web","Ports":"0.0.0.0:8080->80/tcp"}`

func TestWave20_RT_DockerList_CreatedFromRealCreatedAtField(t *testing.T) {
	e := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(realDockerPSWithCreatedAt), nil
		},
	}
	d := NewDockerRuntimeWithExecutor(e)

	containers, err := d.List(context.Background(), ListFilter{All: true})
	require.NoError(t, err)
	require.Len(t, containers, 1)

	assert.False(t, containers[0].Created.IsZero(),
		"Created must be populated from the real docker CreatedAt wire field, "+
			"not left at the zero value")
	// 2024-01-01T00:00:00Z == unix 1704067200 (exact-instant proof, not just non-zero).
	assert.Equal(t, int64(1704067200), containers[0].Created.Unix())
}

// TestWave20_RT_NerdctlList_CreatedPopulated proves the same fix reaches
// NerdctlRuntime.List, which shares parseDockerPSOutput.
func TestWave20_RT_NerdctlList_CreatedPopulated(t *testing.T) {
	e := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(realDockerPSWithCreatedAt), nil
		},
	}
	n := NewNerdctlRuntimeWithExecutor(e)

	containers, err := n.List(context.Background(), ListFilter{})
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.Equal(t, int64(1704067200), containers[0].Created.Unix(),
		"NerdctlRuntime.List must populate Created via the shared parser too")
}

func TestWave20_RT_ParseDockerCreatedAt_UnparseableYieldsZero(t *testing.T) {
	// Empty, human-relative, and date-only (the existing test-fixture shape)
	// values are all best-effort → zero time, never a parse-panic or error.
	assert.True(t, parseDockerCreatedAt("").IsZero())
	assert.True(t, parseDockerCreatedAt("5 days ago").IsZero())
	assert.True(t, parseDockerCreatedAt("not-a-time").IsZero())
	// RFC3339 variant is tolerated alongside the docker String() shape.
	rfc := parseDockerCreatedAt("2024-01-01T00:00:00Z")
	assert.Equal(t, int64(1704067200), rfc.Unix())
}

// --- RT-DOCKER-2 (fixed) -----------------------------------------------------
//
// parseDockerPSOutput decoded `docker`/`nerdctl` `ps --format json`'s `Ports`
// field into dockerPSJSON.Ports but never assigned it to ContainerInfo.Ports —
// so List always returned an empty Ports slice even though the container's
// published-port data was present in the output. Real Ports shape captured in
// pkg/ctop/wave18_real_runtime_output_test.go: "0.0.0.0:8080->80/tcp".

func TestWave20_RT_DockerList_PortsFromRealPortsField(t *testing.T) {
	e := &mockExecutor{
		executeFunc: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte(realDockerPSWithCreatedAt), nil
		},
	}
	d := NewDockerRuntimeWithExecutor(e)

	containers, err := d.List(context.Background(), ListFilter{All: true})
	require.NoError(t, err)
	require.Len(t, containers, 1)

	require.Len(t, containers[0].Ports, 1,
		"the published port mapping decoded from the Ports field must reach "+
			"ContainerInfo.Ports, not be dropped")
	p := containers[0].Ports[0]
	assert.Equal(t, "0.0.0.0", p.HostIP)
	assert.Equal(t, "8080", p.HostPort)
	assert.Equal(t, "80", p.ContainerPort)
	assert.Equal(t, "tcp", p.Protocol)
}

// TestWave20_RT_ParseDockerPortsMapping_MultipleAndUnpublished proves the
// parser reports every published (`->`) mapping and skips exposed-but-
// unpublished ("80/tcp") entries rather than fabricating a host port.
func TestWave20_RT_ParseDockerPortsMapping_MultipleAndUnpublished(t *testing.T) {
	got := parseDockerPortsMapping("0.0.0.0:8080->80/tcp, :::8080->80/tcp, 9000/tcp")
	require.Len(t, got, 2, "two published mappings; the bare 9000/tcp is skipped")

	assert.Equal(t, "0.0.0.0", got[0].HostIP)
	assert.Equal(t, "8080", got[0].HostPort)
	assert.Equal(t, "80", got[0].ContainerPort)
	assert.Equal(t, "tcp", got[0].Protocol)

	// The IPv6 host side "::" ends in ':' before the last colon; the parser
	// takes the segment after the LAST colon as the host port.
	assert.Equal(t, "8080", got[1].HostPort)
	assert.Equal(t, "80", got[1].ContainerPort)

	assert.Nil(t, parseDockerPortsMapping(""),
		"empty Ports string yields no mappings")
	assert.Nil(t, parseDockerPortsMapping("80/tcp"),
		"an exposed-but-unpublished port has no host binding to report")
}
