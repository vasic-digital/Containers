package envconfig

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wave-20 batch CT-HARDEN-ENV-HARD guard suite (§11.4.115 GREEN-polarity
// regression guards; committed default = GUARD). These guards exercise the
// pure parse / validation / error-surfacing LOGIC through t.Setenv fixtures
// and the pure parseDotEnvLine / parseLabels seams — no external
// infrastructure (§11.4.27, §11.4.107 honest boundary: they prove the
// loader logic, not a live SSH connection). Validation errors are surfaced
// through Parse() and LoadFromFile() (both already return an error);
// LoadFromEnv() is the intentionally-unvalidated convenience wrapper whose
// signature must not change (non-breaking for its cmd/* direct callers).

// isolateHosts clears any leaked host env so a test observes exactly the
// hosts it declares (deterministic + re-runnable, §11.4.98 / §11.4.50).
func isolateHosts(t *testing.T) {
	t.Helper()
	for n := 1; n <= 3; n++ {
		p := prefix + "HOST_" + itoa(n) + "_"
		t.Setenv(p+"NAME", "")
		t.Setenv(p+"ADDRESS", "")
		t.Setenv(p+"PORT", "")
	}
}

func itoa(n int) string {
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	return "0"
}

// ---- ENV-1: envBool tri-state (extended vocab applied; invalid surfaced) ----

// GUARD: extended boolean vocabulary is APPLIED (not silently dropped to
// the fallback). ENABLED=yes MUST yield Enabled==true; SSH_CONTROL_MASTER=off
// MUST yield ControlMasterEnabled==false (fallback is true).
func TestWave20_ENV1_BoolExtendedVocabApplied(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"ENABLED", "yes")
	t.Setenv(prefix+"SSH_CONTROL_MASTER", "off")

	cfg, err := Parse()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled,
		"ENABLED=yes must parse to true, not silent fallback false")
	assert.False(t, cfg.ControlMasterEnabled,
		"SSH_CONTROL_MASTER=off must disable, not stay on via fallback")

	// Pure-helper coverage of the full accepted vocabulary.
	for _, v := range []string{"1", "t", "true", "yes", "y", "on", "enabled"} {
		got, ok := parseBoolExtended(v)
		assert.True(t, ok && got, "%q should parse true", v)
	}
	for _, v := range []string{"0", "f", "false", "no", "n", "off", "disabled"} {
		got, ok := parseBoolExtended(v)
		assert.True(t, ok && !got, "%q should parse false", v)
	}
}

// GUARD: a SET-but-unrecognised boolean (e.g. the typo "ture") is surfaced
// as an error through Parse(), never silently swallowed to the fallback.
func TestWave20_ENV1_InvalidBoolSurfacesError(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"ENABLED", "ture")

	_, err := Parse()
	require.Error(t, err, "ENABLED=ture must surface an error, not fallback")
	assert.Contains(t, err.Error(), "ENABLED")
}

func TestWave20_ENV1_InvalidControlMasterSurfacesError(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"SSH_CONTROL_MASTER", "sometimes")

	_, err := Parse()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSH_CONTROL_MASTER")
}

// ---- ENV-2: per-host required-field validation ----

// GUARD: a host with a NAME but no ADDRESS is rejected with a surfaced
// error, not loaded as a silent Address=="" broken host.
func TestWave20_ENV2_HostNameWithoutAddressErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"HOST_1_NAME", "solo")
	t.Setenv(prefix+"HOST_1_ADDRESS", "") // explicitly empty

	_, err := Parse()
	require.Error(t, err,
		"HOST_1_NAME set with empty ADDRESS must error, not load broken")
	assert.Contains(t, err.Error(), "HOST_1_ADDRESS")
}

// GUARD: a malformed host PORT ("22a") is rejected, not silently coerced
// (envInt fallback 0 → types.ToRemoteHosts 0→22) which masks the typo.
func TestWave20_ENV2_HostMalformedPortErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"HOST_1_NAME", "h1")
	t.Setenv(prefix+"HOST_1_ADDRESS", "10.0.0.9")
	t.Setenv(prefix+"HOST_1_PORT", "22a")

	_, err := Parse()
	require.Error(t, err, "HOST_1_PORT=22a must error, not coerce to 22")
	assert.Contains(t, err.Error(), "HOST_1_PORT")
}

// GUARD: a fully-formed host still loads cleanly (no false-positive).
func TestWave20_ENV2_ValidHostLoads(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"HOST_1_NAME", "h1")
	t.Setenv(prefix+"HOST_1_ADDRESS", "10.0.0.9")
	t.Setenv(prefix+"HOST_1_PORT", "2222")

	cfg, err := Parse()
	require.NoError(t, err)
	require.Len(t, cfg.Hosts, 1)
	assert.Equal(t, 2222, cfg.Hosts[0].Port)
}

