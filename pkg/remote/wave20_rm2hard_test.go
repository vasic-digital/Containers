package remote

// Wave-20 REMOTE2-HARD (second-pass, SECURITY lens) permanent regression
// guards (§11.4.115 GREEN polarity). These assert the FIXED behavior of
// two NEW read-only audit findings the first pkg/remote pass (REMOTE-1/2 +
// wave18 CT-HARDEN + wave20 REMOTE-HIGH-1..3 / REMOTE-MED-1..4) did NOT
// cover:
//
//   - RM2-1 (compose.go — remote-shell command injection): Up/Down/Status/
//     Logs build the command STRING they pass to executor.Execute, which
//     runs `ssh <host> <cmd>` so the REMOTE login shell re-parses it. The
//     caller-controlled compose fields (project.File / Name / Profile via
//     projectArgs, the service list in Up, the project.Name label filter in
//     Status, and the service name in Logs) were spliced in UNESCAPED —
//     while the sibling runtime.go (wave17) already shell-escapes every
//     caller value it splices. A value like "x; touch /tmp/pwn" injected a
//     second command running with the SSH user's privileges. Guarded by
//     TestWave20_RM2_ComposeShellEscapesCallerValues. Each subtest's paired
//     RED reproduction (revert the one shellEscape() at that splice site,
//     re-run the subtest, observe `--- FAIL`, restore) is recorded in the
//     anti-tautology run log in the stream report rather than committed as
//     a live broken test.
//
//   - RM2-2 (connection_pool.go — ControlMaster socket path missing the SSH
//     user): the pool KEY (hostKey) is user@address:port, but the socket
//     path was keyed ONLY by address+port. Two configured hosts sharing an
//     address:port but differing in user (deploy@gpu1:22 vs root@gpu1:22)
//     got distinct pool entries yet the SAME socket file; the second host's
//     `-fNM` master dial collided with the first's live socket, so it could
//     never pool and thrashed on every Acquire — and the code contradicted
//     its own CLAUDE.md contract ("one socket per (user@host:port)").
//     Guarded by TestWave20_RM2_ControlSocketPathIncludesUser. RED
//     reproduction: revert controlSocketPath's format to
//     `fmt.Sprintf("ctrl-%s-%d", host.Address, host.SSHPort())` → the two
//     distinct-user hosts collide onto one path and the guard FAILs.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/compose"
	"digital.vasic.containers/pkg/logging"
)

// -----------------------------------------------------------------------------
// RM2-1 — compose.go shell-escapes every caller-controlled remote-command value
// -----------------------------------------------------------------------------

