package policy

// Wave-20 DEEPER (POL2-DEEP) permanent regression guards (§11.4.115 GREEN
// polarity — each guard asserts the FIXED behavior, so the standing suite
// stays green on the fixed tree; the RED reproduction is captured out-of-band
// by surgically reverting the fix hunk, never committed). §11.4.118
// loop-until-dry SECOND-pass finding beyond wave19 (LZPOL-1/2) and the
// wave20 core (POL2-1/2/3).
//
//   - POL2-DEEP-1 (MED, VerifyAgainstYAML mem/memswap fallback fidelity gap):
//     resolve()/VerifyAgainstYAML documented itself as mirroring
//     apply_caps.py's make_cap(entry, default) fallback, but only inherited a
//     pattern's Pids/OOMAdj from the top-level `defaults:` — NOT its
//     Mem/Memswap. make_cap (scripts/resource-policy/apply_caps.py:155-161)
//     falls back ALL FOUR fields (`str(d.get("mem", default.mem))`, ...). So a
//     pattern that legitimately OMITS `mem:`/`memswap:` to inherit the 2g
//     default — a make_cap-valid YAML shape that apply_caps.py resolves to an
//     identical cap — was scored as DRIFT by VerifyAgainstYAML (yp.Mem was ""
//     and compared against policy.go's explicit "2g"). That is a false-drift
//     FAIL-bluff (§11.4.1): the drift detector reports disagreement where the
//     two representations genuinely agree, breaking the declared dual source
//     of truth (policy.go:7). Root cause: resolve() passed yp.Mem/yp.Memswap
//     straight through with no `== ""` -> default fallback. Guarded by
//     TestWave20_POL2_YAMLPatternOmitsMemInheritsDefault_NoFalseDrift + the
//     negative control TestWave20_POL2_YAMLPatternOmitsMemDiffersFromGo_StillDrift.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWave20_POL2_YAMLPatternOmitsMemInheritsDefault_NoFalseDrift is the
// permanent POL2-DEEP-1 guard. The real policy.yaml's "mcp-firecrawl*" pattern
// specifies mem: "2g" / memswap: "2g", which equal the top-level defaults.
// Removing those two lines yields a make_cap-EQUIVALENT YAML: apply_caps.py's
// make_cap(entry, default) inherits mem/memswap from `defaults:` and resolves
// mcp-firecrawl to exactly {2g, 2g, 2048, 500} — identical to policy.go's
// Default() rule for it. VerifyAgainstYAML MUST therefore report NO drift.
// Pre-fix (resolve passed yp.Mem="" through) it reported a spurious cap-drift
// error, false-failing the sync gate; this test would fail there, catching a
// regression that drops the mem/memswap fallback.
func TestWave20_POL2_YAMLPatternOmitsMemInheritsDefault_NoFalseDrift(t *testing.T) {
	real := realPolicyYAMLPath()
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading real policy.yaml: %v", err)
	}
	// Unique 4-line block for mcp-firecrawl (mem/memswap == the 2g defaults),
	// reduced to omit mem+memswap so both must be inherited from `defaults:`.
	const block = "  - match: \"mcp-firecrawl*\"\n" +
		"    mem: \"2g\"\n" +
		"    memswap: \"2g\"\n" +
		"    pids: 2048\n"
	const reduced = "  - match: \"mcp-firecrawl*\"\n" +
		"    pids: 2048\n"
	mutated := strings.Replace(string(raw), block, reduced, 1)
	if mutated == string(raw) {
		t.Fatal("mutation was a no-op — the mcp-firecrawl fixture block this test " +
			"depends on has changed; update the block literal")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatalf("writing mutated policy.yaml: %v", err)
	}

	if err := VerifyAgainstYAML(p); err != nil {
		t.Fatalf("VerifyAgainstYAML reported drift on a make_cap-equivalent YAML "+
			"(mcp-firecrawl omitting mem/memswap to inherit the 2g default): %v\n"+
			"resolve() must inherit mem/memswap from defaults exactly like "+
			"apply_caps.py's make_cap(entry, default), otherwise an omitted "+
			"mem/memswap is a false-drift FAIL-bluff", err)
	}
}

// TestWave20_POL2_YAMLPatternOmitsMemDiffersFromGo_StillDrift is the negative
// control for POL2-DEEP-1 (§11.4.107(10) golden-bad): the fix must NOT
// over-correct into blindly passing every omitted-mem pattern. The "ollama*"
// pattern's policy.go cap is 12g, but the top-level default is 2g. Removing
// ollama's `mem: "12g"` line makes make_cap resolve it to the 2g default —
// which genuinely DISAGREES with policy.go's 12g. VerifyAgainstYAML MUST still
// report drift (the inherited 2g != the Go-side 12g). A fix that suppressed
// drift for any omitted-mem pattern would mask this real disagreement.
func TestWave20_POL2_YAMLPatternOmitsMemDiffersFromGo_StillDrift(t *testing.T) {
	real := realPolicyYAMLPath()
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading real policy.yaml: %v", err)
	}
	// Unique 2-line block: ollama's match line + its mem line. Drop the mem
	// line so ollama inherits the 2g default, which != policy.go's 12g.
	const block = "  - match: \"ollama*\"\n" +
		"    mem: \"12g\"\n"
	const reduced = "  - match: \"ollama*\"\n"
	mutated := strings.Replace(string(raw), block, reduced, 1)
	if mutated == string(raw) {
		t.Fatal("mutation was a no-op — the ollama fixture block this test " +
			"depends on has changed; update the block literal")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(p, []byte(mutated), 0o644); err != nil {
		t.Fatalf("writing mutated policy.yaml: %v", err)
	}

	err = VerifyAgainstYAML(p)
	if err == nil {
		t.Fatal("VerifyAgainstYAML accepted an omitted-mem pattern (ollama*) whose " +
			"inherited 2g default != policy.go's 12g — the fallback fix must not " +
			"mask a genuine mem disagreement")
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected a drift error for ollama's inherited-2g vs Go-12g, got: %v", err)
	}
}
