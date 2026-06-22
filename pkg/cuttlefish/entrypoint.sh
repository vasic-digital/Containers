#!/usr/bin/env bash
# Containerfile entrypoint for the Cuttlefish (cvd) image. Runs launch_cvd
# as the foreground PID 1 so the container's lifetime IS the cvd instance's
# lifetime — when launch_cvd exits (or `stop_cvd` is exec'd in), the
# container exits. This is the §6.B-aware pattern pkg/emulator/entrypoint.sh
# established: container "Up" actually reflects the VM running, never a
# crash-looped shadow.
#
# IMPORTANT (§11.4.161 documented exception): this entrypoint only works in
# a ROOTFUL + PRIVILEGED container with /dev/kvm + the vhost/vsock/tun
# device nodes + --network host. pkg/cuttlefish.buildContainerRunArgs
# supplies those flags. Without them launch_cvd cannot make the cvd-ebr
# bridge and exits non-zero (surfaced in `podman logs`).
#
# Inputs (via env):
#   CF_IMG_URL        — optional URL of the device-image zip to fetch into
#                       $HOME at run time (when not baked at build time).
#   CF_HOST_PKG_URL   — optional URL of cvd-host_package.tar.gz (same build).
#   CF_BASE_INSTANCE  — optional 1-based cvd instance number (default: cvd
#                       default). Resolved by stable instance NUMBER, never
#                       a discovered index (§11.4.111).
#   CF_DAEMON         — "true" (default) runs launch_cvd --daemon then keeps
#                       PID 1 alive tailing the cvd launcher log; "false"
#                       runs launch_cvd in the foreground directly.

set -euo pipefail

cd "${HOME:-/cuttlefish}"

# Fetch artifacts at run time if URLs are provided and not already present.
if [[ -n "${CF_IMG_URL:-}" && ! -e super.img && ! -e system.img ]]; then
    echo "[cvd-entrypoint] fetching device image: ${CF_IMG_URL}" >&2
    curl -fsSL "${CF_IMG_URL}" -o cf-img.zip
    unzip -q cf-img.zip
    rm -f cf-img.zip
fi
if [[ -n "${CF_HOST_PKG_URL:-}" && ! -x bin/launch_cvd ]]; then
    echo "[cvd-entrypoint] fetching host package: ${CF_HOST_PKG_URL}" >&2
    curl -fsSL "${CF_HOST_PKG_URL}" -o cvd-host_package.tar.gz
    tar -xzf cvd-host_package.tar.gz
    rm -f cvd-host_package.tar.gz
fi

# Resolve the launch_cvd binary: prefer the host-package-extracted
# ./bin/launch_cvd (FACT, §4.3), fall back to the cuttlefish-base PATH entry.
LAUNCH_CVD="./bin/launch_cvd"
if [[ ! -x "${LAUNCH_CVD}" ]]; then
    LAUNCH_CVD="$(command -v launch_cvd || true)"
fi
if [[ -z "${LAUNCH_CVD}" || ! -x "${LAUNCH_CVD}" ]]; then
    echo "ERROR: launch_cvd not found (no ./bin/launch_cvd and not on PATH)." >&2
    echo "       Provide CF_HOST_PKG_URL or bake cvd-host_package.tar.gz at build time." >&2
    exit 1
fi

BASE_INSTANCE_FLAG=()
if [[ -n "${CF_BASE_INSTANCE:-}" ]]; then
    BASE_INSTANCE_FLAG=("--base_instance_num=${CF_BASE_INSTANCE}")
fi

echo "[cvd-entrypoint] launching cvd via ${LAUNCH_CVD} (daemon=${CF_DAEMON:-true})" >&2

if [[ "${CF_DAEMON:-true}" == "true" ]]; then
    # Daemonised launch, then keep PID 1 alive by tailing the launcher log
    # so the container lives until stop_cvd (exec'd in by Cuttlefish.Stop)
    # or `podman rm -f` tears it down.
    HOME="${HOME:-/cuttlefish}" "${LAUNCH_CVD}" --daemon "${BASE_INSTANCE_FLAG[@]}"
    LOG="${HOME:-/cuttlefish}/cuttlefish_runtime/launcher.log"
    echo "[cvd-entrypoint] cvd daemonised; tailing ${LOG} to keep PID 1 alive" >&2
    # Wait for the log to exist, then tail it as PID 1.
    for _ in $(seq 1 30); do
        [[ -e "${LOG}" ]] && break
        sleep 1
    done
    exec tail -F "${LOG}"
fi

# Foreground launch — launch_cvd itself is PID 1.
exec env HOME="${HOME:-/cuttlefish}" "${LAUNCH_CVD}" "${BASE_INSTANCE_FLAG[@]}"
