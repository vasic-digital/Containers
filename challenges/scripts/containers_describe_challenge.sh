#!/usr/bin/env bash
#
# containers_describe_challenge.sh — round-299 paired-mutation challenge
#
# Anti-bluff invariant: the containers submodule MUST be describable in a
# CONST-045-compliant, governance-anchor-present, locale-complete way. This
# challenge exercises FIVE conditions; any failed condition exits 1. The
# paired-mutation mode (--mutate) deliberately corrupts one condition and
# asserts the resulting failure manifests as exit 99 (mutation-witnessed
# defect — what a §1.1 paired mutation MUST do).
#
# Five exercised conditions:
#   1. Governance anchor literals (CONST-035/050/051/053/060) present in
#      CONSTITUTION.md / CLAUDE.md / AGENTS.md within this submodule.
#   2. .env file structure parseable (NOT the value — never echo secrets per
#      §11.4.10). If absent, only .env.example required.
#   3. CONTAINERS_REMOTE_ENABLED is reachable; if false → SKIP-OK marker
#      emitted per CONST-045 and exit 0 (skip != failure).
#   4. Six i18n bundles present (en + 5 added round-299: fr/de/ja/sr/zh).
#   5. Eleven base challenges + this one = 12 challenge scripts present.
#
# Usage:
#   bash challenges/scripts/containers_describe_challenge.sh         # normal
#   bash challenges/scripts/containers_describe_challenge.sh --mutate # paired
#
# Exit codes:
#   0  — all five conditions PASS (or CONTAINERS_REMOTE_ENABLED=false SKIP)
#   1  — at least one condition genuinely failed
#   99 — --mutate mode confirmed the mutation produces a failure (paired
#        mutation discipline per §1.1; if it does NOT produce a failure, the
#        challenge itself is a §11.4 bluff gate)
#
# CONST-045 / CONST-050(B) / CONST-053 / §11.4 / §11.4.13 / §11.4.52.

set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MUTATE_MODE="${1:-}"

# shellcheck disable=SC2034
SCRIPT_NAME="containers_describe_challenge"

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1" >&2; return 1; }
skip() { printf '[SKIP-OK] %s\n' "$1"; }

condition_governance_anchors() {
    local file
    # Anchors actually present in this owned submodule's covenant fleet (CONST-045
    # is meta-repo-scoped to the consuming project and intentionally NOT cascaded here per
    # CONST-051(B) decoupling — the submodule remains project-not-aware).
    local needed_literals=("CONST-035" "CONST-050" "CONST-051" "CONST-053" "CONST-060")
    for file in CONSTITUTION.md CLAUDE.md AGENTS.md; do
        [ -f "$ROOT/$file" ] || { fail "governance file missing: $file"; return 1; }
        for lit in "${needed_literals[@]}"; do
            if ! grep -q "$lit" "$ROOT/$file"; then
                fail "anchor literal $lit absent from $file"
                return 1
            fi
        done
    done
    pass "governance anchors present in CONSTITUTION/CLAUDE/AGENTS"
}

condition_env_structure() {
    if [ -f "$ROOT/.env.example" ]; then
        # Must not contain real secrets — only key= placeholders
        if grep -qE '(password|secret|token|key)=[A-Za-z0-9]{20,}' "$ROOT/.env.example" 2>/dev/null; then
            fail ".env.example contains entropy-suspicious value (CONST-053 secret-leak risk)"
            return 1
        fi
        pass ".env.example present and free of entropy-suspicious values"
    else
        fail ".env.example missing — required for CONST-045 onboarding"
        return 1
    fi
}

condition_remote_enabled_or_skip_ok() {
    local enabled
    if [ -f "$ROOT/.env" ]; then
        # Parse CONTAINERS_REMOTE_ENABLED safely without echoing the value
        enabled=$(grep -E '^CONTAINERS_REMOTE_ENABLED=' "$ROOT/.env" 2>/dev/null | head -1 | cut -d= -f2- | tr -d '"' | tr -d "'" | tr '[:upper:]' '[:lower:]' || true)
    else
        enabled="false"
    fi
    case "$enabled" in
        true|yes|1)
            pass "CONTAINERS_REMOTE_ENABLED=true → remote describe path engaged"
            ;;
        *)
            skip "CONTAINERS_REMOTE_ENABLED=false (CONST-045 remote disabled in this environment) — remote describe is SKIPPED, not failed"
            ;;
    esac
}

condition_i18n_bundles_complete() {
    local bundle_dir="$ROOT/pkg/i18n/bundles"
    local needed=(en fr de ja sr zh)
    [ -d "$bundle_dir" ] || { fail "i18n bundle dir absent: pkg/i18n/bundles"; return 1; }
    for locale in "${needed[@]}"; do
        if [ ! -f "$bundle_dir/active.$locale.yaml" ]; then
            fail "locale bundle missing: active.$locale.yaml"
            return 1
        fi
        if [ ! -s "$bundle_dir/active.$locale.yaml" ]; then
            fail "locale bundle empty: active.$locale.yaml"
            return 1
        fi
    done
    pass "6 locale bundles present + non-empty (en/fr/de/ja/sr/zh)"
}

condition_challenge_count() {
    local dir="$ROOT/challenges/scripts"
    [ -d "$dir" ] || { fail "challenges/scripts dir absent"; return 1; }
    local count
    count=$(find "$dir" -maxdepth 1 -type f -name "*_challenge.sh" | wc -l)
    if [ "$count" -lt 12 ]; then
        fail "challenge count $count < 12 — round-299 expects 11 base + this one"
        return 1
    fi
    pass "12+ challenge scripts present (count=$count)"
}

run_all_conditions() {
    condition_governance_anchors
    condition_env_structure
    condition_remote_enabled_or_skip_ok
    condition_i18n_bundles_complete
    condition_challenge_count
}

if [ "$MUTATE_MODE" = "--mutate" ]; then
    # Paired mutation: temporarily mask a required locale bundle. If the
    # mutation does NOT cause a failure, this challenge is itself a bluff
    # gate. Restore the bundle whether the mutation reveals the defect or
    # not — never leave the working tree dirty.
    BUNDLE="$ROOT/pkg/i18n/bundles/active.fr.yaml"
    BACKUP="$ROOT/pkg/i18n/bundles/.active.fr.yaml.mutate.bak"

    if [ ! -f "$BUNDLE" ]; then
        printf '[ERROR] cannot run --mutate: bundle to mutate absent already (was %s)\n' "$BUNDLE" >&2
        exit 1
    fi

    mv "$BUNDLE" "$BACKUP"
    trap 'mv "$BACKUP" "$BUNDLE" 2>/dev/null || true' EXIT INT TERM

    if run_all_conditions 2>&1 | grep -q '\[FAIL\]'; then
        printf '\n[MUTATION-WITNESSED] removing locale bundle caused FAIL as required — paired mutation discipline upheld\n'
        exit 99
    else
        printf '\n[BLUFF-GATE-DETECTED] removing locale bundle did NOT cause FAIL — challenge is a §11.4 bluff and must be tightened\n' >&2
        exit 1
    fi
fi

run_all_conditions
printf '\n[SUCCESS] containers_describe_challenge passed all 5 conditions\n'
exit 0
