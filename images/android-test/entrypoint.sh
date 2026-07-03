#!/bin/bash
set -e
API_LEVELS="28 29 30 31 33 34 35 36"
RESULTS_DIR="/test-results"
mkdir -p "$RESULTS_DIR"

for API in $API_LEVELS; do
    AVD="yole_test_api${API}"
    echo "=== Testing API ${API} on ${AVD} ==="
    emulator -avd "$AVD" -no-window -no-audio -gpu swiftshader_indirect &
    EMULATOR_PID=$!
    adb wait-for-device
    boot_completed=""
    while [[ "$boot_completed" != "1" ]]; do
        boot_completed=$(adb shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')
        sleep 2
    done
    adb install /apk/yole-android-debug.apk
    adb shell am instrument -w -r -e class digital.vasic.yole.android.SaveTests \
        digital.vasic.yole.android.test/androidx.test.runner.AndroidJUnitRunner \
        > "$RESULTS_DIR/api${API}_results.txt" 2>&1 || true
    adb emu kill
    wait $EMULATOR_PID 2>/dev/null || true
done

echo "All API levels tested. Results in $RESULTS_DIR"
