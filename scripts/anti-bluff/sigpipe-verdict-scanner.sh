#!/usr/bin/env bash
# SIGPIPE-V gate — entry point.
#
# Walks tracked shell files and reports pipelines whose status decides a verdict
# while their last stage may close the read end early. See
# scripts/anti-bluff/lib/shell_sigpipe.sh for the mechanism, the measured
# threshold, and what is deliberately NOT flagged.
#
# Exit codes:
#   0  clean
#   1  new finding outside the baseline (gate failure)
#   2  baseline drift (a baselined finding is gone — the baseline is stale)
#   3  invocation error
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

MODE="all"
BASELINE="${ROOT_DIR}/challenges/baselines/sigpipe-verdict-baseline.txt"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)     MODE="$2"; shift 2 ;;
    --baseline) BASELINE="$2"; shift 2 ;;
    --root)     ROOT_DIR="$(cd "$2" && pwd)"; shift 2 ;;
    -h|--help)
      echo "usage: sigpipe-verdict-scanner.sh [--mode all|changed] [--baseline <path>] [--root <dir>]"
      exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 3 ;;
  esac
done

# shellcheck source=lib/shell_sigpipe.sh
source "${SCRIPT_DIR}/lib/shell_sigpipe.sh"

if [[ "$MODE" == "changed" ]]; then
  mapfile -t FILES < <(git -C "${ROOT_DIR}" diff --name-only HEAD 2>/dev/null)
  mapfile -t -O ${#FILES[@]} FILES < <(git -C "${ROOT_DIR}" diff --name-only --cached 2>/dev/null)
  mapfile -t -O ${#FILES[@]} FILES < <(git -C "${ROOT_DIR}" ls-files --others --exclude-standard 2>/dev/null)
elif [[ "$MODE" == "all" ]]; then
  mapfile -t FILES < <(git -C "${ROOT_DIR}" ls-files 2>/dev/null)
  mapfile -t -O ${#FILES[@]} FILES < <(git -C "${ROOT_DIR}" ls-files --others --exclude-standard 2>/dev/null)
else
  echo "invalid --mode: ${MODE}" >&2; exit 3
fi
mapfile -t FILES < <(printf '%s\n' "${FILES[@]}" | awk 'NF && !seen[$0]++')

HITS_FILE="$(mktemp -t sigpipe-v.XXXXXX)"
BASE_KEYS="$(mktemp -t sigpipe-v-base.XXXXXX)"
SEEN_KEYS="$(mktemp -t sigpipe-v-seen.XXXXXX)"
trap 'rm -f "${HITS_FILE}" "${BASE_KEYS}" "${SEEN_KEYS}" "${BASE_KEYS}.s" "${SEEN_KEYS}.s"' EXIT

for f in "${FILES[@]}"; do
  [[ -z "$f" ]] && continue
  fpath="${ROOT_DIR}/${f}"
  [[ -f "$fpath" ]] || continue

  # The gate's own fixture suite is deliberately defective by design.
  case "$f" in
    scripts/anti-bluff/tests/fixtures/*) continue ;;
  esac

  case "$f" in
    *.sh|*.bash|*.bats) ;;
    *) # extensionless files that are nonetheless shell. Read the first line
       # directly — a `head | head | grep -q` probe in this verdict position
       # is the very defect this gate exists to catch.
       shebang=""
       IFS= read -r shebang < "$fpath" 2>/dev/null || true
       [[ $shebang == '#!'*sh* ]] || continue ;;
  esac

  scan_shell_sigpipe "$f" "$fpath" >>"${HITS_FILE}" || true
done

if [[ -f "${BASELINE}" ]]; then
  grep -vE '^[[:space:]]*(#|$)' "${BASELINE}" > "${BASE_KEYS}" || true
else
  : > "${BASE_KEYS}"
fi
: > "${SEEN_KEYS}"

# A hit is "path:line:ID:context"; the stable key is "path:ID" — a line number
# moves whenever anything above it is edited, and a baseline that churns on
# every unrelated edit is a baseline nobody maintains.
NEW_HITS=0
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  key="$(awk -F: '{print $1 ":" $3}' <<<"$line")"
  if grep -qxF "${key}" "${BASE_KEYS}"; then
    echo "${key}" >> "${SEEN_KEYS}"
  else
    echo "$line"
    NEW_HITS=$((NEW_HITS+1))
  fi
done < "${HITS_FILE}"

DRIFT=0
sort -u "${SEEN_KEYS}" > "${SEEN_KEYS}.s"
sort -u "${BASE_KEYS}" > "${BASE_KEYS}.s"
mapfile -t STALE < <(comm -23 "${BASE_KEYS}.s" "${SEEN_KEYS}.s")
if [[ "$MODE" == "all" ]] && (( ${#STALE[@]} > 0 )); then
  echo "" >&2
  echo "WARN: ${#STALE[@]} baseline entr(ies) no longer present; baseline is stale." >&2
  printf '  %s\n' "${STALE[@]}" >&2
  DRIFT=1
fi

SCANNED=${#FILES[@]}
if (( NEW_HITS > 0 )); then
  echo "" >&2
  echo "FAIL: ${NEW_HITS} pipeline(s) decide a verdict while their last stage may close the pipe early." >&2
  echo "      Under 'set -o pipefail' the writer dies of SIGPIPE (141) BECAUSE the pattern matched," >&2
  echo "      so the check fails when it should pass. Use bash pattern matching in the verdict" >&2
  echo "      position instead:  [[ \$var == *pat* ]]  /  [[ \$var =~ \$re ]]  /  case \$var in ..." >&2
  exit 1
fi

if (( DRIFT > 0 )); then
  echo "" >&2
  echo "FAIL: baseline is stale (${#STALE[@]} entr(ies))." >&2
  exit 2
fi

echo "OK: SIGPIPE-V clean (mode=${MODE}, ${SCANNED} path(s) enumerated)." >&2
exit 0
