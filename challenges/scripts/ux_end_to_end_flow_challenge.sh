#!/usr/bin/env bash
# ux_end_to_end_flow_challenge.sh — anti-bluff UX Challenge for the
# Containers submodule per CONST-035 + CONST-050(B). Submodule
# cascade per CONST-051(A). End-to-end user journey coherence.

set -uo pipefail

CT_BIN="${CONTAINERS_BIN:-}"
TIMEOUT_SEC="${UX_TIMEOUT_SEC:-30}"
USER_HOSTILE=('panic:' 'goroutine [0-9]+ \[running\]:' 'runtime error:' 'segmentation fault' 'fatal error:')

echo "=== Containers UX End-to-End Flow Challenge ==="
echo "  bin=$CT_BIN timeout=${TIMEOUT_SEC}s"

if [[ -z "$CT_BIN" ]] || [[ ! -x "$CT_BIN" ]]; then
    echo "[1/5] SKIP: CONTAINERS_BIN unset — SKIP-OK: #env-binary-missing"
    echo "=== Containers UX Challenge: PASSED (SKIP-OK) ==="
    exit 0
fi
echo "[1/5] Binary present: PASS"

assert_no_panic() {
    local label="$1" body="$2"
    for pat in "${USER_HOSTILE[@]}"; do
        # Bash's own regex operator, NOT `printf ... | grep -qE`. Under the
        # `set -o pipefail` at the top of this file that pipeline reports 141
        # when grep -q closes the pipe on a match, so a body over ~4 KiB made
        # THIS CHECK FAIL OPEN: the leak was found and the `&&` did not fire.
        if [[ $body =~ $pat ]]; then
            echo "  FAIL: $label leaked: $pat"
            return 1
        fi
    done
    # Explicit: without it the function's status is the last loop iteration's,
    # so a clean body whose last pattern simply did not match returned 1.
    return 0
}

help_out=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --help 2>&1 || timeout "$TIMEOUT_SEC" "$CT_BIN" -h 2>&1 || true)
assert_no_panic "--help" "$help_out" || exit 1
[[ -z "$help_out" ]] && { echo "[2/5] FAIL: empty help"; exit 1; }
echo "[2/5] Help discovery: PASS"

ver_out=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --version 2>&1 || timeout "$TIMEOUT_SEC" "$CT_BIN" -v 2>&1 || true)
assert_no_panic "--version" "$ver_out" || exit 1
echo "[3/5] Version surface: PASS"

set +e
bogus_out=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --does-not-exist-flag 2>&1)
bogus_exit=$?
set -e
assert_no_panic "bogus flag" "$bogus_out" || exit 1
[[ "$bogus_exit" -ge 124 ]] && { echo "[4/5] FAIL: crashed"; exit 1; }
echo "[4/5] Graceful recovery: PASS (exit $bogus_exit)"

post=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --help 2>&1 || timeout "$TIMEOUT_SEC" "$CT_BIN" -h 2>&1 || true)
assert_no_panic "post-error --help" "$post" || exit 1
[[ -z "$post" ]] && { echo "[5/5] FAIL: post-error help broken"; exit 1; }
echo "[5/5] Post-error liveness: PASS"

echo
echo "=== Containers UX Challenge: PASSED ==="
echo "  evidence: journey=discover→help→version→recover→post-liveness bogus_exit=$bogus_exit"
