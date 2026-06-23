# Containerfile for the crossbuild Linux builder image (JDK 17 +
# Gradle, no Wine). Image is the runtime that pkg/crossbuild's
# LinuxContainerBackend uses to produce .deb / .rpm / Linux
# jpackage runtime images on any host with rootless podman/docker.
#
# Two variants produced from this Containerfile via different
# --build-arg combinations:
#
#   - jdk17-amd64  (default; for x86_64 Linux artifacts on Apple
#     Silicon hosts costs emulation overhead but is the lingua
#     franca target)
#   - jdk17-arm64  (native on Apple Silicon; faster but produces
#     arm64 .deb / runtime images)
#
# Build commands (rootless podman on any host with the runtime):
#
#   podman build \
#       --platform linux/amd64 \
#       -t ghcr.io/vasic-digital/crossbuild-linux:jdk17-amd64 \
#       -f linux_container.Containerfile .
#
#   podman build \
#       --platform linux/arm64 \
#       -t ghcr.io/vasic-digital/crossbuild-linux:jdk17-arm64 \
#       -f linux_container.Containerfile .
#
# Verify (per architecture):
#
#   podman run --rm ghcr.io/vasic-digital/crossbuild-linux:jdk17-amd64 \
#       gradle --version
#
# Once the image is on the host, pkg/crossbuild's
# LinuxContainerBackend.Build() succeeds end-to-end.

ARG BASE_IMAGE=debian:12-slim
FROM ${BASE_IMAGE}

LABEL org.opencontainers.image.title="crossbuild-linux"
LABEL org.opencontainers.image.description="JDK 17 + Gradle container for cross-building Linux .deb/.rpm/runtime-image artifacts"
LABEL org.opencontainers.image.source="https://github.com/vasic-digital/Containers"
LABEL org.opencontainers.image.licenses="See parent project LICENSE"
LABEL digital.vasic.containers.package="pkg/crossbuild"

ENV DEBIAN_FRONTEND=noninteractive
ENV PATH=/opt/gradle/bin:/opt/jdk/bin:${PATH}

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg \
        unzip zip \
        binutils fakeroot rpm \
        fonts-dejavu fonts-liberation \
 && rm -rf /var/lib/apt/lists/*

# JDK 17 (Temurin). Architecture detected at build time via uname -m
# so the same Containerfile produces both amd64 and arm64 images
# without conditionals in build args.
RUN ARCH="$(uname -m)" \
 && case "$ARCH" in \
        x86_64)  URL="https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.13%2B11/OpenJDK17U-jdk_x64_linux_hotspot_17.0.13_11.tar.gz" ;; \
        aarch64) URL="https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.13%2B11/OpenJDK17U-jdk_aarch64_linux_hotspot_17.0.13_11.tar.gz" ;; \
        *)       echo "unsupported arch $ARCH" >&2; exit 1 ;; \
    esac \
 && mkdir -p /opt/jdk \
 && curl -fsSL "$URL" | tar -xz -C /opt/jdk --strip-components=1 \
 && /opt/jdk/bin/java -version

# Gradle 8.11.1 — pinned to match the consuming project's gradle/wrapper version.
ENV GRADLE_VERSION=8.11.1
RUN curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/gradle.zip \
 && unzip -q /tmp/gradle.zip -d /opt \
 && mv "/opt/gradle-${GRADLE_VERSION}" /opt/gradle \
 && rm /tmp/gradle.zip \
 && /opt/gradle/bin/gradle --version

WORKDIR /work/src
CMD ["/opt/gradle/bin/gradle", "--version"]
