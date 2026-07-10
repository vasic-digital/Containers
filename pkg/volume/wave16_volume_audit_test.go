package volume

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"digital.vasic.containers/pkg/logging"
	"digital.vasic.containers/pkg/remote"
)

// Wave-16 volume audit guards. Each guard reproduces a real defect on the
// pre-fix code and turns GREEN only against the fixed behaviour. Reuses the
// package's existing configurable mocks (moreVolExecutor / moreVolHostMgr).

// TestWave16_Volume_ShellQuote_PreventsInjection covers DEFECT-3: remote-shell
// command lines interpolate caller-supplied volume paths, so a path with a
// space is word-split (wrong directory) and a path with shell metacharacters
// (;, $(...), backticks) is executed verbatim on the remote host — arbitrary
// command injection. The fix passes every path through shellQuote.
func TestWave16_Volume_ShellQuote_PreventsInjection(t *testing.T) {
	// Unit: the helper single-quotes and escapes any embedded single quote.
	assert.Equal(t, `'/a b'`, shellQuote("/a b"))
	assert.Equal(t, `'a'\''b'`, shellQuote("a'b"))

	// Integration: a RemotePath carrying a space + a shell metacharacter
	// sequence reaches the remote shell as ONE quoted literal. Without
	// quoting the mkdir line would be `mkdir -p /mnt/a b;touch /tmp/pwned`,
	// which both splits the path and runs an injected command.
	var cmds []string
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			cmds = append(cmds, cmd)
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	m := NewSSHFSMounter(exec, logging.NopLogger{}, DefaultMountOptions())
	host := remote.RemoteHost{Name: "h", Address: "1.2.3.4", User: "u"}
	evil := "/mnt/a b;touch /tmp/pwned"
	require.NoError(t, m.Mount(
		context.Background(), host,
		VolumeMount{Name: "x", LocalPath: "/l", RemotePath: evil},
	))

	require.NotEmpty(t, cmds)
	mkdir := cmds[0]
	// The path appears as a single-quoted literal ...
	assert.Contains(t, mkdir, "mkdir -p '"+evil+"'")
	// ... and never as a bare, unquoted metacharacter sequence.
	assert.False(t, strings.HasPrefix(mkdir, "mkdir -p "+evil),
		"path must be quoted, got unquoted command: %q", mkdir)
}

// TestWave16_Volume_Unmount_FreesNameForReuse covers DEFECT-1: a successful
// Unmount previously left the entry in the map (marked unmounted), so the name
// could never be re-mounted ("already exists") and the map grew unbounded
// across mount/unmount churn. The fix deletes the entry on success.
func TestWave16_Volume_Unmount_FreesNameForReuse(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	exec := &moreVolExecutor{} // every command succeeds
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))
	require.Len(t, mgr.ListMounts(), 1)

	require.NoError(t, mgr.Unmount(context.Background(), "data"))

	// Entry removed: the map shrinks back and the name is free.
	assert.Empty(t, mgr.ListMounts())
	_, statErr := mgr.Status("data")
	assert.Error(t, statErr)

	// Re-mounting the same name now succeeds (no "already exists").
	require.NoError(t, mgr.Mount(context.Background(), mount))
	assert.Len(t, mgr.ListMounts(), 1)
}

// TestWave16_Volume_Unmount_PropagatesRemoteFailure covers DEFECT-2: when the
// remote unmount command itself fails (busy / unreachable) the manager
// previously ignored the error and reported success, so UnmountAll and callers
// believed a still-mounted volume was gone. The fix propagates the error and
// keeps the entry marked failed.
func TestWave16_Volume_Unmount_PropagatesRemoteFailure(t *testing.T) {
	hm := &moreVolHostMgr{hosts: map[string]remote.RemoteHost{
		"host-1": {Name: "host-1", Address: "10.0.0.1", User: "u", Port: 22},
	}}
	// mkdir + sshfs mount succeed; the fusermount unmount fails (exit 1).
	exec := &moreVolExecutor{
		fn: func(_ context.Context, _ remote.RemoteHost, cmd string) (*remote.CommandResult, error) {
			if strings.HasPrefix(cmd, "fusermount") {
				return &remote.CommandResult{ExitCode: 1, Stderr: "device is busy"}, nil
			}
			return &remote.CommandResult{ExitCode: 0}, nil
		},
	}
	mgr := newMoreTestVolumeManager(hm, exec)

	mount := VolumeMount{
		Name: "data", Type: MountSSHFS,
		LocalPath: "/a", RemotePath: "/b", HostName: "host-1",
	}
	require.NoError(t, mgr.Mount(context.Background(), mount))

	err := mgr.Unmount(context.Background(), "data")
	require.Error(t, err) // the remote failure is NOT swallowed

	// The entry is preserved and marked failed so UnmountAll / callers see the
	// volume is still mounted remotely.
	info, statErr := mgr.Status("data")
	require.NoError(t, statErr)
	assert.Equal(t, MountFailed, info.State)
}
