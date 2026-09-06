#!/usr/bin/env bash
# CLEAN FIXTURE — must NOT be flagged.
# Here the pipeline really is INSIDE the test / a substitution, so its status
# is discarded and the trap cannot reach a verdict.
set -uo pipefail
if [[ "$(printf '%s\n' "$blob" | head -1)" == "expected" ]]; then echo ok; fi
if [ "$(cat f | head -1)" = x ]; then echo ok; fi
