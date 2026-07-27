// cmd/emulator-matrix — multi-AVD matrix runner for Android Compose UI
// tests. Constitutional anchor: parent-project clause 6.I (Multi-Emulator
// Container Matrix as Real-Device Equivalent) + clause 6.K
// (Builds-Inside-Containers Mandate). the consuming project's
// scripts/run-emulator-tests.sh becomes thin glue invoking this CLI
// after this package ships.
//
// Usage:
//
//	emulator-matrix \
//	  --android-sdk-root /opt/android-sdk \
//	  --apk releases/1.2.1/android-debug/app-debug.apk \
//	  --test-class app.example.challenges.Challenge01AppLaunchAndTrackerSelectionTest \
//	  --evidence-dir .ci-evidence/App-1.2.2 \
//	  --avds CZ_API28_Phone,CZ_API30_Phone,CZ_API34_Phone,Pixel_9a \
//	  --cold-boot \
//	  --image-manifest tools/ci-containers/vm-images.json
//
// Each comma-separated AVD entry MAY include the API level after a
// colon: `Pixel_9a:36:phone` (name:apiLevel:formFactor). The api level
// + form factor are recorded in the attestation file per clause 6.I
// clause 4.
//
// Exit codes:
//
//	0 — every AVD booted, every test passed (matrix attestation green)
//	1 — at least one AVD failed boot OR at least one test failed
//	2 — invalid CLI arguments OR the runner errored before producing
//	    any attestation rows
//
// Anti-bluff posture (clauses 6.J/6.L): a CLI that exits 0 when ANY
// AVD failed would be a bluff. The exit-code logic above means
// `tag.sh` can rely on `exit 0 ⇒ matrix passed`.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"digital.vasic.containers/pkg/emulator"
)

func parseAVDs(spec string) ([]emulator.AVD, error) {
	if spec == "" {
		return nil, fmt.Errorf("--avds MUST be a non-empty comma-separated list")
	}
	parts := strings.Split(spec, ",")
	avds := make([]emulator.AVD, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		fields := strings.Split(p, ":")
		avd := emulator.AVD{Name: fields[0]}
		if len(fields) > 1 {
			lvl, err := strconv.Atoi(strings.TrimSpace(fields[1]))
			if err != nil {
				return nil, fmt.Errorf("AVD %q: invalid api level %q", p, fields[1])
			}
			avd.APILevel = lvl
		}
		if len(fields) > 2 {
			avd.FormFactor = strings.TrimSpace(fields[2])
		}
		avds = append(avds, avd)
	}
	if len(avds) == 0 {
		return nil, fmt.Errorf("--avds parsed to zero AVDs")
	}
	return avds, nil
}

