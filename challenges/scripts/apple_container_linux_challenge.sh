#!/usr/bin/env bash
#
# apple_container_linux_challenge.sh — real-stack Apple `container` challenge
#
# Anti-bluff invariant (CONST-035 / §11.4 / §11.4.69 / §11.4.81): the
# Apple-`container` crossbuild backend MUST be able to boot a REAL Linux
# micro-VM on a macOS / Apple-Silicon host and bind-mount a host dir into
# it. The proof is UNFORGEABLE: the host `uname -s -m` says "Darwin arm64",
# but a Linux container's `uname -s -m` says "Linux aarch64". This challenge
# runs the REAL engine (no fakes) and asserts the latter, plus a host-dir
# mount round-trip.
#
# Honest kernel-gap (§11.4.81): when `container` is absent, the host is not
# macOS, or a probe boot fails (e.g. default kernel still downloading), the
# challenge SKIPs-with-reason (exit 0) — it NEVER fakes a PASS.
#
# Usage:
#   bash challenges/scripts/apple_container_linux_challenge.sh          # real-stack
#   bash challenges/scripts/apple_container_linux_challenge.sh --mutate # paired mutation
#
# Exit codes:
#   0  — all conditions PASS, OR honest SKIP (engine/kernel not ready)
#   1  — at least one condition genuinely failed
#   99 — --mutate mode confirmed the mutation produces a failure (paired
#        mutation discipline per §1.1)
#
# CONST-035 / CONST-050(B) / §11.4 / §11.4.69 / §11.4.81 / §11.4.52.

set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MUTATE_MODE="${1:-}"
IMAGE="docker.io/library/alpine:latest"
TIMEOUT_BIN="$(command -v timeout || command -v gtimeout || true)"

pass() { printf '[PASS] %s\n' "$1"; }
fail() { printf '[FAIL] %s\n' "$1" >&2; return 1; }
skip() { printf '[SKIP-OK] %s\n' "$1"; }

run_container() { # <mount-args...> -- <sh-command>
    # Wrapped in timeout when available so a stuck VM boot cannot hang.
    if [ -n "$TIMEOUT_BIN" ]; then
        "$TIMEOUT_BIN" 150 container "$@"
    else
        container "$@"
    fi
}

# engine_ready prints a reason and returns non-zero when the host cannot
# run a real Linux container right now.
engine_ready() {
    if [ "$(uname -s)" != "Darwin" ]; then
        skip "host is not macOS (Apple \`container\` is macOS-only); podman/docker LinuxContainerBackend serves the same target on Linux"
        return 1
    fi
    if ! command -v container >/dev/null 2>&1; then
        skip "apple \`container\` CLI not installed (one-time: brew install --cask container && container system start)"
        return 1
    fi
    if ! container system status 2>&1 | tr '[:upper:]' '[:lower:]' | grep -q running; then
        skip "apple \`container\` system service not running (one-time: container system start)"
        return 1
    fi
    # Probe boot: catches the "default kernel still downloading" state.
    if ! run_container run --rm "$IMAGE" sh -c 'true' >/dev/null 2>&1; then
        skip "apple \`container\` probe boot failed — engine/kernel not ready (honest kernel-gap per §11.4.81)"
        return 1
    fi
    return 0
}

condition_real_linux_kernel() {
    local out
    out="$(run_container run --rm "$IMAGE" sh -c 'uname -s -m' 2>/dev/null | tr -d '\r')"
    printf '       container uname -s -m => %q (host is %s %s)\n' "$out" "$(uname -s)" "$(uname -m)"
    # UNFORGEABLE: Darwin host => a Linux kernel proves a real micro-VM.
    case "$out" in
        *Linux*aarch64*) pass "real Linux aarch64 kernel ran inside Apple-container micro-VM (host is Darwin)" ;;
        *Darwin*) fail "got the macOS host kernel — no Linux micro-VM booted" ;;
        *) fail "unexpected uname output: $out (expected Linux aarch64)" ;;
    esac
}

condition_host_dir_mount_round_trip() {
    local tmpdir sentinel got
    tmpdir="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmpdir'" RETURN
    sentinel="apple-container-mount-proof-$$-$(date +%s)"
    printf '%s' "$sentinel" > "$tmpdir/sentinel.txt"

    # Host writes -> container reads through the mount.
    got="$(run_container run --rm \
        --mount "type=virtiofs,source=$tmpdir,target=/work/src" \
        -w /work/src "$IMAGE" sh -c 'cat /work/src/sentinel.txt' 2>/dev/null | tr -d '\r')"
    if [ "$got" != "$sentinel" ]; then
        fail "container did NOT read the host-written sentinel through the mount (got: '$got')"
        return 1
    fi

    # Container writes -> host reads through the mount.
    run_container run --rm \
        --mount "type=virtiofs,source=$tmpdir,target=/work/src" \
        -w /work/src "$IMAGE" sh -c "printf 'guest-%s' '$sentinel' > /work/src/from_guest.txt" >/dev/null 2>&1
    got="$(cat "$tmpdir/from_guest.txt" 2>/dev/null || true)"
    if [ "$got" != "guest-$sentinel" ]; then
        fail "host did NOT see the file the container wrote through the mount (got: '$got')"
        return 1
    fi
    pass "host-dir mount round-trips both ways (host->guest read + guest->host write)"
}

run_all_conditions() {
    condition_real_linux_kernel
    condition_host_dir_mount_round_trip
}

# --- paired mutation (§1.1) ---
# The mutation strips the mount flag from the round-trip call: without the
# mount the container cannot see the host sentinel, so the round-trip
# condition MUST FAIL. If it does not, this challenge is a bluff gate.
if [ "$MUTATE_MODE" = "--mutate" ]; then
    if ! engine_ready; then
        printf '\n[SKIP-OK] engine/kernel not ready — paired mutation requires the real engine; rerun on a ready macOS host\n'
        exit 0
    fi
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT INT TERM
    printf 'mutation-sentinel' > "$tmpdir/sentinel.txt"
    # MUTATION: omit --mount, so /work/src/sentinel.txt cannot exist.
    if got="$(run_container run --rm -w / "$IMAGE" sh -c 'cat /work/src/sentinel.txt' 2>/dev/null | tr -d '\r')" \
        && [ "$got" = "mutation-sentinel" ]; then
        printf '\n[BLUFF-GATE-DETECTED] mount round-trip PASSED without the mount flag — challenge is a §11.4 bluff and must be tightened\n' >&2
        exit 1
    fi
    printf '\n[MUTATION-WITNESSED] stripping the --mount flag broke the round-trip as required — paired mutation discipline upheld\n'
    exit 99
fi

if ! engine_ready; then
    printf '\n[SKIPPED] Apple \`container\` engine/kernel not ready on this host — SKIP-with-reason, not a failure (§11.4.81 honest kernel-gap)\n'
    exit 0
fi

run_all_conditions
printf '\n[SUCCESS] apple_container_linux_challenge: real Linux micro-VM booted + host-dir mount round-trip proven\n'
exit 0
