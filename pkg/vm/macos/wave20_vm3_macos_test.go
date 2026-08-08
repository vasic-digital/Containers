// SPDX-License-Identifier: Apache-2.0
package macos

// Wave-20 VM3-9-ARGSWEEP permanent regression guard (§11.4.115 GREEN polarity):
// osExecTartRunner.SSHExec composes the ssh destination positional "<user>@<ip>".
// A user beginning with '-' (e.g. "-oProxyCommand=<cmd>") makes the whole
// destination begin with '-', which ssh's getopt parses as an OPTION rather
// than a host → ProxyCommand arbitrary command execution on the Tart host.
// SSHExec must refuse a leading-dash user BEFORE any spawn.
//
// Anti-tautology anchor: `if strings.HasPrefix(user, "-") {` — disabling it
// (`if false && …`) lets the poison user fall through to `tart ip`, whose error
// does NOT mention the guard, flipping the test RED; restore → GREEN.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWave20_VM3_9_SSHExecRefusesLeadingDashUser(t *testing.T) {
	r := &osExecTartRunner{}

	_, _, code, err := r.SSHExec(
		context.Background(), "vm",
		"-oProxyCommand=touch /tmp/pwn", "admin", "echo ok", time.Second)
	require.Error(t, err, "SSHExec must refuse a leading-dash ssh user (ProxyCommand RCE)")
	require.Contains(t, err.Error(), "begins with '-'",
		"the refusal must come from the VM3-9 user guard, not a later tart failure")
	require.Equal(t, -1, code, "a refused SSHExec must report exit -1 (never spawned)")

	// Benign positive control: a normal user passes the guard and fails LATER
	// (at `tart ip`, because Tart is absent on this host) — a DIFFERENT error
	// that must NOT mention the guard. Bound the context so a Mac-with-tart host
	// cannot hang on `tart ip --wait 60`.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err2 := r.SSHExec(ctx, "vm", "admin", "admin", "echo ok", time.Second)
	require.Error(t, err2)
	require.False(t, strings.Contains(err2.Error(), "begins with '-'"),
		"a benign user must pass the guard and fail later at tart ip, got: %v", err2)
}
