package policy

// Wave-19 policy-hardening permanent regression guards (§11.4.115 GREEN
// polarity — each guard asserts the FIXED behavior, so the standing suite
// stays green on the fixed tree; the RED reproduction is captured out-of-band
// by surgically reverting the fix hunk, never committed).
//
//   - CT-HARDEN-LZPOL-1 (parseSize panics "index out of range [-1]" on
//     whitespace-only input): the s == "" guard sat BEFORE strings.TrimSpace,
//     so " "/"\t" survived it, trimmed to "", and s[len(s)-1] indexed s[-1]
//     → panic. Reachable through the public API (Cap.Bytes/Cap.Validate on a
//     blank YAML/config Mem field). A panic where an "empty size" error was
//     intended is a §11.4.1 FAIL-bluff. Guarded by TestParseSize_Whitespace-
//     Only_ReturnsErrorNotPanic + TestCapValidate_WhitespaceMem_NoPanic.
//   - CT-HARDEN-LZPOL-2 (Cap.Validate approves oom_score_adj above the
//     kernel-valid maximum 1000): Validate rejected OOMAdj <= 0 but had no
//     upper bound, so a cap with OOMAdj > 1000 passed even though a write
//     >1000 to /proc/<pid>/oom_score_adj is rejected with EINVAL and the cap
//     could never be applied (§11.4.69 validation false-negative). Guarded by
//     TestCapValidate_OOMAdjAboveKernelMax_Rejected + the OOMAdj:500 negative
//     control TestCapValidate_OOMAdjWithinRange_StillValidates.

import (
	"strings"
	"testing"
)

// TestParseSize_WhitespaceOnly_ReturnsErrorNotPanic is the permanent
// CT-HARDEN-LZPOL-1 guard at the parseSize level: every whitespace-only input
// MUST return a clean "empty size" error and MUST NOT panic. A regression that
// moves the empty-check back before the trim re-exposes the s[-1] index panic,
// which the recover() below converts into a test failure.
func TestParseSize_WhitespaceOnly_ReturnsErrorNotPanic(t *testing.T) {
	for _, in := range []string{" ", "   ", "\t", "\n", " \t\n "} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parseSize(%q) PANICKED (must return a clean error): %v", in, r)
				}
			}()
			_, err := parseSize(in)
			if err == nil {
				t.Fatalf("parseSize(%q): expected an error, got nil", in)
			}
			if !strings.Contains(err.Error(), "empty size") {
				t.Fatalf("parseSize(%q): expected an \"empty size\" error, got: %v", in, err)
			}
		}()
	}
}

// TestCapValidate_WhitespaceMem_NoPanic is the permanent CT-HARDEN-LZPOL-1
// guard at the public-API level: a Cap with a whitespace-only Mem field (a
// plausible blank YAML/config value) MUST return an error, not crash the
// process. The recover() asserts no panic escaped.
func TestCapValidate_WhitespaceMem_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Cap.Validate with whitespace Mem PANICKED (public API): %v", r)
		}
	}()
	c := Cap{Mem: "  ", Memswap: "1g", Pids: 1024, OOMAdj: 500}
	if err := c.Validate(); err == nil {
		t.Fatalf("expected a Validate error for a whitespace-only Mem, got nil")
	}
}

// TestCapValidate_OOMAdjAboveKernelMax_Rejected is the permanent
// CT-HARDEN-LZPOL-2 guard: an OOMAdj above the kernel-valid maximum (1000)
// MUST be rejected, because a write >1000 to /proc/<pid>/oom_score_adj is
// rejected with EINVAL. A regression that drops the upper-bound check lets the
// invalid cap pass and fails this test.
func TestCapValidate_OOMAdjAboveKernelMax_Rejected(t *testing.T) {
	c := Cap{Mem: "1g", Memswap: "1g", Pids: 1024, OOMAdj: 5000}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate accepted OOMAdj=5000 (kernel max is 1000) — validation gap")
	}
	if !strings.Contains(err.Error(), "oom_score_adj") {
		t.Fatalf("expected an oom_score_adj range error, got: %v", err)
	}
}

// TestCapValidate_OOMAdjWithinRange_StillValidates is the negative control for
// CT-HARDEN-LZPOL-2: a valid in-range OOMAdj (the bundled Default() value 500)
// MUST still pass — the new upper bound must not over-correct into rejecting
// legitimate caps.
func TestCapValidate_OOMAdjWithinRange_StillValidates(t *testing.T) {
	c := Cap{Mem: "1g", Memswap: "1g", Pids: 1024, OOMAdj: 500}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid in-range cap (OOMAdj=500): %v", err)
	}
}