// ---- ENV-3: envInt tri-state + range validation ----

func TestWave20_ENV3_MalformedIntSurfacesError(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"CONNECT_TIMEOUT", "abc")

	_, err := Parse()
	require.Error(t, err, "CONNECT_TIMEOUT=abc must error, not fallback")
	assert.Contains(t, err.Error(), "CONNECT_TIMEOUT")
}

// GUARD: a zero (or non-positive) timeout is rejected — 0 is unbounded /
// zero downstream in pkg/remote and must not be accepted silently.
func TestWave20_ENV3_ZeroTimeoutErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"CONNECT_TIMEOUT", "0")

	_, err := Parse()
	require.Error(t, err, "CONNECT_TIMEOUT=0 must error")
	assert.Contains(t, err.Error(), "CONNECT_TIMEOUT")

	t.Setenv(prefix+"CONNECT_TIMEOUT", "10") // restore in-range
	t.Setenv(prefix+"COMMAND_TIMEOUT", "0")
	_, err = Parse()
	require.Error(t, err, "COMMAND_TIMEOUT=0 must error")
	assert.Contains(t, err.Error(), "COMMAND_TIMEOUT")
}

// GUARD: an inverted port range (start > end) is rejected.
func TestWave20_ENV3_InvertedPortRangeErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"PORT_RANGE_START", "30000")
	t.Setenv(prefix+"PORT_RANGE_END", "20000")

	_, err := Parse()
	require.Error(t, err, "PORT_RANGE_START>END must error")
	assert.Contains(t, err.Error(), "PORT_RANGE_START")
}

// GUARD: SSH_MAX_CONNECTIONS must be >= 1 (0 / negative rejected).
func TestWave20_ENV3_MaxConnectionsFloorErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"SSH_MAX_CONNECTIONS", "0")

	_, err := Parse()
	require.Error(t, err, "SSH_MAX_CONNECTIONS=0 must error")
	assert.Contains(t, err.Error(), "SSH_MAX_CONNECTIONS")
}

// GUARD: an out-of-range port is rejected.
func TestWave20_ENV3_PortRangeOutOfBoundsErrors(t *testing.T) {
	isolateHosts(t)
	t.Setenv(prefix+"PORT_RANGE_END", "70000")

	_, err := Parse()
	require.Error(t, err, "PORT_RANGE_END=70000 must error")
	assert.Contains(t, err.Error(), "PORT_RANGE_END")
}

// ---- ENV-4: dotenv inline-comment must require whitespace before '#' ----

// GUARD: '#' inside an unquoted value (no preceding whitespace) is
// preserved — a genuine inline comment needs " #". §11.4.10: the fixture
// uses a neutral token "a#b", never a real secret.
func TestWave20_ENV4_HashInUnquotedValuePreserved(t *testing.T) {
	_, v, ok := parseDotEnvLine("K=a#b")
	require.True(t, ok)
	assert.Equal(t, "a#b", v,
		"'#' with no preceding whitespace is part of the value, not a comment")

	_, v, ok = parseDotEnvLine("K=a #b")
	require.True(t, ok)
	assert.Equal(t, "a", v,
		"' #' (whitespace before '#') IS an inline comment")

	// Existing convention still holds: whitespace-separated comment.
	_, v, ok = parseDotEnvLine("K=value # this is a comment")
	require.True(t, ok)
	assert.Equal(t, "value", v)

	// Quoted values are returned verbatim (comment logic not reached).
	_, v, ok = parseDotEnvLine(`K="a#b"`)
	require.True(t, ok)
	assert.Equal(t, "a#b", v)
}

// ---- ENV-5: parseLabels must not mint an empty-key label ----

func TestWave20_ENV5_EmptyKeyLabelSkipped(t *testing.T) {
	got := parseLabels("=true")
	_, has := got[""]
	assert.False(t, has, `parseLabels("=true") must not create a "" key`)

	got = parseLabels("gpu=1,=x,arch=amd64")
	_, has = got[""]
	assert.False(t, has, "an empty-key pair in a list must be skipped")
	assert.Equal(t, "1", got["gpu"])
	assert.Equal(t, "amd64", got["arch"])
	assert.Len(t, got, 2)
}

// Guard against accidental secret echo in validation messages (§11.4.10):
// a malformed-int error must not contain any password/key value.
func TestWave20_NoSecretEchoInErrors(t *testing.T) {
	isolateHosts(t)
	const secret = "sup3r-s3cret-should-never-appear"
	t.Setenv(prefix+"DEFAULT_SSH_PASSWORD", secret)
	t.Setenv(prefix+"CONNECT_TIMEOUT", "abc") // force a validation error

	_, err := Parse()
	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), secret),
		"validation error must never echo credential material")
}
