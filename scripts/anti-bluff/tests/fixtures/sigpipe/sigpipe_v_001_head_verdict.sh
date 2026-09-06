#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# head closes the read end just as surely as grep -q does.
set -uo pipefail
while producer | head -n1; do
    echo loop
done
