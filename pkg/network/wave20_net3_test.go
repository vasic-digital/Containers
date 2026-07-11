package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// This file covers the Wave-20 NET3 network security batch: the tunnel ssh
// destination argument-injection guard (sibling of pkg/egress §EG2-1). Each
// guard below asserts the FIXED behavior (§11.4.115 GREEN polarity); the
// accompanying doc comment states the surgical revert that reproduces RED.

// --- NET3: refuse a '-'-prefixed ssh tunnel destination -------------------

// TestWave20_NET3_RefusesDashPrefixedDestination proves CreateTunnel refuses a
// tunnel whose composed destination ("<user>@<address>") begins with '-' and
// NEVER spawns ssh for it. The tunnel destination is appended as the FINAL
// positional argv element with no "--" guard, so a host.User beginning with '-'
// (e.g. "-oProxyCommand=<cmd>") is parsed by ssh's own getopt as an OPTION —
// arbitrary command execution — exactly like the pkg/egress §EG2-1 vector.
//
// This is a spawn-count spy: the fake ssh appends one line per invocation, so a
// guard that refuses BEFORE spawning leaves that log absent (ssh spawn count 0).
//
// §11.4.115 RED/GREEN: PASSes on the fixed tree. A surgical revert of the
// single-line guard condition in CreateTunnel to `if false &&
// strings.HasPrefix(dest, "-")` (keeps `dest`/`strings` referenced so the
// package still builds) reproduces RED — ssh IS spawned for the '-'-prefixed
// destination, the spawn log appears, and require.Never fails.
func TestWave20_NET3_RefusesDashPrefixedDestination(t *testing.T) {
	dir := t.TempDir()
	spawnLog := filepath.Join(dir, "spawns.log")

	// Spawn-count spy: every ssh invocation appends "SPAWN". A destination
	// refused before the spawn leaves this file absent. The fake ignores its
	// own argv (it does NOT execute any injected ProxyCommand) — hermetic.
	writeFakeSSH(t, fmt.Sprintf("printf 'SPAWN\\n' >> %s\nsleep 5", spawnLog))

	// host.User begins with '-', so tunnelDestination(host) == the composed
	// "-oProxyCommand=touch /tmp/pwned@10.0.0.9" begins with '-'.
	hm := &mockHostManager{hosts: map[string]remote.RemoteHost{
		"evil": {
			Name:    "evil",
			Address: "10.0.0.9",
			User:    "-oProxyCommand=touch /tmp/pwned",
			Port:    22,
		},
	}}
	mgr := NewTunnelManager(hm, logging.NopLogger{}, WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "evil", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "18091",
		RemotePort: "5432",
	})
	require.Error(t, err,
		"a tunnel destination beginning with '-' must be refused (argument "+
			"injection into ssh getopt)")
	require.Nil(t, info)
	assert.Contains(t, err.Error(), "begins with '-'",
		"the refusal error must name the leading-dash cause")

	// Spawn-count spy: ssh must have been spawned ZERO times. Give any (buggy)
	// async spawn a window to create the log, then assert it never appears.
	require.Never(t, func() bool {
		_, statErr := os.Stat(spawnLog)
		return statErr == nil
	}, 300*time.Millisecond, 30*time.Millisecond,
		"ssh must NEVER be spawned for a '-'-prefixed destination "+
			"(spawn count must stay 0)")
}

// TestWave20_NET3_BenignDestinationReachesSpawn is the anti-tautology positive
// control: a benign "user@host" destination (no leading '-') must NOT be
// rejected by the guard — CreateTunnel proceeds to spawn ssh. This proves the
// guard discriminates on the leading '-' rather than blanket-refusing every
// destination (a guard that rejected everything would pass the negative test
// above for the wrong reason).
//
// §11.4.115: PASSes on BOTH the fixed tree AND the guard-neutralized tree — it
// is the discriminator, not the RED flip. Its job is to fail if the guard is
// ever written to over-reject.
func TestWave20_NET3_BenignDestinationReachesSpawn(t *testing.T) {
	dir := t.TempDir()
	spawnLog := filepath.Join(dir, "spawns.log")
	writeFakeSSH(t, fmt.Sprintf("printf 'SPAWN\\n' >> %s\nsleep 5", spawnLog))

	// wave17TunnelHost(): User "deploy", Address "10.0.0.1" → "deploy@10.0.0.1"
	// (no leading '-'), so the guard must not fire.
	mgr := NewTunnelManager(wave17TunnelHost(), logging.NopLogger{},
		WithPortRange(25000, 26000))
	t.Cleanup(func() { _ = mgr.CloseAll() })

	info, err := mgr.CreateTunnel(context.Background(), "gpu-1", TunnelSpec{
		Direction:  TunnelLocal,
		LocalPort:  "18092",
		RemotePort: "5432",
	})
	require.NoError(t, err)
	require.NotNil(t, info)

	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(spawnLog)
		return rerr == nil && strings.Contains(string(data), "SPAWN")
	}, 3*time.Second, 10*time.Millisecond,
		"a benign 'user@host' destination must reach the ssh spawn "+
			"(the guard must not reject it)")
}
