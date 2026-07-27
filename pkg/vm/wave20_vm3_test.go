// SPDX-License-Identifier: Apache-2.0
package vm

// Wave-20 VM3-7-ARGSWEEP permanent regression guards (§11.4.115 GREEN polarity)
// for the command-injection class the cross-cutting ARGSWEEP audit surfaced in
// pkg/vm/clients.go: realSSHClient.Upload/Download splice a GUEST path into the
// `scp -t <dir>` / `scp -f <path>` string that the guest LOGIN SHELL re-parses
// (crypto/ssh session.Run/Start runs the string through the guest's shell). A
// guest path carrying a shell metacharacter (`;`, `$(...)`, backtick, `|`, `&`,
// whitespace) breaks out of the scp command and runs attacker-chosen commands
// on the guest. Both sites must single-quote the path (shellQuote) AND prepend
// `--` (so scp's own getopt cannot parse a leading-'-' path as an option).
//
// Each test captures the EXACT command the in-process ssh server (reused from
// clients_test.go: startSSHServer / connectAuthenticated / readExecCommand)
// receives, and asserts it is the neutralised form. The handler captures the
// command then closes the channel — the transfer outcome is irrelevant here
// (successful transfer is already covered by clients_test.go); this test proves
// the COMMAND CONSTRUCTION, and closing keeps the test hang-free.
//
// Anti-tautology anchors (revert → the captured command carries the raw,
// shell-active guest path → require.Equal FAILs; restore → GREEN):
//   Upload:   `"scp -t -- " + shellQuote(targetDir)` → `"scp -t " + targetDir`
//   Download: `"scp -f -- " + shellQuote(vmPath)`    → `"scp -f " + vmPath`

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// captureCmdServer starts an in-process ssh server whose session handler
// records the first exec command string, then closes the channel. Returns the
// port and a getter for the captured command.
func captureCmdServer(t *testing.T) (int, func() string) {
	t.Helper()
	var mu sync.Mutex
	var cmd string
	port := startSSHServer(t, sshServerOpts{
		sessionHandler: func(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
			c, _ := readExecCommand(reqs)
			mu.Lock()
			cmd = c
			mu.Unlock()
			_ = ch.Close()
		},
	})
	return port, func() string {
		mu.Lock()
		defer mu.Unlock()
		return cmd
	}
}

func TestWave20_VM3_7_UploadShellQuotesGuestDir(t *testing.T) {
	dir := t.TempDir()
	hostSrc := filepath.Join(dir, "src")
	require.NoError(t, os.WriteFile(hostSrc, []byte("payload"), 0o644))

	port, getCmd := captureCmdServer(t)
	c := connectAuthenticated(t, port, "")

	// Guest dir carries a shell metacharacter + a space. Upload derives the
	// target dir via filepath.Dir(vmPath) = "/tmp/eviL;touch pwn".
	_ = c.Upload(t.Context(), hostSrc, "/tmp/eviL;touch pwn/src")

	require.Equal(t, "scp -t -- '/tmp/eviL;touch pwn'", getCmd(),
		"Upload must single-quote the guest dir + prepend -- so `;` cannot inject the guest shell (VM3-7)")
}

func TestWave20_VM3_7_DownloadShellQuotesGuestPath(t *testing.T) {
	port, getCmd := captureCmdServer(t)
	c := connectAuthenticated(t, port, "")

	dst := filepath.Join(t.TempDir(), "out")
	_ = c.Download(t.Context(), "/guest/eviL;touch pwn/out", dst)

	require.Equal(t, "scp -f -- '/guest/eviL;touch pwn/out'", getCmd(),
		"Download must single-quote the guest path + prepend -- so `;` cannot inject the guest shell (VM3-7)")
}
