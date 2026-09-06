#!/usr/bin/env bash
# §1.1 paired-mutation proof for the SIGPIPE-V gate.
#
# Every mutation here is DATA — a fixture script under tests/fixtures/sigpipe/
# carrying the bad pattern. Nothing in this harness edits the detector, so the
# proof cannot be made to pass by weakening the thing it is proving.
#
# Two directions, both required:
#   CATCH  — each defective fixture must produce its SIGPIPE-V id.
#   QUIET  — each clean fixture must produce nothing. A detector that flags the
#            prescribed fix, or the harmless shapes, gets switched off in a week,
#            so "does not fire" is as much a contract as "does fire".
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIX="${SCRIPT_DIR}/fixtures/sigpipe"
source "${SCRIPT_DIR}/../lib/shell_sigpipe.sh"

pass=0; fail=0

run_fixture() {
  local name="$1" expected_id="$2" file="${FIX}/$3"
  local out
  if [[ ! -f "$file" ]]; then
    echo "FAIL ${name}: fixture missing at ${file}"; fail=$((fail+1)); return
  fi
  out="$(scan_shell_sigpipe "${name}" "${file}" || true)"
  if [[ -z "$expected_id" ]]; then
    if [[ -n "$out" ]]; then
      echo "FAIL ${name}: expected NO hit, got:"; sed 's/^/        /' <<<"$out"
      fail=$((fail+1)); return
    fi
    echo "PASS ${name}: quiet, as required"
  else
    if [[ "$out" != *"${expected_id}"* ]]; then
      echo "FAIL ${name}: expected ${expected_id}, got: ${out:-<nothing>}"
      fail=$((fail+1)); return
    fi
    echo "PASS ${name}: caught ${expected_id}"
  fi
  pass=$((pass+1))
}

echo "=== CATCH: a defective fixture must be flagged ==="
run_fixture "M1 if + grep -q"          "SIGPIPE-V-001" sigpipe_v_001_if_grep_q.sh
run_fixture "M2 if ! + grep -q"        "SIGPIPE-V-001" sigpipe_v_001_if_negated.sh
run_fixture "M3 grep -q || exit"       "SIGPIPE-V-001" sigpipe_v_001_or_exit.sh
run_fixture "M4 while + head -n1"      "SIGPIPE-V-001" sigpipe_v_001_head_verdict.sh
run_fixture "M5 awk exit && ..."       "SIGPIPE-V-001" sigpipe_v_001_awk_exit.sh
run_fixture "M6 closed [[ ]] && pipe"  "SIGPIPE-V-001" sigpipe_v_001_closed_bracket_and.sh

echo
echo "=== QUIET: a harmless or already-fixed shape must NOT be flagged ==="
run_fixture "M7 no pipefail set"       ""              clean_no_pipefail.sh
run_fixture "M8 status discarded"      ""              clean_status_discarded.sh
run_fixture "M9 inside a heredoc"      ""              clean_heredoc_body.sh
run_fixture "M10 the prescribed fixes" ""              clean_fixed_forms.sh
run_fixture "M11 pipeline inside test" ""              clean_inside_test.sh

echo
echo "=== SIGPIPE-V self-test: ${pass} passed, ${fail} failed, 11 mutations ==="
if (( fail )); then
  echo "SIGPIPE-V paired-mutation proof FAILED"
  exit 1
fi
echo "SIGPIPE-V paired-mutation proof PASSED"
exit 0
