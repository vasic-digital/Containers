package remote

// Wave-20 REMOTE3-ARGSWEEP permanent regression guards (§11.4.115 GREEN
// polarity) for the SECURITY finding-class the cross-cutting ARGSWEEP audit
// surfaced in pkg/remote: argument-injection via a LEADING-DASH ssh/scp
// DESTINATION positional, plus an unescaped remote-shell RUNTIME token.
//
// Root cause (§11.4.6, cited): host.User / host.Address / host.Runtime are
// verbatim env config (CONTAINERS_REMOTE_HOST_N_USER / _ADDRESS / _RUNTIME,
// ZERO validation). ssh/scp are spawned as a bare argv (no shell), so shell
// metacharacters in the destination are inert — but the destination is a
// POSITIONAL argv element with no reliable "--" host terminator, so a
// host.User of e.g. "-oProxyCommand=<cmd>" is parsed by ssh/scp's OWN getopt
// as an option → ProxyCommand arbitrary-command-execution on the
// control-plane host. This is the un-propagated sibling of the already-fixed
// pkg/network/tunnel.go (tunnelDestination leading-dash refuse) +
// pkg/remote/compose.go (RM2 shellEscape). The RemoteRuntime.runtimeCmd token
// (host.Runtime) is a distinct MED: it is spliced as the LEADING token of the
// remote `sh -c` string the remote login shell re-parses, while the
// container-id args in the SAME command are already shellEscape'd — a
// host.Runtime like "docker;evilcmd" injects.
//
// Each guard below asserts the FIXED behavior. The anti-tautology RED
// reproduction (surgical revert of the single fix anchor, capturing the real
// `--- FAIL` line, then restore) is recorded in the stream report rather than
// committed as a live broken test (mirrors the wave20_remote2hard /
// wave20_remote3 convention).
//
//   - RM3-1/2/3 (ssh_executor.go sshArgs / CopyFile+CopyDir scp dests /
//     connection_pool.go masterArgs + closeEntry): all route through the
//     single-source-of-truth sshDestination/scpDestination guard. The shared
//     anti-tautology anchor is the guard line
//     `if strings.HasPrefix(dest, "-") {` in sshDestination — reverting it to
//     `if false && strings.HasPrefix(dest, "-") {` disables the refusal for
//     every call site at once, flipping every RM3-1/2/3 subtest RED (each
//     asserts ssh/scp is NEVER spawned for a leading-dash destination), and
//     restore returns them all GREEN.
//   - RM3-5 (runtime.go runtimeCmd): anchor is
//     `fmt.Sprintf("%s %s", shellEscape(rt), args)`; revert `shellEscape(rt)`
//     → `rt` and the guard FAILs (the captured command carries the raw,
//     shell-active runtime token instead of the single-quote-escaped form).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
)

// rm3PoisonUser is a host.User crafted so the composed destination
// ("<user>@<address>") BEGINS WITH '-', which ssh/scp's getopt parses as an
// option — here injecting ProxyCommand (arbitrary command execution).
const rm3PoisonUser = "-oProxyCommand=touch /tmp/rm3_pwn"

// rm3CountingBody is a fake ssh/scp shell body that appends one line to the
// file named by $<counterEnv> on every invocation, then exits 0. It is the
// spawn-count spy: zero lines == the binary was NEVER spawned.
func rm3CountingBody(counterEnv string) string {
	return "#!/bin/sh\nprintf 'x\\n' >> \"$" + counterEnv + "\"\nexit 0\n"
}

// rm3SpawnCount returns how many times the spy fake ssh/scp ran (newline
// count in the counter file; 0 when the file was never created == never
// spawned).
func rm3SpawnCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// RM3-1 — sshArgs (SSHExecutor.Execute) refuses a leading-dash ssh destination
// -----------------------------------------------------------------------------

