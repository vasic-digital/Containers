// SPDX-License-Identifier: Apache-2.0
package brokertest

// Wave-20 SITE-10a-ARGSWEEP permanent regression guard (§11.4.115 GREEN
// polarity): StartNATS/StartPostgres/StartRedis build a `run` args slice with
// the image as a positional and execute it via argv (exec.CommandContext, no
// shell). A leading-dash image supplied through WithImage would be parsed by
// docker/podman run's getopt as a FLAG (argv flag-injection). appendImagePositional
// prepends the end-of-options `--` so the image is always the IMAGE positional.
//
// Anti-tautology anchor: `return append(args, "--", image)` → `return
// append(args, image)` drops the delimiter → RED; restore → GREEN.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWave20_SITE10A_AppendImagePositionalDelimitsImage(t *testing.T) {
	got := appendImagePositional([]string{"run", "-d", "--name", "x"}, "-oEvilFlag")
	require.Equal(t, []string{"run", "-d", "--name", "x", "--", "-oEvilFlag"}, got)
	require.Equal(t, "--", got[len(got)-2],
		"the -- delimiter must immediately precede the image")
	require.Equal(t, "-oEvilFlag", got[len(got)-1],
		"the image must be the final positional (any container command follows AFTER it)")

	// A benign image is likewise placed after the delimiter, unchanged.
	b := appendImagePositional([]string{"run"}, "docker.io/library/nats:2.10-alpine")
	require.Equal(t, []string{"run", "--", "docker.io/library/nats:2.10-alpine"}, b)
}
