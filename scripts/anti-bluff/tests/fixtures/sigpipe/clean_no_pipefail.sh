#!/usr/bin/env bash
# CLEAN FIXTURE — must NOT be flagged.
# Same dangerous shape, but pipefail is never set, so the trap cannot fire:
# the pipeline's status is grep's, and grep exits 0 on a match.
set -u
if echo "$body" | grep -q MARKER; then echo yes; fi
