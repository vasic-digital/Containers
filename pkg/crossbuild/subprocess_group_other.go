//go:build !unix

package crossbuild

import "os/exec"

// configureProcessGroup is a no-op on non-Unix platforms (Windows,
// Plan9, WASM/js): syscall.SysProcAttr{Setpgid: true} and negative-pid
// kill(2) process-group semantics do not exist there. cmd.Cancel is
// left nil, so the default exec.Cmd behaviour (kill the direct child
// only, via cmd.Process.Kill()) applies — the same as before this
// fix. True process-TREE termination on Windows needs a Job Object
// (tracked as the XB2-6 follow-up; out of scope here since neither
// this package's Selector nor its tests currently exercise a Windows
// build host).
func configureProcessGroup(cmd *exec.Cmd) {}
