// SPDX-License-Identifier: Apache-2.0
package ctop

// Wave-20 CT3-ARGSWEEP permanent regression guard (§11.4.115 GREEN polarity)
// for the §11.4.108 injection the cross-cutting ARGSWEEP audit surfaced in
// pkg/ctop/collector.go: both remote container-monitoring commands splice rt
// (= host.Runtime, an unvalidated CONTAINERS_REMOTE_HOST_N_RUNTIME env-config
// value) into a string handed to sshExecutor.Execute, which runs it through the
// REMOTE login shell with NO argv escaping. The CT3-9 fix validated the dynamic
// container id but left rt RAW in BOTH the `ps` and `stats` commands — so a
// host.Runtime like `docker;evilcmd` injected a second remote command. Both
// builders must shell-quote rt.
//
// Anti-tautology anchors (revert either → the built command carries the raw,
// shell-active runtime token → RED; restore → GREEN):
//   list:  `"%s ps -a --format json", shellQuote(rt)` → `..., rt`
//   stats: `"%s stats --no-stream --format json %s", shellQuote(rt), id` → `..., rt, id`

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWave20_CT3ARGSWEEP_RemoteCommandsQuoteRuntime(t *testing.T) {
	const inj = "docker;touch /tmp/ct3_pwn"
	q := shellQuote(inj)
	require.NotEqual(t, inj, q, "sanity: the injection runtime must require quoting")

	t.Run("buildRemoteListCommand", func(t *testing.T) {
		cmd := buildRemoteListCommand(inj)
		require.Contains(t, cmd, q,
			"ps builder must shell-quote the runtime token (§11.4.108 injection); got %q", cmd)
		require.False(t, strings.HasPrefix(cmd, inj+" "),
			"ps builder must not splice the raw runtime as the leading remote-shell token; got %q", cmd)
	})

	t.Run("buildRemoteStatsCommand", func(t *testing.T) {
		cmd, err := buildRemoteStatsCommand(inj, "abc123def456")
		require.NoError(t, err)
		require.Contains(t, cmd, q,
			"stats builder must shell-quote the runtime token (§11.4.108 injection); got %q", cmd)
		require.False(t, strings.HasPrefix(cmd, inj+" "),
			"stats builder must not splice the raw runtime as the leading remote-shell token; got %q", cmd)
	})

	t.Run("CT3-9 id validation preserved", func(t *testing.T) {
		_, err := buildRemoteStatsCommand("docker", "bad;id$(x)")
		require.Error(t, err, "CT3-9: an unsafe container id must still be refused (not silently quoted)")
	})
}