// TestWave20_RM3_SSHExecuteRefusesLeadingDashDestination is the permanent
// RM3-1 guard. A host.User of "-oProxyCommand=..." makes the ssh destination
// positional begin with '-', so ssh's getopt would parse it as an option
// (ProxyCommand RCE). Execute must refuse it via sshArgs→sshDestination
// BEFORE spawning ssh; a benign user@host must still reach the spawn.
func TestWave20_RM3_SSHExecuteRefusesLeadingDashDestination(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "ssh-spawns")
	t.Setenv("RM3_SSH_SPAWNS", counter)
	installFakeSSH(t, rm3CountingBody("RM3_SSH_SPAWNS"))

	exec, err := NewSSHExecutor(
		logging.NopLogger{},
		WithControlMaster(false),
		WithConnectTimeout(5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	poison := RemoteHost{Name: "evil", Address: "10.0.0.1", User: rm3PoisonUser}
	_, err = exec.Execute(context.Background(), poison, "echo ok")
	require.Error(t, err,
		"Execute must refuse a leading-dash ssh destination (ProxyCommand RCE)")
	require.Equal(t, 0, rm3SpawnCount(t, counter),
		"ssh must NEVER be spawned for a leading-dash destination")

	// Positive control: a benign user@host destination still reaches the spawn.
	benign := RemoteHost{Name: "ok", Address: "10.0.0.2", User: "deploy"}
	_, err = exec.Execute(context.Background(), benign, "echo ok")
	require.NoError(t, err)
	require.Greater(t, rm3SpawnCount(t, counter), 0,
		"sanity: a benign user@host destination must still spawn ssh")
}

// -----------------------------------------------------------------------------
// RM3-2 — CopyFile / CopyDir refuse a leading-dash scp destination
// -----------------------------------------------------------------------------

// TestWave20_RM3_SCPCopyRefusesLeadingDashDestination is the permanent RM3-2
// guard, covering all three scp destination construction sites (CopyFile,
// CopyDir matching-basename → parent, CopyDir basename-mismatch → dest) plus a
// benign positive control. Each subtest asserts scp is NEVER spawned for a
// leading-dash destination.
func TestWave20_RM3_SCPCopyRefusesLeadingDashDestination(t *testing.T) {
	poison := RemoteHost{Name: "evil", Address: "10.0.0.1", User: rm3PoisonUser}

	newExec := func(t *testing.T, scpCounterEnv string) *SSHExecutor {
		installFakeSCP(t, rm3CountingBody(scpCounterEnv))
		// CopyDir(basename-mismatch) also runs a pre-clean via ssh; a benign
		// fake ssh keeps that off the real client. PATH resolution only — the
		// pre-clean's ssh is refused by the same dest guard anyway.
		installFakeSSH(t, "#!/bin/sh\nexit 0\n")
		e, err := NewSSHExecutor(
			logging.NopLogger{},
			WithControlMaster(false),
			WithConnectTimeout(5*time.Second),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.Close() })
		return e
	}

	t.Run("CopyFile", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "scp-spawns")
		t.Setenv("RM3_SCP_CF", counter)
		e := newExec(t, "RM3_SCP_CF")
		local := filepath.Join(t.TempDir(), "f.txt")
		require.NoError(t, os.WriteFile(local, []byte("x"), 0o644))
		err := e.CopyFile(context.Background(), poison, local, "/remote/f.txt")
		require.Error(t, err, "CopyFile must refuse a leading-dash scp destination")
		require.Equal(t, 0, rm3SpawnCount(t, counter),
			"scp must NEVER spawn for a leading-dash CopyFile destination")
	})

	t.Run("CopyDir_MatchingBasename", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "scp-spawns")
		t.Setenv("RM3_SCP_CDM", counter)
		e := newExec(t, "RM3_SCP_CDM")
		local := t.TempDir()
		remote := "/remote/" + filepath.Base(local) // matching basenames → parent path
		err := e.CopyDir(context.Background(), poison, local, remote)
		require.Error(t, err,
			"CopyDir (matching basename) must refuse a leading-dash scp destination")
		require.Equal(t, 0, rm3SpawnCount(t, counter),
			"scp must NEVER spawn for a leading-dash CopyDir(parent) destination")
	})

	t.Run("CopyDir_BasenameMismatch", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "scp-spawns")
		t.Setenv("RM3_SCP_CDB", counter)
		e := newExec(t, "RM3_SCP_CDB")
		local := t.TempDir()
		remote := "/remote/differentname"
		require.NotEqual(t, filepath.Base(local), filepath.Base(remote),
			"setup: basenames must differ to hit the mismatch branch")
		err := e.CopyDir(context.Background(), poison, local, remote)
		require.Error(t, err,
			"CopyDir (basename mismatch) must refuse a leading-dash scp destination")
		require.Equal(t, 0, rm3SpawnCount(t, counter),
			"scp must NEVER spawn for a leading-dash CopyDir(dest) destination")
	})

	t.Run("BenignPositiveControl", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "scp-spawns")
		t.Setenv("RM3_SCP_OK", counter)
		e := newExec(t, "RM3_SCP_OK")
		local := filepath.Join(t.TempDir(), "f.txt")
		require.NoError(t, os.WriteFile(local, []byte("x"), 0o644))
		benign := RemoteHost{Name: "ok", Address: "10.0.0.2", User: "deploy"}
		require.NoError(t, e.CopyFile(context.Background(), benign, local, "/remote/f.txt"))
		require.Greater(t, rm3SpawnCount(t, counter), 0,
			"sanity: a benign scp destination must still spawn scp")
	})
}

// -----------------------------------------------------------------------------
// RM3-3 — ConnectionPool masterArgs + closeEntry refuse a leading-dash dest
// -----------------------------------------------------------------------------

