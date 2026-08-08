package distribution

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"digital.vasic.containers/pkg/scheduler"
)

// TestDeployRemote_CommandInjection is the ATM-C056 §11.4.115 RED/guard test.
//
// It drives the REAL remote command-string builders (buildRemoteRunCommand /
// buildRemoteRemoveCommand) with a decoy shell-injection payload in the
// registry-controlled Name/Image fields, then executes the built string
// through /bin/sh in an isolated temp dir. The payload attempts to `touch`
// a sentinel file; if the shell metacharacters break out of their intended
// argument the sentinel is created (VULNERABLE), otherwise they were
// neutralised by single-quoting (FIXED).
//
// Polarity switch RED_MODE (§11.4.115), default "1":
//
//	RED_MODE=1 (default) — assert the payload IS neutralised (the fix).
//	    pre-fix code  -> injection fires  -> FAIL  (RED baseline)
//	    fixed code    -> neutralised      -> PASS  (GREEN regression guard)
//	RED_MODE=0 — forensic reproduce mode: assert the payload BREAKS OUT.
//	    pre-fix code  -> injection fires  -> PASS  (captures the reproduction
//	                                               as positive evidence)
//	    fixed code    -> neutralised      -> FAIL  (vuln gone, expected)
//
// So on the CURRENT (pre-fix) tree: default run FAILS (RED now); RED_MODE=0
// run PASSES and is the captured §11.4.115 RED-on-broken evidence. After the
// fix: default run PASSES (GREEN guard) and stays the permanent regression
// guard.
func TestDeployRemote_CommandInjection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("SKIP-with-reason (§11.4.3): no POSIX /bin/sh available")
	}

	wantNeutralised := os.Getenv("RED_MODE") != "0" // default "1"

	cases := []struct {
		name  string
		build func(payload string) string // calls the REAL builder
	}{
		{
			name: "run_cmd_Name_field",
			build: func(payload string) string {
				req := scheduler.ContainerRequirements{
					Name: payload, Image: "img",
				}
				return buildRemoteRunCommand(
					"true", req.Name, req.Image, req.Ports,
				)
			},
		},
		{
			name: "run_cmd_Image_field",
			build: func(payload string) string {
				req := scheduler.ContainerRequirements{
					Name: "app", Image: payload,
				}
				return buildRemoteRunCommand(
					"true", req.Name, req.Image, req.Ports,
				)
			},
		},
		{
			name: "remove_cmd_Name_field",
			build: func(payload string) string {
				req := scheduler.ContainerRequirements{Name: payload}
				return buildRemoteRemoveCommand("true", req.Name)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sentinel := filepath.Join(t.TempDir(), "pwned")
			// Decoy payload: if the shell interprets it, it touches the
			// sentinel; the trailing `true` swallows any following arg.
			payload := "x; touch " + sentinel + "; true"

			built := c.build(payload)

			// Physical oracle: run the built string through a real shell.
			_ = exec.Command(sh, "-c", built).Run() // exit code irrelevant
			_, statErr := os.Stat(sentinel)
			neutralised := statErr != nil // sentinel absent => neutralised

			if neutralised != wantNeutralised {
				if wantNeutralised {
					t.Fatalf(
						"ATM-C056 command injection: field NOT neutralised. "+
							"Built command executed the decoy payload "+
							"(sentinel %q created).\nbuilt: %s",
						sentinel, built,
					)
				}
				t.Fatalf(
					"RED_MODE=0 reproduce: expected the injection to fire "+
						"but the payload was neutralised (already fixed?).\n"+
						"built: %s",
					built,
				)
			}
		})
	}
}
