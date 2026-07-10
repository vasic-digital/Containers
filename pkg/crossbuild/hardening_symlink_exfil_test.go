package crossbuild

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCTHardenXbuildF1_SymlinkArtifactNotFollowed is the §11.4.115 guard for
// XBUILD-F1 (independent-review finding): a build that drops a symlink AT an
// in-SourceDir OutputSubpath pointing OUTSIDE SourceDir must NOT be followed by
// the artifact-copy step. OutputSubpath stays within SourceDir (so isWithinDir
// passes), but copyRegularFile's os.Open would otherwise follow the freshly
// created link and exfiltrate an arbitrary host file (an SSH key, etc.) into
// HostOutputDir. The fix makes copyFile refuse a top-level symlink artifact,
// matching copyDir's existing no-follow stance. This drives the REAL
// HostDirectBackend (real sh -c runner) end to end — no mocks.
//
//	default (RED_MODE unset / "1"): GUARD — Build errors AND the secret is NOT
//	    copied into HostOutputDir.
//	RED_MODE=0: forensic reproduce — pre-fix, Build succeeds and exfiltrates the
//	    outside secret (this is the residual the reviewer captured live).
func TestCTHardenXbuildF1_SymlinkArtifactNotFollowed(t *testing.T) {
	guard := os.Getenv("RED_MODE") != "0" // default guard; RED_MODE=0 = reproduce

	// Secret lives OUTSIDE SourceDir.
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_ed25519")
	const secretContent = "SUPER-SECRET-PRIVATE-KEY-MATERIAL"
	if err := os.WriteFile(secret, []byte(secretContent), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// The build drops a FRESH symlink at an in-SourceDir subpath pointing at the
	// outside secret. OutputSubpath itself stays within SourceDir (passes the
	// string-math isWithinDir check), so only the symlink no-follow guard at the
	// copy choke-point can stop the exfil.
	sub := "desktopApp/build/app.bin"
	buildCmd := "mkdir -p desktopApp/build && ln -s " + secret + " " + sub

	be := NewHostDirectBackend()
	res := be.Build(context.Background(), BuildRequest{
		Target:        Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		SourceDir:     srcDir,
		BuildCommand:  buildCmd,
		OutputSubpath: sub,
		HostOutputDir: outDir,
		Timeout:       30 * time.Second,
	})

	// Did the secret leak into HostOutputDir?
	copied := filepath.Join(outDir, filepath.Base(sub))
	leaked := false
	if b, err := os.ReadFile(copied); err == nil && strings.Contains(string(b), secretContent) {
		leaked = true
	}

	if guard {
		if res.Error == nil {
			t.Fatalf("guard: expected Build to REFUSE the symlink artifact, got success (ArtifactPath=%q)", res.ArtifactPath)
		}
		if leaked {
			t.Fatalf("guard: secret EXFILTRATED into HostOutputDir despite the error (%q)", copied)
		}
	} else {
		if !leaked {
			t.Fatalf("RED_MODE=0 reproduce: expected the pre-fix symlink exfil (secret copied to %q), but it did not happen (err=%v)", copied, res.Error)
		}
	}
}

// TestCTHardenXbuildF3_OutputSubpathDotRejected guards the review's F3 note:
// an OutputSubpath resolving to SourceDir itself (".", "./") would make the
// artifact-copy step treat the WHOLE source tree as the "artifact." That is a
// degenerate misuse, not a real artifact; validateRequest now rejects it so the
// contract is unambiguous (OutputSubpath names an artifact WITHIN SourceDir).
func TestCTHardenXbuildF3_OutputSubpathDotRejected(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	for _, sub := range []string{".", "./", "foo/.."} {
		err := validateRequest(BuildRequest{
			SourceDir:     srcDir,
			BuildCommand:  "true",
			OutputSubpath: sub,
			HostOutputDir: outDir,
		})
		if err == nil {
			t.Fatalf("OutputSubpath %q (resolves to SourceDir) must be rejected, got nil error", sub)
		}
		if !strings.Contains(err.Error(), "SourceDir itself") {
			t.Fatalf("OutputSubpath %q: expected a 'SourceDir itself' rejection, got: %v", sub, err)
		}
	}
}
