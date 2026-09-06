#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# The canonical shape: grep -q decides an if-condition under pipefail.
set -uo pipefail
body="$(cat /some/large/file)"
if echo "$body" | grep -q "=== PASS"; then
    echo "pass"
else
    echo "fail"
fi
