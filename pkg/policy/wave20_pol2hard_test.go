package policy

// Wave-20 POL2-HARD permanent regression guards (§11.4.115 GREEN polarity —
// each guard asserts the FIXED behavior, so the standing suite stays green
// on the fixed tree; the RED reproduction is captured out-of-band by
// surgically reverting the fix hunk, never committed).
//
//   - POL2-1 (HIGH, CapFor false-negative on malformed Match glob):
//     Policy.Validate() never probed each rule's Match pattern for glob
//     syntax errors. CapFor's `ok, err := filepath.Match(...); if err == nil
//     && ok` treats a syntax error (err != nil) identically to "no match"
//     (both leave the branch untaken), so a rule with a malformed pattern
//     (e.g. an unclosed "[" character class) silently NEVER matches in
//     CapFor and the affected service falls through to Default forever —
//     while Validate() kept reporting the policy "valid". Guarded by
//     TestPolicyValidate_MalformedMatchGlob_Rejected + the negative control
//     TestPolicyValidate_WellFormedGlobs_StillValidate.
//   - POL2-2 (MED, parseSize uint64 overflow): `return n * mult, nil` had no
//     overflow check, so an oversized numeric size (e.g. "20000000000g")
//     silently wraps to a small, WRONG byte count with a nil error — a
//     validation false-negative of the same shape as POL2-1. Guarded by
//     TestParseSize_OverflowingSize_ReturnsErrorNotWrappedValue + the
//     negative control TestParseSize_InRangeSize_StillParses.
//   - POL2-3 (doc + §11.4.86 drift-proofing, VerifyAgainstYAML): the package
//     doc told callers to run [VerifyAgainstYAML] to keep policy.go's
//     Default() aligned with scripts/resource-policy/policy.yaml, but the
//     symbol did not exist anywhere in the tree — nothing enforced the two
//     hand-maintained representations staying in sync. Implemented as a
//     real field-by-field diff (resolving each YAML pattern's omitted
//     pids/oom_adj from the top-level `defaults:` block, mirroring
//     apply_caps.py's make_cap(entry, default) fallback) and guarded
//     against the real, committed policy.yaml by
//     TestVerifyAgainstYAML_RealPolicyYAML_NoDrift, with the detector's
//     ability to actually catch drift (not just trivially pass) proven by
//     TestVerifyAgainstYAML_DriftedYAML_Rejected.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realPolicyYAMLPath returns the canonical path to
// scripts/resource-policy/policy.yaml relative to this package's directory
// (pkg/policy -> containers root -> scripts/resource-policy).
func realPolicyYAMLPath() string {
	return filepath.Join("..", "..", "scripts", "resource-policy", "policy.yaml")
}

// TestPolicyValidate_MalformedMatchGlob_Rejected is the permanent POL2-1
// guard: a rule whose Match pattern is not a syntactically valid
// filepath.Match glob (an unclosed "[" character class) MUST fail
// Policy.Validate(), instead of silently passing while CapFor can never
// match it.
func TestPolicyValidate_MalformedMatchGlob_Rejected(t *testing.T) {
	p := &Policy{
		Default: Cap{Mem: "1g", Memswap: "1g", Pids: 1024, OOMAdj: 500},
		Rules: []Rule{
			{Match: "mcp-puppeteer[", Cap: Cap{Mem: "1g", Memswap: "1g", Pids: 1024, OOMAdj: 500}},
		},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal(`Validate() accepted a policy with a malformed Match glob ("mcp-puppeteer[") — ` +
			"the rule would silently never match in CapFor, permanently falling through to Default")
	}
	if !strings.Contains(err.Error(), "Match pattern") {
		t.Fatalf("expected an 'invalid Match pattern' error, got: %v", err)
	}
}

