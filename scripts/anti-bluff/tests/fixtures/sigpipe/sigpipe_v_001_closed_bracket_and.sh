#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# A CLOSED [[ ]] test joined to the pipeline by &&. The pipeline is still the
# deciding command; an earlier build of the detector wrongly treated any [[ on
# the line as "we are inside a test" and missed this shape entirely.
set -uo pipefail
if [[ $RUNNER_EXIT -eq 0 ]] && echo "$RUNNER_OUT" | grep -q "=== PASS"; then
    echo pass
else
    echo fail
fi
