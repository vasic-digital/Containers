# syntax=docker/dockerfile:1
FROM ubuntu:24.04

# DS-0029: --no-install-recommends keeps the image minimal (no suggested extras).
RUN apt-get update && apt-get install -y --no-install-recommends \
    libnvidia-encode-550 \
    libva-drm2 \
    libva-x11-2 \
    mesa-vulkan-drivers \
    vulkan-tools \
    && rm -rf /var/lib/apt/lists/*

COPY bin/host-agent /host-agent

EXPOSE 47998-48000/udp

# DS-0002: run as an unprivileged user. The host-agent only reads world-readable
# host state (/proc, /sys) and serves UDP; it needs no root at runtime. NET_ADMIN
# for the WireGuard mesh is granted via the pod securityContext, not image root.
RUN groupadd --system --gid 10001 helix \
    && useradd --system --uid 10001 --gid 10001 --no-create-home helix
USER 10001:10001

ENTRYPOINT ["/host-agent"]
