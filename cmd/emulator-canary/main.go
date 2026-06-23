// cmd/emulator-canary — release cold-start canary runner.
//
// Installs a release-signed APK on a cold-booted Android emulator,
// launches its main activity, observes logcat for FATAL crashes, and
// writes a canary attestation JSON. Designed to close the §6.Z tooling
// gap: debug-signed androidTest APKs cannot test release-signed APKs
// (signature mismatch), so this command exercises the release build
// directly without an instrumentation test harness.
//
// Usage:
//
//	emulator-canary \
//	  --android-sdk-root /opt/android-sdk \
//	  --apk releases/1.2.36/android-release/app-release.apk \
//	  --package digital.vasic.app.client \
//	  --activity .MainActivity \
//	  --avd Pixel_8:35:phone \
//	  --evidence-dir .ci-evidence/canary-1.2.36 \
//	  --cold-boot
//
// AVD format: Name[:APILevel[:FormFactor]] — identical to emulator-matrix.
//
// Exit codes:
//
//	0 — activity reached RESUMED state AND no FATAL in logcat (canary PASS)
//	1 — activity did not resume OR FATAL detected (canary FAIL)
//	2 — configuration error (missing required flags, boot failure, etc.)
//
// Anti-bluff posture (clauses 6.J/6.L): exit 0 MEANS "activity resumed
// AND no FATAL". A bare "app installed without crashing" is NOT exit 0 —
// that is the §6.Z bluff the canary was designed to prevent.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"digital.vasic.containers/pkg/emulator"
)

func parseAVDSpec(spec string) emulator.AVD {
	parts := strings.Split(spec, ":")
	avd := emulator.AVD{Name: parts[0]}
	if len(parts) > 1 {
		if lvl, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			avd.APILevel = lvl
		}
	}
	if len(parts) > 2 {
		avd.FormFactor = strings.TrimSpace(parts[2])
	}
	return avd
}

func main() {
	flagSDK := flag.String("android-sdk-root", os.Getenv("ANDROID_SDK_ROOT"),
		"Host path to the Android SDK (default $ANDROID_SDK_ROOT)")
	flagAPK := flag.String("apk", "",
		"Host path to the release APK to install (required)")
	flagPkg := flag.String("package", "",
		"Android package name of the APK (required, e.g. digital.vasic.lava.client)")
	flagActivity := flag.String("activity", ".MainActivity",
		"Fully-qualified activity to launch (e.g. .MainActivity)")
	flagAVD := flag.String("avd", "",
		"AVD spec: Name[:APILevel[:FormFactor]] (required)")
	flagEvidence := flag.String("evidence-dir", "",
		"Directory to write canary attestation JSON + logcat (required)")
	flagColdBoot := flag.Bool("cold-boot", true,
		"Disable snapshot reload (clause 6.I clause 6 — gating canary runs MUST cold-boot)")
	flagBootTimeout := flag.Duration("boot-timeout", 5*time.Minute,
		"Per-AVD cold-boot timeout")
	flagActivityTimeout := flag.Duration("activity-timeout", 30*time.Second,
		"How long to wait for the activity to reach RESUMED state")
	flagLogcatWindow := flag.Duration("logcat-window", 10*time.Second,
		"Duration of logcat to observe after activity launch")
	flagJSON := flag.Bool("json", false,
		"Print the CanaryResult JSON to stdout on completion (in addition to the attestation file)")

	flag.Parse()

	if *flagSDK == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --android-sdk-root or $ANDROID_SDK_ROOT is required")
		os.Exit(2)
	}
	if *flagAPK == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --apk is required")
		os.Exit(2)
	}
	if *flagPkg == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --package is required")
		os.Exit(2)
	}
	if *flagAVD == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --avd is required")
		os.Exit(2)
	}
	if *flagEvidence == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --evidence-dir is required")
		os.Exit(2)
	}

	avd := parseAVDSpec(*flagAVD)

	cfg := emulator.CanaryConfig{
		AndroidSdkRoot:  *flagSDK,
		APKPath:         *flagAPK,
		PackageName:     *flagPkg,
		LaunchActivity:  *flagActivity,
		AVD:             avd,
		EvidenceDir:     *flagEvidence,
		ColdBoot:        *flagColdBoot,
		BootTimeout:     *flagBootTimeout,
		ActivityTimeout: *flagActivityTimeout,
		LogcatWindow:    *flagLogcatWindow,
	}

	ctx := context.Background()
	result, err := emulator.RunCanary(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: canary run failed: %v\n", err)
		os.Exit(2)
	}

	if *flagJSON {
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(b))
	}

	fmt.Printf("Canary run finished. Attestation: %s\n", result.AttestationFile)
	fmt.Printf("  AVD:              %s (API %d)\n", result.AVDName, result.APILevel)
	fmt.Printf("  Activity resumed: %v\n", result.ActivityResumed)
	fmt.Printf("  Fatal detected:   %v\n", result.FatalDetected)
	fmt.Printf("  Passed:           %v\n", result.Passed)
	if result.Error != "" {
		fmt.Printf("  Error:            %s\n", result.Error)
	}

	if !result.Passed {
		fmt.Fprintln(os.Stderr,
			"CANARY FAILED — activity did not resume OR FATAL crash detected.")
		os.Exit(1)
	}
	fmt.Println("CANARY PASSED — activity resumed, no FATAL crash detected.")
}
