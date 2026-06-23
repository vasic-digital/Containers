# syntax=docker/dockerfile:1
# DS-0002 intentionally NOT cleared (root kept, justified): the Android emulator
# requires root at runtime — /dev/kvm device access for hardware acceleration plus
# write access to the root-owned SDK (/opt/android-sdk) and AVD store
# (/root/.android/avd). The emulator-base image is built around the root user;
# dropping to a non-root USER breaks KVM acceleration and AVD boot. Genuine root
# requirement, not an unhardened image (test-only emulator runner).
FROM ghcr.io/vasic-digital/android-emulator-base:latest

ENV ANDROID_SDK_ROOT=/opt/android-sdk
ENV EMULATOR_AVD_DIR=/root/.android/avd

RUN sdkmanager --install \
    "system-images;android-28;google_apis;x86_64" \
    "system-images;android-29;google_apis;x86_64" \
    "system-images;android-30;google_apis;x86_64" \
    "system-images;android-31;google_apis;x86_64" \
    "system-images;android-33;google_apis;x86_64" \
    "system-images;android-34;google_apis;x86_64" \
    "system-images;android-35;google_apis;x86_64" \
    "system-images;android-36;google_apis;x86_64"

COPY avds/ $EMULATOR_AVD_DIR/

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