// TestPolicyValidate_WellFormedGlobs_StillValidate is the negative control
// for POL2-1: the bundled Default() policy's 65 real-world glob patterns
// MUST still validate cleanly — the new glob-syntax probe must not
// over-correct into rejecting legitimate patterns.
func TestPolicyValidate_WellFormedGlobs_StillValidate(t *testing.T) {
	p := Default()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() rejected the bundled Default() policy's well-formed globs: %v", err)
	}
}

// TestParseSize_OverflowingSize_ReturnsErrorNotWrappedValue is the permanent
// POL2-2 guard: a numeric size large enough that n*mult overflows uint64
// MUST return an error, not silently wrap around to a small, wrong byte
// count with a nil error.
func TestParseSize_OverflowingSize_ReturnsErrorNotWrappedValue(t *testing.T) {
	got, err := parseSize("20000000000g")
	if err == nil {
		t.Fatalf(`parseSize("20000000000g") = %d, <nil error> — expected an overflow error, `+
			"not a silently-wrapped value", got)
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Fatalf(`parseSize("20000000000g"): expected an "overflow" error, got: %v`, err)
	}
}

// TestParseSize_InRangeSize_StillParses is the negative control for POL2-2:
// a legitimate, comfortably in-range size must still parse correctly — the
// new overflow guard must not reject values that fit in uint64.
func TestParseSize_InRangeSize_StillParses(t *testing.T) {
	got, err := parseSize("2g")
	if err != nil {
		t.Fatalf(`parseSize("2g") unexpected error: %v`, err)
	}
	if got != uint64(2)<<30 {
		t.Fatalf(`parseSize("2g") = %d, want %d`, got, uint64(2)<<30)
	}
}

// TestVerifyAgainstYAML_RealPolicyYAML_NoDrift is the permanent POL2-3
// guard: VerifyAgainstYAML against the real, committed
// scripts/resource-policy/policy.yaml MUST report no drift against
// Default() — the two hand-maintained representations are the project's
// declared dual source of truth (package doc, policy.go:7) and must stay
// aligned.
func TestVerifyAgainstYAML_RealPolicyYAML_NoDrift(t *testing.T) {
	path := realPolicyYAMLPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("real policy.yaml not found at %s (relative-path assumption broken?): %v", path, err)
	}
	if err := VerifyAgainstYAML(path); err != nil {
		t.Fatalf("VerifyAgainstYAML(%s) reported drift against Default(): %v", path, err)
	}
}

// TestVerifyAgainstYAML_DriftedYAML_Rejected is the negative control for
// POL2-3, proving the detector actually detects drift rather than
// trivially passing: a copy of the real policy.yaml with one pattern's mem
// value mutated MUST be rejected.
func TestVerifyAgainstYAML_DriftedYAML_Rejected(t *testing.T) {
	real := realPolicyYAMLPath()
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading real policy.yaml: %v", err)
	}
	// The first `mem: "12g"` in the file is the "ollama*" pattern's mem
	// field (policy.go's Default() has Cap{"12g", ...} for "ollama*").
	mutated := strings.Replace(string(raw), `mem: "12g"`, `mem: "11g"`, 1)
	if mutated == string(raw) {
		t.Fatal("mutation was a no-op — the fixture text this test depends on has changed; " +
			"update the replaced literal")
	}
	dir := t.TempDir()
	mutatedPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(mutatedPath, []byte(mutated), 0o644); err != nil {
		t.Fatalf("writing mutated policy.yaml: %v", err)
	}
	err = VerifyAgainstYAML(mutatedPath)
	if err == nil {
		t.Fatal("VerifyAgainstYAML accepted a YAML with a mutated mem value — " +
			"the drift detector is not detecting drift")
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected a drift error, got: %v", err)
	}
}

// TestVerifyAgainstYAML_MissingFile_ReturnsError confirms a missing YAML
// path returns a clean, wrapped error rather than any other failure mode.
func TestVerifyAgainstYAML_MissingFile_ReturnsError(t *testing.T) {
	err := VerifyAgainstYAML(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("VerifyAgainstYAML on a nonexistent path returned nil, expected an error")
	}
}
