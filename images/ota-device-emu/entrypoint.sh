#!/bin/sh
# entrypoint.sh — OTA device-emulator container entrypoint.
#
# Purpose
#   Run a consumer-supplied statically-built `ota-device-emu` binary that
#   speaks an OTA control-plane's HTTP protocol, impersonating one OTA target
#   ("device") for Tier-1 control-plane testing — replacing live hardware.
#
# Contract
#   The `ota-device-emu` binary is NOT built here. The consuming project
#   (an OTA control-plane consumer) builds it as a static Linux binary and supplies it
#   to this image by ONE of:
#     (a) volume-mount at run time:
#           podman run -v $PWD/bin/ota-device-emu:/usr/local/bin/ota-device-emu:ro ...
#     (b) bind/COPY-override into a derived image (FROM ota-device-emu:dev).
#   Either way the binary MUST land at OTA_DEVICE_EMU_BIN (default
#   /usr/local/bin/ota-device-emu) and be executable for the runtime user.
#
# Inputs (environment — the env-var interface the consumer configures)
#   OTA_BASE_URL          (required) control-plane base URL, e.g. http://control-plane:8080
#   OTA_ADMIN_USER        (optional) admin/enrollment user, if the binary authenticates
#   OTA_ADMIN_PASS        (optional) admin/enrollment password (NEVER logged — §11.4.10)
#   OTA_HARDWARE_ID       (optional) stable hardware identifier this device reports
#   OTA_MODEL             (optional) device model string, e.g. orangepi-5-max
#   OTA_OS                (optional) device OS string, e.g. android / linux
#   OTA_CURRENT_VERSION   (optional) version the emulated device currently runs
#   OTA_DEVICE_EMU_BIN    (optional) path to the binary (default /usr/local/bin/ota-device-emu)
#   OTA_DEVICE_EMU_EXTRA_ARGS (optional) extra args appended to the binary invocation
#
# Outputs / Side-effects
#   Execs the binary (PID 1 hand-off via `exec`). All binary stdout/stderr is
#   the container log. No files written by this wrapper.
#
# Behaviour
#   `--help` / `-h` (or no binary present + no args): print this contract and exit 0.
#   Otherwise: validate OTA_BASE_URL is set, confirm the binary exists+executable,
#   then `exec` it. A missing binary or missing OTA_BASE_URL is a clear, honest
#   non-zero exit — never a silent success.
#
# Dependencies: POSIX sh (alpine /bin/sh). No bashisms (Constitution §11.4.67).
# Cross-references: images/ota-device-emu/Dockerfile, docs/ota-device-emulation.md,
#   cmd/ota-device-emu-boot/main.go.

set -eu

BIN="${OTA_DEVICE_EMU_BIN:-/usr/local/bin/ota-device-emu}"

print_contract() {
    cat <<'EOF'
ota-device-emu — OTA device-emulator container

This image provides the RUNTIME + entrypoint for a consumer-supplied
`ota-device-emu` binary that impersonates one OTA target device against an
OTA control plane (Tier-1 control-plane protocol testing — NOT a real Android
A/B update_engine/AVB flow; that needs Cuttlefish-on-Linux-KVM, out of scope).

The binary itself is built in the consuming project and supplied via:
  podman run -v ./bin/ota-device-emu:/usr/local/bin/ota-device-emu:ro \
    -e OTA_BASE_URL=http://control-plane:8080 \
    -e OTA_HARDWARE_ID=emu-001 -e OTA_MODEL=orangepi-5-max \
    -e OTA_OS=android -e OTA_CURRENT_VERSION=1.0.0 \
    ota-device-emu:dev

Environment interface:
  OTA_BASE_URL          (required) control-plane base URL
  OTA_ADMIN_USER        (optional) enrollment/admin user
  OTA_ADMIN_PASS        (optional) enrollment/admin password (never logged)
  OTA_HARDWARE_ID       (optional) stable hardware id this device reports
  OTA_MODEL             (optional) device model
  OTA_OS                (optional) device OS (android/linux)
  OTA_CURRENT_VERSION   (optional) currently-installed version
  OTA_DEVICE_EMU_BIN    (optional) binary path (default /usr/local/bin/ota-device-emu)
  OTA_DEVICE_EMU_EXTRA_ARGS (optional) extra args appended to the binary

On-demand boot/health: a consumer uses digital.vasic.containers pkg/boot +
pkg/health to bring this image up and health-check it. See
cmd/ota-device-emu-boot/main.go and docs/ota-device-emulation.md.
EOF
}

# --help / -h with no binary or as first arg: print the contract.
case "${1:-}" in
    -h|--help)
        print_contract
        exit 0
        ;;
esac

# If the binary is absent, `--help`-style invocation still succeeds (so the
# image is self-documenting), but an attempt to actually run is an honest error.
if [ ! -x "$BIN" ]; then
    print_contract
    echo >&2
    echo "ERROR: ota-device-emu binary not found or not executable at: $BIN" >&2
    echo "       Supply it via volume mount or a derived image. See contract above." >&2
    exit 3
fi

if [ -z "${OTA_BASE_URL:-}" ]; then
    echo "ERROR: OTA_BASE_URL is required but unset. See: $0 --help" >&2
    exit 2
fi

# Hand off to the consumer binary as PID 1. Pass through any explicit args,
# plus optional OTA_DEVICE_EMU_EXTRA_ARGS (word-split intentionally).
# shellcheck disable=SC2086
exec "$BIN" "$@" ${OTA_DEVICE_EMU_EXTRA_ARGS:-}
