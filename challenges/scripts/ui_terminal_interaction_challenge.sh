#!/usr/bin/env bash
# ui_terminal_interaction_challenge.sh — anti-bluff UI Challenge for
# the Containers submodule per CONST-035 + CONST-050(B). Submodule
# cascade per CONST-051(A). Drives the configured container tool
# binary non-interactively.

set -uo pipefail

CT_BIN="${CONTAINERS_BIN:-}"
TIMEOUT_SEC="${UI_TIMEOUT_SEC:-30}"
USER_HOSTILE=('panic:' 'goroutine [0-9]+ \[running\]:' 'runtime error:' 'segmentation fault' 'fatal error:')

echo "=== Containers UI Terminal-Interaction Challenge ==="
echo "  bin=$CT_BIN timeout=${TIMEOUT_SEC}s"

if [[ -z "$CT_BIN" ]] || [[ ! -x "$CT_BIN" ]]; then
    echo "[1/4] SKIP: CONTAINERS_BIN not set — SKIP-OK: #env-binary-missing"
    echo "=== Containers UI Challenge: PASSED (SKIP-OK) ==="
    exit 0
fi
echo "[1/4] Binary present: PASS"

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
[[ -z "$help_out" ]] && { echo "[2/4] FAIL: empty help"; exit 1; }
echo "[2/4] Help output: PASS"

ver_out=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --version 2>&1 || timeout "$TIMEOUT_SEC" "$CT_BIN" -v 2>&1 || true)
assert_no_panic "--version" "$ver_out" || exit 1
echo "[3/4] Version output: PASS"

set +e
bogus=$(timeout "$TIMEOUT_SEC" "$CT_BIN" --this-flag-does-not-exist 2>&1)
bogus_exit=$?
set -e
[[ "$bogus_exit" -ge 124 ]] && { echo "[4/4] FAIL: crashed"; exit 1; }
assert_no_panic "bogus flag" "$bogus" || exit 1
echo "[4/4] Invalid-flag: PASS (exit $bogus_exit)"

echo
echo "=== Containers UI Challenge: PASSED ==="
echo "  evidence: bin=$CT_BIN bogus_exit=$bogus_exit"