func main() {
	flagSdkRoot := flag.String("android-sdk-root", os.Getenv("ANDROID_SDK_ROOT"),
		"Host path to the Android SDK (default $ANDROID_SDK_ROOT)")
	flagAPK := flag.String("apk", "", "Host path to the debug APK to install")
	flagTestClass := flag.String("test-class", "",
		"Fully-qualified instrumentation test class")
	flagGradleModule := flag.String("gradle-module", "app",
		"Bare gradle module whose connectedDebugAndroidTest task runs "+
			"(e.g. \"app\", \"api-app\"); the runner targets "+
			":<module>:connectedDebugAndroidTest")
	flagEvidence := flag.String("evidence-dir", "",
		"Where to write per-AVD attestation rows (real-device-verification.json)")
	flagAVDs := flag.String("avds", "",
		"Comma-separated AVD list; entries MAY be 'Name:APILevel:FormFactor'")
	flagColdBoot := flag.Bool("cold-boot", true,
		"Disable snapshot reload (clause 6.I clause 6 — gating runs MUST cold-boot)")
	flagBootTimeout := flag.Duration("boot-timeout", 5*time.Minute,
		"Per-AVD cold-boot timeout")
	flagTestTimeout := flag.Duration("test-timeout", 10*time.Minute,
		"Per-test execution timeout")
	flagConcurrent := flag.Int("concurrent", 1,
		"Max concurrent emulators (default 1; values >1 set MatrixResult.Gating=false)")
	flagDev := flag.Bool("dev", false,
		"Developer-iteration mode; permits snapshot reload, sets MatrixResult.Gating=false")
	flagTestReportGlob := flag.String("test-report-glob", "",
		"Host-glob pattern (CWD-relative) for JUnit XML test reports; empty disables JUnit parsing")
	flagImageManifest := flag.String("image-manifest", "",
		"Optional path to a vm-images.json manifest. When set AND the AVD's required system-image is absent under ANDROID_SDK_ROOT, fetch via pkg/cache. Empty preserves the pre-Phase-B behavior.")
	// Phase 6 (Group C remaining) — per-row network simulation +
	// screenshot-on-failure capture.
	flagNetworkProfile := flag.String("network-profile", "",
		"Network conditions profile: edge|2g|3g|4g|lte|wifi|ethernet|none. Empty disables shaping.")
	flagNetworkBandwidthDown := flag.Int("network-bandwidth-down", 0,
		"Override down-link bandwidth (kbps) on top of --network-profile. 0 = use profile default.")
	flagNetworkBandwidthUp := flag.Int("network-bandwidth-up", 0,
		"Override up-link bandwidth (kbps) on top of --network-profile. 0 = use profile default.")
	flagNetworkLatency := flag.Int("network-latency", 0,
		"Override latency (ms) on top of --network-profile. 0 = use profile default.")
	flagNetworkLoss := flag.Float64("network-loss", 0,
		"Override packet-loss (%) on top of --network-profile. 0 = use profile default. Range: [0, 100].")
	flagCaptureScreenshot := flag.Bool("capture-screenshot-on-failure", true,
		"Capture a forensic screenshot when a row fails. Default true; set false to opt out.")

	// Parent project's clause 6.X (Container-Submodule Emulator Wiring Mandate,
	// added 2026-05-13) requires the emulator process to run INSIDE a
	// podman/docker container for gate runs. Until §6.X-debt closes
	// fully (Android-emulator container image baked + tested on Linux
	// x86_64), the CLI accepts the runner choice as a parameter and
	// records it in the attestation.
	//
	// "auto" resolves the OS-correct runner via emulator.ResolveRunner:
	// Linux→containerized (uses /dev/kvm in-container), macOS→host-direct
	// (Apple HVF is a host-only API a Linux container cannot reach),
	// Windows→host-direct (WHPX is likewise host-only). See accel.go.
	//
	// The literal default stays "host-direct" so existing callers that
	// do not pass --runner keep their pre-2026-05 behavior unchanged;
	// "auto" is opt-in.
	flagRunner := flag.String("runner", "host-direct",
		"Emulator runner: auto|host-direct|containerized. 'auto' picks the OS-correct runner (Linux→containerized, macOS/Windows→host-direct). §6.X gate runs require containerized; workstation iteration permits host-direct.")
	flagContainerImage := flag.String("container-image", "",
		"Container image for the Android emulator. Required when --runner=containerized.")
	flagContainerRuntime := flag.String("container-runtime", "podman",
		"Container runtime CLI: podman|docker. Used when --runner=containerized.")
	flag.Parse()

	if *flagAPK == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --apk is required")
		os.Exit(2)
	}
	if *flagTestClass == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --test-class is required")
		os.Exit(2)
	}
	if *flagEvidence == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --evidence-dir is required")
		os.Exit(2)
	}
	if *flagSdkRoot == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --android-sdk-root or $ANDROID_SDK_ROOT is required")
		os.Exit(2)
	}
	avds, err := parseAVDs(*flagAVDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(2)
	}

	// Phase 6: validate network override knobs before invoking the
	// matrix. Negative bandwidths and out-of-range loss are
	// configuration errors — fail fast (exit 2) rather than silently
	// applying nonsense to the emulator console.
	if *flagNetworkBandwidthDown < 0 || *flagNetworkBandwidthUp < 0 || *flagNetworkLatency < 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --network-bandwidth-down/--network-bandwidth-up/--network-latency MUST be >= 0")
		os.Exit(2)
	}
	if *flagNetworkLoss < 0 || *flagNetworkLoss > 100 {
		fmt.Fprintln(os.Stderr, "ERROR: --network-loss MUST be in [0, 100]")
		os.Exit(2)
	}

	// §6.X runner selection. host-direct preserves the pre-2026-05-13
	// behavior (used by workstation iteration). containerized routes
	// every emulator boot through the Containerized impl, which boots
	// inside a podman/docker container per §6.X clause 1. "auto"
	// resolves the OS-correct runner from runtime.GOOS — Linux uses
	// the in-container /dev/kvm path, macOS/Windows use host-direct
	// because their accelerators (HVF / WHPX) are host-only APIs a
	// Linux container cannot reach (see pkg/emulator/accel.go).
	resolvedRunner, rerr := emulator.ResolveRunner(*flagRunner, runtime.GOOS)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", rerr)
		os.Exit(2)
	}
	if *flagRunner == "auto" {
		profile := emulator.AccelProfileForOS(runtime.GOOS)
		fmt.Printf("[§6.X] runner=auto resolved to %s (accel=%s, goos=%s)\n",
			resolvedRunner, profile.Accel, runtime.GOOS)
	}
	// --container-image is required ONLY when the RESOLVED runner is
	// containerized — an "auto" resolution to host-direct (macOS,
	// Windows) does not need an image.
	if resolvedRunner == emulator.RunnerContainerized && *flagContainerImage == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --container-image is required when the resolved runner is containerized")
		os.Exit(2)
	}

	ctx := context.Background()
	// EMU-2: build a per-invocation Emulator FACTORY rather than a single
	// shared instance. Under Concurrent>1 the matrix runner hands each
	// concurrent AVD its OWN fresh Emulator, so a per-AVD Teardown always
	// acts on the container its own Boot created — a single shared
	// Containerized would let AVD-B's Boot overwrite the shared
	// containerName before AVD-A's Teardown, force-removing the wrong
	// container. The factory also isolates gradleModule per invocation
	// (EMU-3). Serial runs get a fresh instance per AVD too.
	var newEmu func() emulator.Emulator
	if resolvedRunner == emulator.RunnerContainerized {
		cfg := emulator.ContainerizedConfig{
			RuntimeBinary: *flagContainerRuntime,
			Image:         *flagContainerImage,
			GradleModule:  *flagGradleModule,
		}
		// Validate the config ONCE up front (fail-loud) so the factory
		// below operates on already-validated, constant inputs.
		if _, ferr := emulator.NewContainerized(cfg); ferr != nil {
			fmt.Fprintf(os.Stderr, "ERROR: NewContainerized: %v\n", ferr)
			os.Exit(2)
		}
		newEmu = func() emulator.Emulator {
			c, ferr := emulator.NewContainerized(cfg)
			if ferr != nil {
				// Unreachable: cfg was validated non-empty above and is
				// constant, so NewContainerized cannot fail here. Fail loud
				// on any invariant break rather than return a nil Emulator.
				panic(fmt.Sprintf("emulator-matrix: NewContainerized unexpectedly failed on validated config: %v", ferr))
			}
			return c
		}
		fmt.Printf("[§6.X] runner=containerized image=%s runtime=%s\n",
			*flagContainerImage, *flagContainerRuntime)
	} else {
		sdkRoot := *flagSdkRoot
		gradleModule := *flagGradleModule
		newEmu = func() emulator.Emulator {
			return emulator.NewAndroidEmulatorWithModule(sdkRoot, gradleModule)
		}
		// host-direct is OS-correct on macOS (HVF) and Windows (WHPX) —
		// there it IS the accelerated, gate-eligible runner, not a
		// workstation-iteration fallback. It is a workstation-iteration
		// choice only on an OS whose OS-correct runner is containerized
		// (Linux — picking host-direct there skips KVM-in-container).
		if emulator.GateEligibleForOS(resolvedRunner, runtime.GOOS) {
			profile := emulator.AccelProfileForOS(runtime.GOOS)
			fmt.Printf("[§6.X] runner=host-direct is the OS-correct accelerated gate runner for %s (accel=%s)\n",
				runtime.GOOS, profile.Accel)
		} else {
			fmt.Println("[§6.X] runner=host-direct (workstation iteration mode; not gate-eligible on this OS — §6.X gate runs require containerized)")
		}
	}
	runner := emulator.NewAndroidMatrixRunnerWithFactory(newEmu)
	result, err := runner.RunMatrix(ctx, emulator.MatrixConfig{
		AVDs:              avds,
		AndroidSdkRoot:    *flagSdkRoot,
		APKPath:           *flagAPK,
		TestClass:         *flagTestClass,
		GradleModule:      *flagGradleModule,
		EvidenceDir:       *flagEvidence,
		BootTimeout:       *flagBootTimeout,
		TestTimeout:       *flagTestTimeout,
		ColdBoot:          *flagColdBoot,
		Concurrent:        *flagConcurrent,
		Dev:               *flagDev,
		TestReportGlob:    *flagTestReportGlob,
		ImageManifestPath: *flagImageManifest,
		NetworkProfile:    *flagNetworkProfile,
		NetworkOverride: emulator.NetworkConditions{
			DownKbps:    *flagNetworkBandwidthDown,
			UpKbps:      *flagNetworkBandwidthUp,
			LatencyMS:   *flagNetworkLatency,
			LossPercent: *flagNetworkLoss,
		},
		CaptureScreenshotOnFailure: *flagCaptureScreenshot,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: matrix runner failed: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("Matrix run finished. Attestation: %s\n", result.AttestationFile)
	for i, t := range result.Tests {
		status := "PASS"
		if !t.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%d] %-25s api=%d %s %s\n",
			i+1, t.AVD.Name, t.AVD.APILevel, t.AVD.FormFactor, status)
		if t.Error != nil {
			fmt.Printf("       error: %v\n", t.Error)
		}
	}
	if result.Gating {
		fmt.Println("Gating: TRUE  (serial run, --dev=false — clause-6.I-clause-7-eligible)")
	} else {
		fmt.Println("Gating: FALSE (--concurrent>1 OR --dev — tag.sh will refuse this attestation)")
	}
	if !result.AllPassed() {
		fmt.Fprintln(os.Stderr,
			"MATRIX FAILED — at least one AVD did not pass. tag.sh MUST refuse this commit.")
		os.Exit(1)
	}
	fmt.Println("MATRIX PASSED — every AVD booted and every test passed.")
}
