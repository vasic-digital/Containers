#!/usr/bin/env bash
# boot_reports_what_it_did_challenge.sh — anti-bluff gate for cmd/boot.
#
# The defect this exists to catch: `boot` ran all three of its phases, started
# nothing at all, printed "Boot completed: 0 services processed", and exited 0.
# Measured before the fix, against a live podman host:
#
#   boot: Boot summary: 0 started, 0 remote, 0 discovered, 0 failed, 0 skipped
#   boot: Boot completed: 0 services processed
#   exit 0
#
# Nothing was listening on the endpoint it claimed to boot, and the host's
# container set was unchanged. A caller that branches on the exit code cannot
# distinguish "the stack is up" from "I did nothing".
#
# Two further no-ops fed it, and both are asserted here:
#   - cmd/boot declared its own distributorAdapter whose DistributeEndpoints
#     was `return 0, nil`, shadowing distribution.DefaultDistributor's real
#     implementation, so remote distribution deployed nothing and said it
#     succeeded;
#   - the endpoint list was a hardcoded consumer-specific host/port literal,
#     which is also a §6.R violation in a module that must stay consumer-
#     agnostic (§11.4.28(B)).
#
# Exit:
#   0 — all assertions PASS
#   1 — one or more FAIL
#   2 — could not determine (no Go toolchain, or the package will not build)

set -uo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || { echo "COULD NOT DETERMINE: no module root at $ROOT" >&2; exit 2; }

PASS_COUNT=0
FAIL_COUNT=0
pass() { echo "PASS: $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }

echo "=== boot_reports_what_it_did_challenge ==="
echo

command -v go >/dev/null 2>&1 || { echo "COULD NOT DETERMINE: no go toolchain" >&2; exit 2; }

TMP="$(mktemp -d)" || { echo "COULD NOT DETERMINE: no writable temp dir" >&2; exit 2; }
trap 'rm -rf "$TMP"' EXIT

if ! go build -o "$TMP/boot" ./cmd/boot 2>"$TMP/build.err"; then
    echo "COULD NOT DETERMINE: cmd/boot does not build:" >&2
    head -5 "$TMP/build.err" >&2
    exit 2
fi

# --- 1. a run that starts nothing must not report success ------------------
echo "[1/4] a boot that starts nothing does not exit 0"
mkdir -p "$TMP/proj"
: > "$TMP/proj/.env"
out="$("$TMP/boot" --env "$TMP/proj/.env" --project "$TMP/proj" --timeout 20s 2>&1)"; rc=$?
echo "    exit=$rc"
if [[ "$rc" -ne 0 ]]; then
    pass "no services booted -> exit $rc (non-zero)"
else
    fail "no services booted -> exit 0. Output: $(echo "$out" | tail -n3 | tr '\n' ' ')"
fi

# --- 2. and it must say so, not claim completion ---------------------------
echo "[2/4] it names the condition instead of claiming completion"
if grep -qiE 'no services|nothing (to boot|was started)|0 services' <<<"$out" \
   && ! grep -qiE 'Boot completed: 0 services processed' <<<"$out"; then
    pass "output states that nothing was booted"
else
    fail "output does not honestly describe a zero-service run: $(echo "$out" | tail -n3 | tr '\n' ' ')"
fi

# --- 3. no silently-no-op distributor override -----------------------------
# distribution.DefaultDistributor already implements DistributeEndpoints and its
# doc comment says so. Any local override in cmd/ that returns a bare (0, nil)
# is deploying nothing and reporting success.
echo "[3/4] cmd/ declares no DistributeEndpoints override that returns (0, nil)"
# Captured first, then matched with bash's own pattern operator. As a
# `... | grep -qE` pipeline under the `set -o pipefail` above, a match kills
# the left-hand grep with SIGPIPE (141) and pipefail promotes it — so this
# check FAILED OPEN: it took the `else pass` branch precisely when it had
# found the no-op override it exists to catch.
dist_overrides="$(grep -rn -A3 'func .*DistributeEndpoints' cmd/ 2>/dev/null || true)"
if [[ $dist_overrides == *"return 0, nil"* ]]; then
    fail "a DistributeEndpoints override in cmd/ returns (0, nil) — distributes nothing, reports success"
else
    pass "no no-op DistributeEndpoints override in cmd/"
fi

# --- 4. no hardcoded consumer host/port literal (§6.R, §11.4.28(B)) --------
echo "[4/4] cmd/boot carries no hardcoded endpoint literal"
if grep -nE '"(localhost|127\.0\.0\.1)"' cmd/boot/*.go >/dev/null 2>&1 \
   || grep -nE 'Port:[[:space:]]*"[0-9]{2,5}"' cmd/boot/*.go >/dev/null 2>&1; then
    echo "    offending lines:"
    grep -nE '"(localhost|127\.0\.0\.1)"|Port:[[:space:]]*"[0-9]{2,5}"' cmd/boot/*.go | sed 's/^/      /'
    fail "cmd/boot hardcodes a host/port literal (§6.R)"
else
    pass "cmd/boot hardcodes no host or port literal"
fi

echo
echo "=== summary: $PASS_COUNT pass, $FAIL_COUNT fail ==="
[[ $FAIL_COUNT -eq 0 ]] && exit 0 || exit 1
