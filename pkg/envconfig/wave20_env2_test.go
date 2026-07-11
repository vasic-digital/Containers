package envconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wave-20 DEEPER (§11.4.118 discovery-pressure) 2nd-pass env-config guard
// suite. §11.4.115 GREEN-polarity regression guards; committed default =
// GUARD. Pure parseDotEnvLine seam — no external infrastructure (§11.4.27,
// §11.4.107 honest boundary: proves the dotenv parse LOGIC, not a live SSH
// connection). §11.4.10: every fixture uses a neutral placeholder token,
// never a real secret.

// GUARD (ENV2-1 / §11.4.10): a QUOTED value followed by an inline comment
// (e.g. PASSWORD="s3cr3t" # note) must strip BOTH the surrounding quotes AND
// the trailing comment — it must NOT retain the literal quotes (which would
// corrupt an SSH password / key into a wrong credential that authenticates
// nowhere). This is the quoted-path sibling of the ENV-4 unquoted-'#' fix
// that the first pass missed. Before the fix the closing quote is no longer
// the last character, so the both-ends-quoted strip is skipped and the value
// keeps its literal quotes; when the trailing comment itself ends in a quote,
// the naive both-ends test even MISFIRES and embeds the comment text into the
// credential.
func TestWave20_ENV2_QuotedSecretWithInlineCommentNotCorrupted(t *testing.T) {
	// Case B — double-quoted secret with a trailing comment. Neutral token.
	_, v, ok := parseDotEnvLine(
		`CONTAINERS_REMOTE_DEFAULT_SSH_PASSWORD="s3cr3t-tok" # prod`,
	)
	require.True(t, ok)
	assert.Equal(t, "s3cr3t-tok", v,
		`quoted value + " # comment" must strip BOTH quotes and the comment, `+
			`not keep the literal surrounding quotes (§11.4.10)`)

	// Case B — single-quoted variant.
	_, v, ok = parseDotEnvLine(`K='p@ss-val' # note`)
	require.True(t, ok)
	assert.Equal(t, "p@ss-val", v,
		"single-quoted value + comment must strip the single quotes")

	// Case C — the comment itself ends in a quote. The naive
	// first-char-and-last-char-are-quotes test would fire and embed the
	// comment into the value; the fix must still yield only the interior.
	_, v, ok = parseDotEnvLine(`K="tok3n" # see "vault"`)
	require.True(t, ok)
	assert.Equal(t, "tok3n", v,
		"a comment ending in a quote must not fool the both-ends strip "+
			"into embedding the comment text into the credential")

	// --- Preservation guards: the fix must NOT regress existing behaviour. ---

	// A '#' INSIDE the quotes is part of the value (ENV-4 quoted case).
	_, v, ok = parseDotEnvLine(`K="a#b"`)
	require.True(t, ok)
	assert.Equal(t, "a#b", v,
		"'#' inside quotes must stay part of the value")

	// A plain quoted value with an interior space is unchanged.
	_, v, ok = parseDotEnvLine(`K="hello world"`)
	require.True(t, ok)
	assert.Equal(t, "hello world", v)

	// An unquoted '#' with no preceding whitespace stays in the value (ENV-4).
	_, v, ok = parseDotEnvLine("K=a#b")
	require.True(t, ok)
	assert.Equal(t, "a#b", v)

	// An unquoted value with a whitespace-separated inline comment is trimmed.
	_, v, ok = parseDotEnvLine("K=value # comment")
	require.True(t, ok)
	assert.Equal(t, "value", v)
}
