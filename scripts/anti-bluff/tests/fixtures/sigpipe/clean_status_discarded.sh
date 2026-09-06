#!/usr/bin/env bash
# CLEAN FIXTURE — must NOT be flagged.
# Real shapes from the corpus whose pipeline status is DISCARDED. Rewriting
# these would be churn in a gate script, which is its own risk.
set -uo pipefail
# assignment via command substitution — status goes nowhere
p50=$(printf '%s\n' "$sorted" | awk -v n="$total" 'NR==int(n*0.5){print; exit}')
state=$( { systemctl is-enabled "$tgt" 2>/dev/null || true; } | head -n1 | tr -d '[:space:]')
huge=$(head -c 8192 /dev/urandom | tr -dc 'A-Za-z0-9' | head -c 8192)
# bare statement inside a redirected group — nothing tests it
{
  printf '%s\n' "$VIOLATIONS" | head -30
} >&2
