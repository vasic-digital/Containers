package vm

// Wave-20 DEEPER (§11.4.118) 2nd-pass VM2 permanent regression guards
// (§11.4.115 GREEN polarity). Each guard below asserts the FIXED
// behavior of a NEW §11.4.118 audit finding surfaced beyond the
// already-landed VM2-1/2/3/5 + CT-HARDEN cluster. The paired RED
// reproduction (surgical revert of the fix, capturing the real
// `--- FAIL` line, then restore) is recorded in the authoring report
// rather than committed as a live broken test.
//
//   - VM2-6 (clients.go escapeJSONString — the QMP `screendump`
//     filename was escaped via a `for k, v := range map` two-pass
//     ReplaceAll whose iteration order Go randomizes, so a filename
//     containing a `"` produced INVALID JSON / QMP-argument injection on
//     a non-deterministic ~13% of calls, and control characters were
//     never escaped at all): guarded by
//     TestWave20_VM2_ScreendumpFilenameJSONEscapeIsDeterministicAndComplete.
//   - VM2-7 (matrix.go RunMatrix — the I1 guard rejected only a
//     traversing CaptureSpec.HostSubpath, leaving target.ID unguarded
//     even though runOne feeds it into the SAME
//     filepath.Join(EvidenceDir, target.ID, ...) for every captured file
//     AND the screenshot-on-failure path, so a target.ID like
//     "../../etc" escaped the evidence sandbox): guarded by
//     TestWave20_VM2_TargetIDPathTraversalRejected.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// VM2-6 — screendump filename JSON escaping is deterministic AND complete
// -----------------------------------------------------------------------------

// TestWave20_VM2_ScreendumpFilenameJSONEscapeIsDeterministicAndComplete is
// the permanent VM2-6 guard. It builds the EXACT QMP command template
// realQMPClient.Screendump uses (`{"execute":"screendump","arguments":
// {"filename":"<escapeJSONString(path)>"}}`) for a set of adversarial
// filenames and asserts that a real JSON parser (what a real QEMU monitor
// runs) accepts the command AND recovers the filename byte-for-byte (no
// early string termination, no injected arguments).
//
// The `"`-bearing names are the load-bearing cases: the pre-fix
// map-iteration escaper produced valid JSON only when Go happened to
// iterate `\` before `"`, which is ~87% of calls — so a single call could
// pass by luck. We loop many times to defeat that non-determinism exactly
// as a real matrix run (repeated screendumps across a target×state matrix)
// eventually would. The control-character name additionally fails the
// pre-fix code DETERMINISTICALLY (it escaped only `\` and `"`, never
// `\n`), independent of map order.
func TestWave20_VM2_ScreendumpFilenameJSONEscapeIsDeterministicAndComplete(t *testing.T) {
	names := []string{
		`a"b`,                // bare double-quote (legal Linux filename byte)
		`x","injected":"y`,   // quote shaped as a QMP-argument-injection payload
		`back\slash`,         // lone backslash
		`both"and\here`,      // both, adjacent
		"has\nnewline\tctrl", // control chars: pre-fix escaper never touched these
		`/evi/a"b/shot.ppm`,  // realistic host screenshot path with a quote
	}
	const iters = 1000
	for _, name := range names {
		for i := 0; i < iters; i++ {
			esc := escapeJSONString(name)
			// EXACT template from realQMPClient.Screendump.
			cmd := `{"execute":"screendump","arguments":{"filename":"` + esc + `"}}`
			var parsed struct {
				Execute   string `json:"execute"`
				Arguments struct {
					Filename string `json:"filename"`
				} `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(cmd), &parsed); err != nil {
				t.Fatalf("iter %d name %q: screendump command is INVALID JSON (QEMU would reject / mis-parse it): %v\ncmd=%s",
					i, name, err, cmd)
			}
			if parsed.Execute != "screendump" {
				t.Fatalf("iter %d name %q: JSON injection changed the command verb to %q\ncmd=%s",
					i, name, parsed.Execute, cmd)
			}
			if parsed.Arguments.Filename != name {
				t.Fatalf("iter %d name %q: filename round-trip mismatch (injection / truncation): got %q\ncmd=%s",
					i, name, parsed.Arguments.Filename, cmd)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// VM2-7 — RunMatrix rejects a target.ID that escapes EvidenceDir
// -----------------------------------------------------------------------------

// TestWave20_VM2_TargetIDPathTraversalRejected is the permanent VM2-7
// guard. runOne writes captured files and the screenshot-on-failure PPM
// to filepath.Join(EvidenceDir, target.ID, ...); the I1 guard only
// checked CaptureSpec.HostSubpath, so a traversing target.ID silently
// escaped the evidence sandbox. Each fixture below is FIRST proven to be a
// genuine traversal (the exact Join runOne performs lands OUTSIDE
// EvidenceDir — this is what makes the guard non-tautological), THEN
// RunMatrix is asserted to reject it.
func TestWave20_VM2_TargetIDPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	r := NewQEMUMatrixRunner(&stubVM{}, nil) // nil store: no image resolution

	run := func(id string) error {
		_, err := r.RunMatrix(context.Background(), VMMatrixConfig{
			Targets:       []VMTarget{{ID: id, Arch: "x86_64"}},
			Script:        "/tmp/x.sh",
			EvidenceDir:   dir,
			ImageManifest: "unused-manifest.json", // store==nil, never loaded
			Concurrent:    1,
		})
		return err
	}

	// Non-tautology floor: a legitimate target.ID MUST still be accepted,
	// so the guard is a targeted traversal reject — not a "reject every ID".
	if err := run("alpine-x86_64"); err != nil {
		t.Fatalf("legitimate target.ID rejected — guard is over-broad: %v", err)
	}

	for _, badID := range []string{
		"../../etc/shadow", // relative-up traversal (genuinely escapes via Join)
		"a/../../b",        // embedded ".." traversal (genuinely escapes via Join)
		"/absolute/evil",   // absolute path (defensive reject, mirrors I1)
	} {
		t.Run(badID, func(t *testing.T) {
			// For the ".."-bearing fixtures, prove the escape is genuine:
			// the exact filepath.Join runOne performs lands OUTSIDE
			// EvidenceDir. (An absolute middle component is neutralised by
			// filepath.Join, so its rejection is defensive, matching the
			// I1 HostSubpath guard's own "/absolute/path" case.)
			if strings.Contains(badID, "..") {
				joined := filepath.Join(dir, badID, "screenshot-on-failure.ppm")
				if strings.HasPrefix(joined, dir+string(filepath.Separator)) {
					t.Fatalf("fixture bug: target.ID %q did NOT escape EvidenceDir (joined=%s); the guard would be vacuous",
						badID, joined)
				}
			}

			err := run(badID)
			if err == nil {
				t.Fatalf("RunMatrix accepted target.ID %q — evidence/screenshot writes would land outside the sandbox", badID)
			}
			if !strings.Contains(err.Error(), "path traversal") {
				t.Fatalf("RunMatrix error for target.ID %q: want a 'path traversal' rejection, got: %v", badID, err)
			}
		})
	}
}
