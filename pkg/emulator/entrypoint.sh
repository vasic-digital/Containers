#!/usr/bin/env bash
# Containerfile entrypoint for the §6.X Android emulator image.
# PID 1 inside the container.
#
# Inputs (via env):
#   ANDROID_AVD_NAME  — AVD identifier set by Containerized.Boot
#                       (default: "default", the image's pre-baked AVD)
#   ANDROID_COLD_BOOT — "true" or "false" (§6.I clause 6 gating runs
#                       MUST be cold-boot; default: true)
#
# Anti-bluff posture: the emulator process IS PID 1. When the emulator
# exits (e.g. boot failure), the container exits, and `podman rm -f`
# in Containerized.Teardown sees "container already gone" rather than
# a stuck process. This eliminates the §6.B class of bluff where the
# container reports "Up" while the emulator inside is crash-looping.

set -euo pipefail

AVD_NAME="${ANDROID_AVD_NAME:-default}"
COLD_BOOT_FLAG=""
if [[ "${ANDROID_COLD_BOOT:-true}" == "true" ]]; then
    COLD_BOOT_FLAG="-no-snapshot"
fi

# Sanity-check the AVD exists in the image. Avd-not-found errors at
# `emulator -avd` time produce opaque exit codes; explicit pre-check
# gives a diagnostic message that surfaces in `podman logs`.
# Captured first, then matched with bash's own pattern operator: as a
# `... | grep -q` pipeline under the `set -o pipefail` above, a MATCH kills
# avdmanager with SIGPIPE (141) and pipefail promotes it, so `if !` would
# declare the AVD missing exactly when it was present.
avd_list="$(avdmanager list avd 2>/dev/null || true)"
if [[ $avd_list != *"Name: ${AVD_NAME}"* ]]; then
    echo "ERROR: AVD '${AVD_NAME}' not found in image. Available:" >&2
    avdmanager list avd 2>&1 >&2 || true
    exit 1
fi

echo "[§6.X-entrypoint] booting emulator avd=${AVD_NAME} cold-boot=${ANDROID_COLD_BOOT:-true}" >&2

# RC3 (2026-06-23 thinker.local blocker): the Android emulator's adbd
# binds CONTAINER-LOOPBACK 127.0.0.1:5555 — it has no flag to bind all
# interfaces. podman's `-p <host>:5575` forwards to the container's
# PUBLISHED interface (tap0/eth0 under rootless slirp4netns/pasta), not
# loopback, so a direct -p :5555 forward reaches nothing and host adb
# stalls at `offline`/timeout. socat bridges the published interface to
# the loopback adbd: it listens on 0.0.0.0:5575 (a DIFFERENT port, so no
# EADDRINUSE clash with adbd's 127.0.0.1:5555) and relays each connection
# to 127.0.0.1:5555. Containerized.Boot forwards the host ADB port to
# 5575. `fork` handles concurrent adb connections; `retry`/`forever` on
# the connect side tolerates socat starting before adbd is listening.
# Backgrounded so the emulator remains PID 1 (§6.B: container Up ⇔
# emulator alive); if the emulator exits, the container exits and socat
# dies with it.
socat TCP-LISTEN:5575,fork,reuseaddr "TCP:127.0.0.1:5555,retry=30,interval=2,forever" >&2 2>&1 &
echo "[§6.X-entrypoint] socat adb bridge 0.0.0.0:5575 -> 127.0.0.1:5555 started (pid $!)" >&2

# Start the emulator. -no-window for headless, -no-audio for the same
# reason, -no-boot-anim for boot speed. -gpu swiftshader_indirect uses
# the software renderer (the only choice without host GPU passthrough
# inside containers in the general case). The emulator's adbd binds
# 127.0.0.1:5555; the socat bridge above makes it reachable on the
# container's published interface for the host-side adb port forward.
exec emulator -avd "${AVD_NAME}" \
    -no-window \
    -no-audio \
    -no-boot-anim \
    -gpu swiftshader_indirect \
    -port 5554 \
    -read-only \
    ${COLD_BOOT_FLAG}
