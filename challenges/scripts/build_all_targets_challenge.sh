#!/usr/bin/env bash
# build_all_targets_challenge.sh — gate for scripts/build-all.sh.
#
# The defect this exists to catch: build-all.sh named three hardcoded build
# targets — ./cmd/core/, ./cmd/host-agent/discovery/ and ./cmd/host-agent/ —
# and NONE of the three exists in this module. Measured before the fix:
#
#   $ bash scripts/build-all.sh
#   === Building the consuming project's service binaries ===
#   Building core...
#   stat .../cmd/core: directory not found
#   exit 1
#
# Two things were wrong, and only one of them is the obvious one:
#   - the targets were a consuming project's binaries, hardcoded into a module
#     that must stay consumer-agnostic (§11.4.28(B)), and they had drifted to
#     name nothing that exists here;
#   - `set -e` aborted on the FIRST missing target, so a casual run reported one
#     broken target when in fact all three were broken. A run must report every
#     missing target, or it under-states the damage.
#
# It also resolved `bin/` against $PWD rather than the module root, so running
# it from anywhere but the root silently wrote somewhere else.
#
# Exit:
#   0 — all assertions PASS
#   1 — one or more FAIL
#   2 — could not determine (no Go toolchain)

set -uo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
SUT="$ROOT/scripts/build-all.sh"

PASS_COUNT=0
FAIL_COUNT=0
pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }

echo "=== build_all_targets_challenge ==="
echo

[[ -f "$SUT" ]] || { echo "COULD NOT DETERMINE: $SUT not found" >&2; exit 2; }
command -v go >/dev/null 2>&1 || { echo "COULD NOT DETERMINE: no go toolchain" >&2; exit 2; }

# --- 1. every target the script would build must actually exist ------------
echo "[1/3] the script names no build target that does not exist"
# Comments are stripped first: the invariant is that no dangling target is
# BUILT, not that a path never appears in prose. The header documents the three
# targets this defect was, by name, and that documentation must not trip a gate.
missing=()
while read -r t; do
    [[ -z "$t" ]] && continue
    [[ -d "$ROOT/${t#./}" ]] || missing+=("$t")
done < <(sed 's/#.*//' "$SUT" | grep -oE '\./cmd/[A-Za-z0-9_/-]+' | sort -u)
if [[ ${#missing[@]} -eq 0 ]]; then
    pass "no dangling ./cmd/... target named in the script"
else
    fail "script names ${#missing[@]} target(s) that do not exist: ${missing[*]}"
fi

# --- 2. it builds this module, from any cwd, and succeeds ------------------
echo "[2/3] a real run from an unrelated cwd succeeds and writes into the module"
TMP="$(mktemp -d)" || { echo "COULD NOT DETERMINE: no temp dir" >&2; exit 2; }
trap 'rm -rf "$TMP"' EXIT
out="$(cd "$TMP" && bash "$SUT" 2>&1)"; rc=$?
echo "    exit=$rc"
if [[ "$rc" -eq 0 ]]; then
    pass "build-all.sh -> exit 0 from an unrelated cwd"
else
    fail "build-all.sh -> exit $rc. Output: $(echo "$out" | tail -n4 | tr '\n' ' ')"
fi
if [[ -z "$(find "$TMP" -mindepth 1 -maxdepth 1 -name bin -print -quit)" ]]; then
    pass "it did not create bin/ in the caller's cwd"
else
    fail "it created bin/ in the caller's cwd — output dir is \$PWD-relative"
fi

# --- 3. a missing target is reported in full, not one-at-a-time ------------
echo "[3/3] with a missing target seeded, it names every missing target"
# Seeded through the script's own BUILD_TARGETS override, so the script under
# test stays in its module and still resolves its own root.
perr="$(BUILD_TARGETS="./cmd/definitely-absent-one ./cmd/definitely-absent-two" \
        bash "$SUT" 2>&1)"; prc=$?
if [[ "$prc" -ne 0 ]] \
   && grep -q 'definitely-absent-one' <<<"$perr" \
   && grep -q 'definitely-absent-two' <<<"$perr"; then
    pass "both missing targets named in one run (no first-failure early exit)"
else
    fail "missing targets not both reported (exit $prc): $(echo "$perr" | tail -n3 | tr '\n' ' ')"
fi

echo
echo "=== summary: $PASS_COUNT pass, $FAIL_COUNT fail ==="
[[ $FAIL_COUNT -eq 0 ]] && exit 0 || exit 1
