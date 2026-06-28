// cmd/vm-matrix — multi-target QEMU matrix runner. Mirrors
// cmd/emulator-matrix's CLI shape; emits the same attestation row
// schema so scripts/tag.sh's 3 Group B gates work unchanged.
//
// Usage:
//
//	vm-matrix \
//	  --image-manifest tools/ci-containers/vm-images.json \
//	  --targets alpine-3.20-x86_64,debian-12-x86_64,fedora-40-x86_64 \
//	  --uploads /host/proxy.jar:/tmp/proxy.jar,/host/binary:/tmp/binary \
//	  --script tests/vm-distro/boot-and-probe.sh \
//	  --captures /tmp/probe-output.json:probe-output.json \
//	  --evidence-dir .ci-evidence/App-Android-1.2.1-127/vm-distro \
//	  --concurrent 1 --cold-boot
//
// Exit codes:
//
//	0 — every target booted, every script exited 0, attestation written
//	1 — at least one target failed
//	2 — invalid CLI args OR runner errored before producing any rows
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"digital.vasic.containers/pkg/cache"
	"digital.vasic.containers/pkg/vm"
)

func parseTargets(spec string) ([]vm.VMTarget, error) {
	if spec == "" {
		return nil, fmt.Errorf("--targets MUST be a non-empty comma-separated list")
	}
	parts := strings.Split(spec, ",")
	out := make([]vm.VMTarget, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Format: <distro>-<version>-<arch>
		fields := strings.Split(p, "-")
		if len(fields) < 3 {
			return nil, fmt.Errorf("target %q: expected <distro>-<version>-<arch>", p)
		}
		arch := fields[len(fields)-1]
		distro := fields[0]
		version := strings.Join(fields[1:len(fields)-1], "-")
		out = append(out, vm.VMTarget{ID: p, Arch: arch, Distro: distro, Version: version})
	}
	return out, nil
}

func parseUploads(spec string) ([]vm.UploadSpec, error) {
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	out := make([]vm.UploadSpec, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f := strings.SplitN(p, ":", 2)
		if len(f) != 2 {
			return nil, fmt.Errorf("expected host:vm in %q", p)
		}
		out = append(out, vm.UploadSpec{HostPath: f[0], VMPath: f[1]})
	}
	return out, nil
}

func parseCaptures(spec string) ([]vm.CaptureSpec, error) {
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	out := make([]vm.CaptureSpec, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f := strings.SplitN(p, ":", 2)
		if len(f) != 2 {
			return nil, fmt.Errorf("expected vm:host_subpath in %q", p)
		}
		out = append(out, vm.CaptureSpec{VMPath: f[0], HostSubpath: f[1]})
	}
	return out, nil
}

func main() {
	flagManifest := flag.String("image-manifest", "", "Path to vm-images.json")
	flagTargets := flag.String("targets", "", "Comma-separated target IDs")
	flagUploads := flag.String("uploads", "", "Comma-separated host:vm pairs")
	flagScript := flag.String("script", "", "Host path to script run on each target")
	flagCaptures := flag.String("captures", "", "Comma-separated vm:host_subpath pairs")
	flagEvidence := flag.String("evidence-dir", "", "Per-target evidence directory")
	flagConcurrent := flag.Int("concurrent", 1, "Max concurrent VMs (default 1; >1 sets gating=false)")
	flagDev := flag.Bool("dev", false, "Developer mode; permits snapshot reload; sets gating=false")
	flagBootTimeout := flag.Duration("boot-timeout", 0, "Per-target boot timeout (default arch-specific)")
	flagScriptTimeout := flag.Duration("script-timeout", 10*time.Minute, "Per-target script timeout")
	flagColdBoot := flag.Bool("cold-boot", true, "Disable snapshot reload (clause 6.I clause 6 — gating runs MUST cold-boot)")
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
		"Capture a forensic guest-screen snapshot via QMP screendump when a row fails. Default true.")
	flag.Parse()

	for _, fld := range [][2]string{
		{*flagManifest, "--image-manifest"},
		{*flagTargets, "--targets"},
		{*flagScript, "--script"},
		{*flagEvidence, "--evidence-dir"},
	} {
		if fld[0] == "" {
			fmt.Fprintf(os.Stderr, "ERROR: %s is required\n", fld[1])
			os.Exit(2)
		}
	}

	targets, err := parseTargets(*flagTargets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(2)
	}

	uploads, err := parseUploads(*flagUploads)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: --uploads: %v\n", err)
		os.Exit(2)
	}

	captures, err := parseCaptures(*flagCaptures)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: --captures: %v\n", err)
		os.Exit(2)
	}

	// Phase 6: validate network override knobs.
	if *flagNetworkBandwidthDown < 0 || *flagNetworkBandwidthUp < 0 || *flagNetworkLatency < 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --network-bandwidth-down/--network-bandwidth-up/--network-latency MUST be >= 0")
		os.Exit(2)
	}
	if *flagNetworkLoss < 0 || *flagNetworkLoss > 100 {
		fmt.Fprintln(os.Stderr, "ERROR: --network-loss MUST be in [0, 100]")
		os.Exit(2)
	}

	cacheRoot := os.Getenv("XDG_CACHE_HOME")
	if cacheRoot == "" {
		cacheRoot = os.Getenv("HOME") + "/.cache"
	}
	cacheRoot = cacheRoot + "/vasic-digital/containers-images"

	ctx := context.Background()
	store := cache.NewFilesystemStore(cacheRoot)
	v := vm.NewQEMUVM()
	runner := vm.NewQEMUMatrixRunner(v, store)
	result, err := runner.RunMatrix(ctx, vm.VMMatrixConfig{
		Targets:       targets,
		Uploads:       uploads,
		Script:        *flagScript,
		Captures:      captures,
		EvidenceDir:   *flagEvidence,
		BootTimeout:   *flagBootTimeout,
		ScriptTimeout: *flagScriptTimeout,
		Concurrent:    *flagConcurrent,
		Dev:           *flagDev,
		ColdBoot:      *flagColdBoot,
		ImageManifest: *flagManifest,
		NetworkProfile: *flagNetworkProfile,
		NetworkOverride: vm.NetworkConditions{
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
	for i, row := range result.Rows {
		status := "PASS"
		if !row.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%d] %-30s %s exit=%d\n", i+1, row.Target.ID, status, row.ScriptExitCode)
	}
	if result.Gating {
		fmt.Println("Gating: TRUE  (serial run, --dev=false — clause-6.I-clause-7-eligible)")
	} else {
		fmt.Println("Gating: FALSE (--concurrent>1 OR --dev — tag.sh will refuse this attestation)")
	}
	if !result.AllPassed() {
		fmt.Fprintln(os.Stderr, "MATRIX FAILED — at least one target did not pass.")
		os.Exit(1)
	}
	fmt.Println("MATRIX PASSED.")
}
