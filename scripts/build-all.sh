#!/bin/bash
# build-all.sh — build every command binary this module ships.
#
# The target list is DERIVED from the module, never hardcoded. It used to be a
# hardcoded list of three consuming-project binaries — ./cmd/core/,
# ./cmd/host-agent/discovery/ and ./cmd/host-agent/ — none of which exists here.
# That was two faults at once: a consumer's vocabulary compiled into a module
# that must stay consumer-agnostic (§11.4.28(B)), and a list free to drift away
# from the tree with nothing to catch it. Deriving the list removes both, because
# there is no longer a second place for the truth to live.
#
# Missing targets are reported ALL AT ONCE. The previous version aborted on the
# first one under `set -e`, so a run reported one broken target when three were
# broken — an error message that under-states the damage is its own defect.
#
# Paths are resolved from this script's location, never from $PWD: the old
# version did `mkdir -p bin` and `ls -la bin/` against the caller's working
# directory, so running it from anywhere but the module root silently created
# and listed a different bin/.
#
# Env:
#   BUILD_GOOS / BUILD_GOARCH   cross-compile target (default: host platform)
#   BUILD_TARGETS               space-separated override of the target list
#
# Exit:
#   0 = every target built
#   1 = a real failure — a named target does not exist, or a build failed
#   2 = could not determine — no Go toolchain, or ./cmd/... cannot be enumerated
#
# Contract guarded by: challenges/scripts/build_all_targets_challenge.sh

set -uo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

err() { printf '%s\n' "$*" >&2; }
undetermined() { printf 'UNDETERMINED: %s\n' "$*" >&2; exit 2; }

command -v go >/dev/null 2>&1 || undetermined "the Go toolchain is not on PATH"
[ -f "$ROOT/go.mod" ] || undetermined "no go.mod at $ROOT; this is not the module root"

# Every command package this module actually ships, asked of the toolchain
# rather than remembered. Overridable via BUILD_TARGETS for a partial build.
TARGETS=()
if [ -n "${BUILD_TARGETS:-}" ]; then
    read -r -a TARGETS <<< "$BUILD_TARGETS"
else
    mapfile -t TARGETS < <(
        cd "$ROOT" && go list -f '{{.ImportPath}}' ./cmd/... 2>/dev/null \
            | sed 's|^digital.vasic.containers/|./|'
    )
fi

if [ "${#TARGETS[@]}" -eq 0 ]; then
    undetermined "could not enumerate any ./cmd/... package under $ROOT"
fi

echo "=== Building ${#TARGETS[@]} command binary/binaries from $ROOT ==="

# Preflight EVERY target before building any, so one run reports the whole
# picture instead of stopping at the first fault.
missing=()
for t in "${TARGETS[@]}"; do
    [ -d "$ROOT/${t#./}" ] || missing+=( "$t" )
done
if [ "${#missing[@]}" -gt 0 ]; then
    err "ERROR: ${#missing[@]} build target(s) do not exist in this module:"
    for m in "${missing[@]}"; do err "    $m"; done
    err "Targets are derived from 'go list ./cmd/...'; a name here that does not"
    err "exist means BUILD_TARGETS was set to something this module does not ship."
    exit 1
fi

mkdir -p "$ROOT/bin" || { err "ERROR: cannot create $ROOT/bin"; exit 1; }

failed=()
for t in "${TARGETS[@]}"; do
    name="$(basename "$t")"
    echo "Building $name..."
    if ! ( cd "$ROOT" && \
           GOOS="${BUILD_GOOS:-}" GOARCH="${BUILD_GOARCH:-}" \
           go build -ldflags='-s -w' -o "$ROOT/bin/$name" "$t" ); then
        err "ERROR: build failed for $t"
        failed+=( "$t" )
    fi
done

if [ "${#failed[@]}" -gt 0 ]; then
    err "=== ${#failed[@]} of ${#TARGETS[@]} target(s) failed to build: ${failed[*]} ==="
    exit 1
fi

echo "=== Build complete: ${#TARGETS[@]} binary/binaries in $ROOT/bin ==="
ls -la "$ROOT/bin/"
exit 0
