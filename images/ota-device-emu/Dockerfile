# syntax=docker/dockerfile:1
#
# ota-device-emu — OTA device-emulator runtime image.
#
# Provides the RUNTIME (CA certs + shell + entrypoint) for a consumer-supplied
# statically-built `ota-device-emu` Go binary. The binary speaks an OTA control
# plane's HTTP protocol and impersonates ONE OTA target device for Tier-1
# control-plane testing (replacing live hardware). The binary is built in the
# consuming project (an OTA control-plane consumer) — this image does NOT build it.
#
# Supplying the binary (pick one):
#   (a) volume mount at run time (recommended for dev):
#         podman run -v ./bin/ota-device-emu:/usr/local/bin/ota-device-emu:ro \
#           -e OTA_BASE_URL=http://control-plane:8080 ota-device-emu:dev
#   (b) derive an image that COPYs the binary in:
#         FROM ota-device-emu:dev
#         COPY bin/ota-device-emu /usr/local/bin/ota-device-emu
#
# Base: alpine (tiny, multi-arch arm64+x86_64, has /bin/sh + ca-certificates).
# A static Go binary (CGO_ENABLED=0) needs no glibc; ca-certificates lets the
# binary reach an https control plane.
#
# Env-var interface + on-demand-boot contract: see entrypoint.sh and
# docs/ota-device-emulation.md.

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && update-ca-certificates

# Non-root by default (Constitution: rootless-only; no sudo/su anywhere).
RUN addgroup -S ota && adduser -S -G ota ota

# Build context is this directory (images/ota-device-emu/), so entrypoint.sh
# is copied by its context-relative name. Build with:
#   podman build -f images/ota-device-emu/Dockerfile \
#     -t ota-device-emu:dev images/ota-device-emu/
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Consumer-supplied binary lands here (volume mount or derived-image COPY).
ENV OTA_DEVICE_EMU_BIN=/usr/local/bin/ota-device-emu

USER ota

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
