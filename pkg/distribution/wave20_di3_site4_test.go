// SPDX-License-Identifier: Apache-2.0
package distribution

// Wave-20 DI3-SITE4-ARGSWEEP permanent regression guard (§11.4.115 GREEN
// polarity) for the §11.4.108 injection inconsistency the cross-cutting
// ARGSWEEP audit surfaced in pkg/distribution/distributor.go: BOTH remote
// command builders interpolate rt (= host.Runtime, an unvalidated
// CONTAINERS_REMOTE_HOST_N_RUNTIME env-config value) into a string that
// ssh_executor.Execute hands to the REMOTE login shell verbatim
// (`ssh host <cmd>` re-parses it). The pre-deploy `rm -f` builder was fixed to
// shellQuote(rt), but the `run -d` builder still spliced rt RAW while quoting
// name+image — so a host.Runtime like `docker;touch /pwn` injected a second
// remote command. Both builders must shell-quote rt.
//
// Anti-tautology anchors (revert either → that builder emits the raw,
// shell-active runtime token → RED; restore → GREEN):
//   remove: `"%s rm -f %s 2>/dev/null || true",\n\t\tshellQuote(rt), ...`
//   run:    `"%s run -d --name %s%s %s",\n\t\tshellQuote(rt), ...`

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWave20_DI3SITE4_RemoteCommandBuildersQuoteRuntime(t *testing.T) {
	// A runtime token crafted to inject a second remote command if spliced raw.
	const inj = "docker;touch /tmp/di3_pwn"
	quoted := shellQuote(inj)
	require.NotEqual(t, inj, quoted, "sanity: the injection runtime must require quoting")

	t.Run("buildRemoteRemoveCommand", func(t *testing.T) {
		cmd := buildRemoteRemoveCommand(inj, "web")
		require.Contains(t, cmd, quoted,
			"remove builder must shell-quote the runtime token (§11.4.108 injection); got %q", cmd)
		require.False(t, strings.HasPrefix(cmd, inj+" "),
			"remove builder must not splice the raw runtime as the leading remote-shell token; got %q", cmd)
	})

	t.Run("buildRemoteRunCommand", func(t *testing.T) {
		cmd := buildRemoteRunCommand(inj, "web", "nginx:latest", nil)
		require.Contains(t, cmd, quoted,
			"run builder must shell-quote the runtime token (§11.4.108 injection); got %q", cmd)
		require.False(t, strings.HasPrefix(cmd, inj+" "),
			"run builder must not splice the raw runtime as the leading remote-shell token; got %q", cmd)
	})
}
