#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# Negated form: SIGPIPE flips the verdict to FAIL because the marker WAS found.
set -uo pipefail
out="$(run_the_thing 2>&1)"
if ! echo "${out}" | grep -q "FAIL=0"; then
    echo "no FAIL=0 line"; exit 1
fi
