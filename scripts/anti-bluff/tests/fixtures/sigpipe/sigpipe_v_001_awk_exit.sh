#!/usr/bin/env bash
# MUTATION FIXTURE — must be flagged SIGPIPE-V-001.
# awk with an early exit, status consumed by &&.
set -uo pipefail
cat big.txt | awk '/marker/{print; exit}' && echo "found"
