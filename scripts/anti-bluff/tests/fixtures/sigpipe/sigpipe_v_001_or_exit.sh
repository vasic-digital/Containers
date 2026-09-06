#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# Status consumed by || — the pipeline guards an exit.
set -euo pipefail
printf '%s' "$body" | grep -qE '"status"' || { echo "FAIL"; exit 1; }