// TestWave20_RM2_ComposeShellEscapesCallerValues is the permanent RM2-1
// guard. Each subtest drives one compose method with a shell-metacharacter-
// bearing caller value at a distinct splice site and asserts the captured
// remote command carries the SINGLE-QUOTE-ESCAPED form (not the raw,
// shell-active value). `inj` requires escaping, so pre-fix (raw splice) the
// captured command contains `inj` but NOT shellEscape(inj); post-fix it
// contains shellEscape(inj). Each subtest hinges on exactly one source line
// (the shellEscape() call at that site), giving a clean anti-tautology
// anchor per §1.1.
func TestWave20_RM2_ComposeShellEscapesCallerValues(t *testing.T) {
	const inj = "x; touch /tmp/pwn"
	escaped := shellEscape(inj)
	require.NotEqual(t, inj, escaped,
		"sanity: the injection value must require shell escaping")

	host := RemoteHost{
		Name: "h", Address: "10.0.0.1", User: "deploy", Runtime: "docker",
	}

	// newOrch builds an orchestrator whose executor captures the exact
	// command string handed to it, for BOTH Execute (Up/Down/Status) and
	// ExecuteStream (Logs).
	newOrch := func(forced string) (*RemoteComposeOrchestrator, *string) {
		captured := new(string)
		exec := &mockExecutor{
			executeFunc: func(_ context.Context, _ RemoteHost, cmd string) (*CommandResult, error) {
				*captured = cmd
				return &CommandResult{ExitCode: 0}, nil
			},
			executeStreamFunc: func(_ context.Context, _ RemoteHost, cmd string) (io.ReadCloser, error) {
				*captured = cmd
				return io.NopCloser(strings.NewReader("")), nil
			},
		}
		return NewRemoteComposeOrchestrator(
			host, exec, logging.NopLogger{}, WithComposeCommand(forced),
		), captured
	}

	t.Run("ProjectFile", func(t *testing.T) {
		orch, captured := newOrch("docker compose")
		require.NoError(t, orch.Up(context.Background(),
			compose.ComposeProject{File: inj}))
		require.Contains(t, *captured, escaped,
			"Up must shell-escape project.File (injection vector) — got %q", *captured)
	})

	t.Run("ProjectName", func(t *testing.T) {
		orch, captured := newOrch("docker compose")
		require.NoError(t, orch.Up(context.Background(),
			compose.ComposeProject{File: "dc.yml", Name: inj}))
		require.Contains(t, *captured, escaped,
			"Up must shell-escape project.Name (injection vector) — got %q", *captured)
	})

	t.Run("ProjectProfile", func(t *testing.T) {
		orch, captured := newOrch("docker compose")
		require.NoError(t, orch.Up(context.Background(),
			compose.ComposeProject{File: "dc.yml", Profile: inj}))
		require.Contains(t, *captured, escaped,
			"Up must shell-escape project.Profile (injection vector) — got %q", *captured)
	})

	t.Run("UpServices", func(t *testing.T) {
		orch, captured := newOrch("docker compose")
		require.NoError(t, orch.Up(context.Background(),
			compose.ComposeProject{File: "dc.yml", Services: []string{inj}}))
		require.Contains(t, *captured, escaped,
			"Up must shell-escape service names (injection vector) — got %q", *captured)
	})

	t.Run("LogsService", func(t *testing.T) {
		orch, captured := newOrch("docker compose")
		reader, err := orch.Logs(context.Background(),
			compose.ComposeProject{File: "dc.yml"}, inj)
		require.NoError(t, err)
		_ = reader.Close()
		require.Contains(t, *captured, escaped,
			"Logs must shell-escape the service name (injection vector) — got %q", *captured)
	})

	t.Run("StatusLabelFilter", func(t *testing.T) {
		// podman-compose forces the label-filter branch that splices
		// project.Name into `podman ps -a --filter label=...`.
		orch, captured := newOrch("podman-compose")
		_, err := orch.Status(context.Background(),
			compose.ComposeProject{File: "dc.yml", Name: inj})
		require.NoError(t, err)
		require.Contains(t, *captured, escaped,
			"Status label filter must shell-escape project.Name (injection vector) — got %q", *captured)
	})
}

// -----------------------------------------------------------------------------
// RM2-2 — ControlMaster socket path includes the SSH user (no cross-user collide)
// -----------------------------------------------------------------------------

// TestWave20_RM2_ControlSocketPathIncludesUser is the permanent RM2-2
// guard. Two hosts that share an address+port but differ in SSH user have
// DISTINCT pool keys (hostKey includes the user), so they MUST also resolve
// to distinct ControlMaster socket paths — otherwise the second host's
// master dial collides with the first's live socket and can never pool.
func TestWave20_RM2_ControlSocketPathIncludesUser(t *testing.T) {
	pool := &ConnectionPool{socketDir: t.TempDir(), opts: DefaultOptions()}

	alice := RemoteHost{Name: "a", Address: "gpu1.example", User: "deploy", Port: 22}
	bob := RemoteHost{Name: "b", Address: "gpu1.example", User: "root", Port: 22}

	require.NotEqual(t, hostKey(alice), hostKey(bob),
		"sanity: distinct SSH users must yield distinct pool keys")

	pa := pool.controlSocketPath(alice)
	pb := pool.controlSocketPath(bob)

	if pa == pb {
		t.Fatalf("RM2-2: deploy@gpu1 and root@gpu1 share the SAME ControlMaster "+
			"socket path %q despite distinct pool keys — the socket path must "+
			"include the SSH user (CLAUDE.md: 'one socket per (user@host:port)'); "+
			"a shared socket lets the second host collide with the first's live "+
			"master and never pool", pa)
	}
	require.Contains(t, pa, "deploy",
		"socket path must carry the SSH user; got %q", pa)
	require.Contains(t, pb, "root",
		"socket path must carry the SSH user; got %q", pb)
}
