# Containerfile for the crossbuild Wine-based Windows builder image.
# This image is the deliverable referenced by the operator
# provisioning procedure in
# `docs/crossbuild/windows-image-provisioning.md` of this submodule.
#
# Buildable on: Linux x86_64 with rootless podman or docker installed.
# NOT buildable on: macOS, FreeBSD (Wine-in-Docker layering breaks).
#
# Build command (rootless podman, Linux x86_64):
#   podman build -t ghcr.io/vasic-digital/crossbuild-wine:latest \
#     -f windows_wine.Containerfile .
#
# Verify:
#   podman run --rm ghcr.io/vasic-digital/crossbuild-wine:latest wine --version
#
# Once the image is on the host, pkg/crossbuild's
# WineContainerBackend.Build() succeeds end-to-end. Until then, that
# Backend honestly returns an error pointing at this file + the
# provisioning doc.

ARG BASE_IMAGE=debian:12-slim
FROM ${BASE_IMAGE}

LABEL org.opencontainers.image.title="crossbuild-wine"
LABEL org.opencontainers.image.description="Linux container running Wine + JDK 17 + Gradle for cross-compiling Windows artifacts"
LABEL org.opencontainers.image.source="https://github.com/vasic-digital/Containers"
LABEL org.opencontainers.image.licenses="See parent project LICENSE"
LABEL digital.vasic.containers.package="pkg/crossbuild"

ENV DEBIAN_FRONTEND=noninteractive
ENV WINEDEBUG=-all
ENV WINEPREFIX=/opt/wine
ENV PATH=/opt/gradle/bin:/opt/jdk/bin:${PATH}

RUN dpkg --add-architecture i386 \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg software-properties-common \
        unzip zip \
        wine wine64 wine32:i386 winbind \
        xvfb \
        fonts-dejavu fonts-liberation \
 && rm -rf /var/lib/apt/lists/*

# JDK 17 (Temurin) — Compose Desktop's jpackage target needs 17+.
RUN mkdir -p /opt/jdk \
 && curl -fsSL \
      https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.13%2B11/OpenJDK17U-jdk_x64_linux_hotspot_17.0.13_11.tar.gz \
    | tar -xz -C /opt/jdk --strip-components=1 \
 && /opt/jdk/bin/java -version

# Gradle 8.11.1 — matches the consuming project's wrapper version. Pinned so a
# transitive Gradle upgrade doesn't silently change build output.
ENV GRADLE_VERSION=8.11.1
RUN curl -fsSL https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip -o /tmp/gradle.zip \
 && unzip -q /tmp/gradle.zip -d /opt \
 && mv /opt/gradle-${GRADLE_VERSION} /opt/gradle \
 && rm /tmp/gradle.zip \
 && /opt/gradle/bin/gradle --version

# Wine prefix init — done once at build time so per-build container
# launches are fast.
RUN wine wineboot --init 2>/dev/null || true \
 && wineserver --wait

WORKDIR /work/src

# Default command is `gradle --version` so the orchestrator can do a
# fast image smoke check via `podman run --rm <image> gradle --version`.
CMD ["/opt/gradle/bin/gradle", "--version"]