// TestWave20_RM3_ConnectionPoolRefusesLeadingDashDestination is the permanent
// RM3-3 guard for the ControlMaster ssh destinations: the `-fNM` master dial
// (masterArgs, reached via Acquire) and the `-O exit` teardown (closeEntry,
// reached via CloseHost). Both must refuse a leading-dash destination without
// ever dialing ssh; a benign host must still carry the destination.
func TestWave20_RM3_ConnectionPoolRefusesLeadingDashDestination(t *testing.T) {
	poison := RemoteHost{Name: "evil", Address: "10.0.0.1", User: rm3PoisonUser}
	benign := RemoteHost{Name: "ok", Address: "10.0.0.2", User: "deploy"}

	t.Run("masterArgs", func(t *testing.T) {
		pool := &ConnectionPool{socketDir: t.TempDir(), opts: DefaultOptions()}
		_, err := pool.masterArgs(poison, "/tmp/sock")
		require.Error(t, err,
			"masterArgs must refuse a leading-dash ControlMaster destination (ProxyCommand RCE)")
		args, err := pool.masterArgs(benign, "/tmp/sock")
		require.NoError(t, err)
		require.True(t, containsSequence(args, "deploy@10.0.0.2"),
			"benign master dial must still carry the user@address destination; got %v", args)
	})

	t.Run("Acquire_NeverDialsPoisonedHost", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "ssh-spawns")
		t.Setenv("RM3_MASTER_SPAWNS", counter)
		installFakeSSH(t, rm3CountingBody("RM3_MASTER_SPAWNS"))
		opts := DefaultOptions()
		opts.ControlSocketDir = t.TempDir()
		pool, err := NewConnectionPool(opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })

		_, err = pool.Acquire(context.Background(), poison)
		require.Error(t, err,
			"Acquire must refuse to dial a leading-dash ControlMaster destination")
		require.Equal(t, 0, rm3SpawnCount(t, counter),
			"ssh master must NEVER be dialed for a leading-dash destination")
	})

	t.Run("closeEntry_via_CloseHost", func(t *testing.T) {
		counter := filepath.Join(t.TempDir(), "ssh-spawns")
		t.Setenv("RM3_CLOSE_SPAWNS", counter)
		installFakeSSH(t, rm3CountingBody("RM3_CLOSE_SPAWNS"))
		opts := DefaultOptions()
		opts.ControlSocketDir = t.TempDir()
		pool, err := NewConnectionPool(opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })

		sock := filepath.Join(opts.ControlSocketDir, "sock-poison")
		require.NoError(t, os.WriteFile(sock, []byte(""), 0o600))
		key := hostKey(poison)
		pool.mu.Lock()
		pool.active[key] = &controlEntry{
			host: poison, socketPath: sock, refs: 0, createdAt: time.Now(),
		}
		pool.mu.Unlock()

		err = pool.CloseHost(poison)
		require.Error(t, err,
			"closeEntry must refuse a leading-dash `-O exit` destination")
		require.Equal(t, 0, rm3SpawnCount(t, counter),
			"ssh `-O exit` must NEVER spawn for a leading-dash destination")
	})
}

// -----------------------------------------------------------------------------
// RM3-5 — runtimeCmd shell-escapes the host.Runtime remote-shell token
// -----------------------------------------------------------------------------

// TestWave20_RM3_RuntimeCmdShellEscapesRuntimeToken is the permanent RM3-5
// guard. host.Runtime is spliced as the LEADING token of the remote command
// the login shell re-parses; a value like "docker;touch /tmp/pwn" injected a
// second remote command. runtimeCmd must shellEscape it (matching the sibling
// container-id quoting in the SAME command), so the captured command carries
// the single-quote-escaped form and NOT the raw shell-active token.
func TestWave20_RM3_RuntimeCmdShellEscapesRuntimeToken(t *testing.T) {
	const inj = "docker;touch /tmp/rm3_pwn"
	escaped := shellEscape(inj)
	require.NotEqual(t, inj, escaped,
		"sanity: the injection runtime must require shell escaping")

	captured := new(string)
	exec := &mockExecutor{
		executeFunc: func(_ context.Context, _ RemoteHost, cmd string) (*CommandResult, error) {
			*captured = cmd
			return &CommandResult{ExitCode: 0}, nil
		},
	}
	host := RemoteHost{Name: "h", Address: "10.0.0.1", User: "deploy", Runtime: inj}
	rr := NewRemoteRuntime(host, exec, logging.NopLogger{})

	// Version drives exec → runtimeCmd(host.Runtime + args).
	_, _ = rr.Version(context.Background())

	require.Contains(t, *captured, escaped,
		"runtimeCmd must shell-escape host.Runtime (injection vector) — got %q", *captured)
	require.False(t, strings.HasPrefix(*captured, inj+" "),
		"runtimeCmd must not splice the raw host.Runtime as the leading remote-shell token — got %q",
		*captured)
}
