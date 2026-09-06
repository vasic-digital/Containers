#!/usr/bin/env bash
# CLEAN FIXTURE — must NOT be flagged.
# The prescribed fixes. If the gate fires on these it is telling people to
# undo the repair, and it will be switched off within a week.
set -uo pipefail
if [[ $body == *"=== PASS"* ]]; then echo pass; fi
if [[ ! $out == *"FAIL=0"* ]]; then echo "no FAIL=0"; exit 1; fi
[[ $body =~ \"status\"[[:space:]]*:[[:space:]]*\"(ok|healthy|UP)\" ]] || { echo FAIL; exit 1; }
case "$body" in *MARKER*) echo found ;; *) echo missing ;; esac
# a full-reading grep is also safe: without -q it consumes the whole stream
if grep -rn 'func .*Distribute' cmd/ 2>/dev/null | grep -E 'return 0, nil' >/dev/null; then
    echo "found"
fi
