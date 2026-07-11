package emulator

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AndroidMatrixRunner is the production [MatrixRunner] for Android
// AVDs. It uses an [Emulator] (typically [AndroidEmulator]) and walks
// the AVD list sequentially: cold-boot → install → run-tests →
// teardown → next AVD. Per clause 6.I clause 6, sequential execution
// avoids cross-AVD interference (a parallel runner would be a future
// optimisation gated on its own falsifiability rehearsal).
type AndroidMatrixRunner struct {
	// emulator is the single shared Emulator used by the SERIAL path and,
	// for backward compatibility, by the concurrent path when newEmulator
	// is nil (legacy shared-instance construction via
	// NewAndroidMatrixRunner). It is nil when the runner was built with a
	// factory. Sharing one instance across the Concurrent>1 worker pool is
	// safe ONLY if the concrete Emulator is itself concurrency-safe
	// (stateless per Boot); Containerized is NOT.
	emulator Emulator

	// newEmulator, when non-nil, yields a FRESH Emulator instance for EACH
	// runOne invocation (the EMU-2 fix). This is the concurrency-safe
	// construction: every goroutine in the Concurrent>1 worker pool owns
	// its own instance, so no per-Boot mutable state — Containerized's
	// containerName / hostADBPort / adbKeyTmpDir, or either emulator's
	// gradleModule — is shared across goroutines. Without it, AVD-B's
	// Boot() overwrote the shared c.containerName between AVD-A's Boot()
	// and AVD-A's Teardown(), so AVD-A's `rm -f` force-removed AVD-B's
	// container. Constructing a fresh instance per invocation also fixes
	// EMU-3 (gradleModule cross-write) by construction.
	newEmulator func() Emulator
}

// adbAccessor is the seam runOne uses to issue side-effects against the
// running emulator that don't fit the Emulator-interface contract:
// network-shaping (`adb emu network speed/delay`) and screenshot capture
// (`adb exec-out screencap -p`). Production: AndroidEmulator implements
// this implicitly via its CommandExecutor + adbBinary. Tests substitute
// a fakeAdbAccessor that records the same calls.
//
// Anti-bluff posture (clauses 6.J/6.L): without this seam, the only
// way to drive the network/screenshot branches in runOne is to use a
// real *AndroidEmulator — but RunInstrumentation in that type shells
// out to ./gradlew, which is not present in the unit-test environment.
// The seam keeps the test path honest (it asserts on the same observable
// adb calls the production path issues) without fabricating gradle.
type adbAccessor interface {
	executorAndAdb() (CommandExecutor, string)
}

// executorAndAdb implements adbAccessor on the production type. The
// matrix runner type-asserts to adbAccessor (NOT to *AndroidEmulator)
// so a test fake can satisfy the interface without inheriting all the
// Emulator surface methods.
func (a *AndroidEmulator) executorAndAdb() (CommandExecutor, string) {
	return a.executor, a.adbBinary()
}

// NewAndroidMatrixRunner constructs a runner backed by a SINGLE shared
// [Emulator]. Callers typically pass [NewAndroidEmulator] for production
// runs and [NewAndroidEmulatorWithExecutor] for tests.
//
// This is the serial-safe constructor. In a Concurrent>1 run the single
// instance is shared across the worker pool, which is safe ONLY when the
// concrete Emulator is itself concurrency-safe (stateless per Boot). For
// any Emulator with per-Boot mutable state ([Containerized] in
// particular), use [NewAndroidMatrixRunnerWithFactory] instead — sharing
// one Containerized across goroutines is the EMU-2 defect (a per-AVD
// Teardown tears down whichever container the most-recent Boot named).
func NewAndroidMatrixRunner(emulator Emulator) *AndroidMatrixRunner {
	return &AndroidMatrixRunner{emulator: emulator}
}

// NewAndroidMatrixRunnerWithFactory constructs a runner that builds a
// FRESH [Emulator] for EACH runOne invocation via factory — the
// concurrency-safe construction REQUIRED for any Concurrent>1 run whose
// Emulator carries per-Boot mutable state ([Containerized]:
// containerName/hostADBPort/adbKeyTmpDir; both emulators: gradleModule).
// Each goroutine owns its own instance, so a per-AVD Teardown always acts
// on the container its own Boot created (fixes EMU-2), and gradleModule is
// never cross-written (fixes EMU-3). Serial runs likewise get a fresh
// instance per AVD, additionally isolating per-AVD state bleed.
//
// factory MUST return a ready-to-use, independent Emulator on every call
// (no mutable state shared between returned instances) — otherwise the
// isolation guarantee, and with it the EMU-2/EMU-3 fixes, do not hold. A
// nil factory is a programming error: the runner then falls back to the
// (serial-only-safe) shared path with a nil shared instance, which panics
// on first use; callers wanting the safe path MUST pass a non-nil factory.
func NewAndroidMatrixRunnerWithFactory(factory func() Emulator) *AndroidMatrixRunner {
	return &AndroidMatrixRunner{newEmulator: factory}
}

// emulatorForInvocation returns the [Emulator] a single runOne invocation
// operates on. With a factory (NewAndroidMatrixRunnerWithFactory) every
// call yields a FRESH, independent instance — the concurrency-safe path
// (EMU-2/EMU-3). Without a factory it returns the single shared instance,
// safe only in the serial path OR when the Emulator implementation is
// itself concurrency-safe (stateless per Boot).
func (r *AndroidMatrixRunner) emulatorForInvocation() Emulator {
	if r.newEmulator != nil {
		return r.newEmulator()
	}
	return r.emulator
}

func defaultIfZero(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

// filepathGlob returns the glob matches for pattern, or nil when
// pattern is empty. Empty pattern is the constitutional "no JUnit
// parsing" case (per MatrixConfig.TestReportGlob KDoc).
func filepathGlob(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, nil
	}
	return filepath.Glob(pattern)
}

// runOne executes one (boot → install → test → teardown) cycle for a
// single AVD. Returns the BootResult and TestResult ready to append to
// MatrixResult.Boots / .Tests. Captures diag pre-test and parses JUnit
// XML post-test per Group B.
//
// runOne is invoked sequentially in serial mode and concurrently from a
// worker pool when MatrixConfig.Concurrent > 1. It operates ONLY on the
// em parameter it is handed — never on r.emulator directly — so RunMatrix
// decides per invocation whether that is a fresh instance (factory
// construction) or the shared instance (legacy construction).
//
// Concurrency contract: concurrent calls are safe iff each concurrent
// invocation is handed its OWN Emulator instance. RunMatrix guarantees
// that for a factory-constructed runner
// ([NewAndroidMatrixRunnerWithFactory]) by calling emulatorForInvocation
// per invocation. For a shared-instance runner
// ([NewAndroidMatrixRunner]) every concurrent invocation is handed the
// SAME instance, which is safe only when that Emulator is itself
// concurrency-safe (stateless per Boot) — [Containerized] is NOT, because
// Boot mutates c.containerName/c.hostADBPort and Teardown acts on
// c.containerName, so two concurrent AVDs would cross-attribute their
// containers (EMU-2). The prior claim that "each invocation owns its own
// emulator instance" was FALSE for the shared-instance path and is now
// mechanically true only via the factory path.
func (r *AndroidMatrixRunner) runOne(
	ctx context.Context,
	em Emulator,
	avd AVD,
	config MatrixConfig,
	bootTimeout time.Duration,
	testTimeout time.Duration,
) (BootResult, TestResult) {
	// Phase B fix-up (C1): when the matrix caller declared an
	// ImageManifestPath, route any missing system-image through
	// pkg/cache BEFORE we try to boot. The type-assertion is the same
	// pattern captureDiagnostic uses below; it preserves the Emulator
	// interface shape (Boot's signature unchanged) while allowing the
	// matrix runner to invoke the cache-routing helper that lives on
	// the concrete AndroidEmulator type.
	//
	// Empty ImageManifestPath preserves the pre-Phase-B path
	// byte-for-byte (the helper returns nil immediately when
	// manifestPath is empty). This is the production wiring that the
	// schema-stub TestRunMatrix_RoutesMissingSystemImageThroughCache_*
	// test exercises end-to-end via runOne.
	// Pre-boot cleanup: remove zombie qemu-system-* processes from any
	// prior interrupted matrix run before launching the new emulator.
	// Per clause 6.M clause 2, this replaces ad-hoc `pkill qemu-system`
	// invocations that the Forbidden Command List would reject. Best-
	// effort: a Cleanup error is logged to stderr but does not abort
	// the matrix row — the subsequent Boot will discover the new
	// emulator's serial via dynamic discovery regardless of stale
	// processes.
	if _, cleanupErr := Cleanup(ctx, avd.Name); cleanupErr != nil {
		fmt.Fprintf(os.Stderr,
			"[matrix] pre-boot cleanup warning for %s: %v\n",
			avd.Name, cleanupErr,
		)
	}
	// Clear any stale AVD lock file that a prior interrupted run may
	// have left. A lock left by an unclean shutdown causes the emulator
	// to refuse boot with "the AVD is already running". Best-effort:
	// a remove failure is not fatal.
	clearAVDLock(avd.Name)

	if ae, ok := em.(*AndroidEmulator); ok && config.ImageManifestPath != "" {
		if cacheErr := ae.ensureSystemImageViaCache(ctx, avd, config.ImageManifestPath); cacheErr != nil {
			return BootResult{
					AVD:   avd,
					Error: fmt.Errorf("cache-routed system-image: %w", cacheErr),
				}, TestResult{
					AVD:        avd,
					TestClass:  config.TestClass,
					Started:    time.Now(),
					Passed:     false,
					Error:      fmt.Errorf("cache-routed system-image: %w", cacheErr),
					Concurrent: maxInt(config.Concurrent, 1),
				}
		}
	}

	boot, err := em.Boot(ctx, avd, config.ColdBoot)
	if err != nil {
		return boot, TestResult{
			AVD:        avd,
			TestClass:  config.TestClass,
			Started:    time.Now(),
			Passed:     false,
			Error:      fmt.Errorf("boot failed: %w", err),
			Concurrent: maxInt(config.Concurrent, 1),
		}
	}
	waitDuration, err := em.WaitForBoot(ctx, boot.ADBPort, bootTimeout)
	boot.BootDuration += waitDuration
	if err != nil {
		boot.Error = err

		// Capture boot diagnostics so the attestation row carries forensic
		// evidence of WHY boot timed out (clause 6.I Group-B diag intent).
		// Write the diagnostic file into the per-AVD evidence directory so
		// the next operator can read it without re-running.
		avdEvidenceDir := ""
		if config.EvidenceDir != "" {
			avdEvidenceDir = filepath.Join(config.EvidenceDir, avd.Name)
		}
		var adbBin string
		if ae, ok := em.(*AndroidEmulator); ok {
			adbBin = ae.adbBinary()
		}
		if adbBin != "" {
			var execForDiag CommandExecutor
			if ae, ok := em.(*AndroidEmulator); ok {
				execForDiag = ae.executor
			}
			if execForDiag != nil {
				bootDiag := CaptureBootDiagnostic(ctx, adbBin, execForDiag, boot.ADBPort, avdEvidenceDir)
				boot.BootDiag = &bootDiag
			}
		}

		_ = em.Teardown(ctx, boot.ADBPort)
		return boot, TestResult{
			AVD:        avd,
			TestClass:  config.TestClass,
			Started:    time.Now(),
			Passed:     false,
			Error:      fmt.Errorf("wait-for-boot failed: %w", err),
			Concurrent: maxInt(config.Concurrent, 1),
		}
	}
	boot.BootCompleted = true

	if err := em.Install(ctx, boot.ADBPort, config.APKPath); err != nil {
		_ = em.Teardown(ctx, boot.ADBPort)
		return boot, TestResult{
			AVD:        avd,
			TestClass:  config.TestClass,
			Started:    time.Now(),
			Passed:     false,
			Error:      fmt.Errorf("install failed: %w", err),
			Concurrent: maxInt(config.Concurrent, 1),
		}
	}

	// Phase 6 (Group C remaining): apply network conditions if either a
	// profile name OR a non-zero override is configured. Failures are
	// logged to stderr and DO NOT flip the row — network shaping is a
	// best-effort enrichment; the row's Passed signal stays bound to
	// the test outcome itself (Sixth Law clause 3 — gating signal stays
	// on user-visible behaviour, not on instrumentation plumbing).
	if config.NetworkProfile != "" || config.NetworkOverride != (NetworkConditions{}) {
		profile, perr := LookupNetworkProfile(config.NetworkProfile)
		if perr != nil {
			// Unknown profile name is a configuration error — surface
			// it as a row failure rather than a silent skip. The row
			// still carries the AVD identity so the operator can map
			// the typo to its source.
			_ = em.Teardown(ctx, boot.ADBPort)
			return boot, TestResult{
				AVD:        avd,
				TestClass:  config.TestClass,
				Started:    time.Now(),
				Passed:     false,
				Error:      fmt.Errorf("network profile lookup: %w", perr),
				Concurrent: maxInt(config.Concurrent, 1),
			}
		}
		conditions := MergeNetworkConditions(profile, config.NetworkOverride)
		if accessor, ok := em.(adbAccessor); ok {
			executor, adbPath := accessor.executorAndAdb()
			serial := fmt.Sprintf("emulator-%d", boot.ADBPort)
			if applyErr := applyNetworkConditions(ctx, executor, adbPath, serial, conditions); applyErr != nil {
				fmt.Fprintf(os.Stderr,
					"[matrix] warning: applyNetworkConditions failed for %s (profile=%q): %v\n",
					avd.Name, config.NetworkProfile, applyErr,
				)
			}
		}
	}

	// Diagnostic capture happens here, between Install and the test —
	// after Android is up and the APK is installed (so the emulator is
	// stable) and before the test runs (so the diag reflects the
	// state the test will encounter).
	diag := r.captureDiagnostic(ctx, em, boot.ADBPort, avd)

	// Propagate the consumer's gradle module onto the emulator so
	// RunInstrumentation targets `:<module>:connectedDebugAndroidTest`.
	// Empty GradleModule is a no-op (the emulator's construction-time
	// default "app" stands). An Emulator implementation that does not
	// satisfy gradleModuleSetter is run with its constructed module.
	if setter, ok := em.(gradleModuleSetter); ok {
		setter.setGradleModule(config.GradleModule)
	}

	startedTest := time.Now()
	out, passed, runErr := em.RunInstrumentation(
		ctx, boot.ADBPort, config.TestClass, testTimeout,
	)
	test := TestResult{
		AVD:            avd,
		TestClass:      config.TestClass,
		Started:        startedTest,
		Duration:       time.Since(startedTest),
		Passed:         passed,
		Output:         out,
		Error:          runErr,
		Diag:           diag,
		Concurrent:     maxInt(config.Concurrent, 1),
		NetworkProfile: config.NetworkProfile,
	}

	// Phase 6 (Group C remaining): forensic screenshot capture on
	// failure. Default true; operator opts OUT explicitly via
	// CaptureScreenshotOnFailure=false. Capture failures are logged
	// to stderr only — they do NOT flip the row (Sixth Law clause 3:
	// the gating signal stays on the test outcome).
	if !test.Passed && config.CaptureScreenshotOnFailure {
		if accessor, ok := em.(adbAccessor); ok {
			executor, adbPath := accessor.executorAndAdb()
			serial := fmt.Sprintf("emulator-%d", boot.ADBPort)
			screenshotPath := filepath.Join(config.EvidenceDir, avd.Name, "screenshot-on-failure.png")
			if scErr := CaptureScreenshot(ctx, executor, adbPath, serial, screenshotPath); scErr == nil {
				// Stored relative to EvidenceDir so a packaged
				// attestation directory is portable across operator
				// machines (no absolute host paths leak into the JSON).
				test.ScreenshotPath = filepath.Join(avd.Name, "screenshot-on-failure.png")
			} else {
				fmt.Fprintf(os.Stderr,
					"[matrix] warning: screenshot capture failed for %s: %v\n",
					avd.Name, scErr,
				)
			}
		}
	}

	// Persist per-AVD evidence (gradle.log + JUnit XML test-report).
	avdDir := filepath.Join(config.EvidenceDir, avd.Name)
	if mkErr := os.MkdirAll(avdDir, 0o755); mkErr == nil {
		logPath := filepath.Join(avdDir, "gradle.log")
		if werr := os.WriteFile(logPath, []byte(out), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr,
				"[matrix] warning: failed to persist gradle.log for %s: %v\n",
				avd.Name, werr,
			)
		}
		matches, _ := filepathGlob(config.TestReportGlob)
		if len(matches) > 0 {
			reportDir := filepath.Join(avdDir, "test-report")
			_ = os.MkdirAll(reportDir, 0o755)
			for _, src := range matches {
				data, rerr := os.ReadFile(src)
				if rerr != nil {
					continue
				}
				dst := filepath.Join(reportDir, filepath.Base(src))
				_ = os.WriteFile(dst, data, 0o644)
			}
			// Parse JUnit failures from every copied XML; aggregate
			// across files (Gradle emits one XML per class).
			var summaries []FailureSummary
			reportEntries, _ := os.ReadDir(reportDir)
			for _, ent := range reportEntries {
				if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".xml") {
					continue
				}
				summaries = append(summaries, parseJUnitFailures(filepath.Join(reportDir, ent.Name()))...)
			}
			test.FailureSummaries = summaries
		}
	}
	if test.FailureSummaries == nil {
		test.FailureSummaries = []FailureSummary{}
	}
	_ = em.Teardown(ctx, boot.ADBPort)
	return boot, test
}

// captureDiagnostic gathers the per-AVD forensic snapshot used by Group
// B's clause 6.I extension. Best-effort: missing fields default to zero
// values so a partial diag is recorded rather than no diag at all.
func (r *AndroidMatrixRunner) captureDiagnostic(ctx context.Context, em Emulator, port int, avd AVD) DiagnosticInfo {
	d := DiagnosticInfo{}
	if ae, ok := em.(*AndroidEmulator); ok {
		target := fmt.Sprintf("localhost:%d", port)
		if sdkOut, err := ae.executor.Execute(ctx, ae.adbBinary(), "-s", target, "shell", "getprop", "ro.build.version.sdk"); err == nil {
			if sdk, perr := strconv.Atoi(strings.TrimSpace(string(sdkOut))); perr == nil {
				d.SDK = sdk
			}
		}
		if modelOut, err := ae.executor.Execute(ctx, ae.adbBinary(), "-s", target, "shell", "getprop", "ro.product.model"); err == nil && strings.TrimSpace(string(modelOut)) != "" {
			d.Device = strings.TrimSpace(string(modelOut))
		} else if devOut, err := ae.executor.Execute(ctx, ae.adbBinary(), "-s", target, "shell", "getprop", "ro.product.device"); err == nil {
			d.Device = strings.TrimSpace(string(devOut))
		}
		if devicesOut, err := ae.executor.Execute(ctx, ae.adbBinary(), "devices", "-l"); err == nil {
			for _, line := range strings.Split(string(devicesOut), "\n") {
				if strings.HasPrefix(line, target) {
					d.ADBDevicesState = strings.TrimSpace(line)
					break
				}
			}
		}
		// Target (system-images package id) — best-effort via
		// avdmanager. The CLI is `avdmanager list avd -c` for compact
		// output; falling back to empty is acceptable.
		avdmanager := filepath.Join(ae.androidSdkRoot, "cmdline-tools", "latest", "bin", "avdmanager")
		if avdmanagerOut, err := ae.executor.Execute(ctx, avdmanager, "list", "avd"); err == nil {
			d.Target = parseAVDTarget(string(avdmanagerOut), avd.Name)
		}
	}
	return d
}

// parseAVDTarget walks `avdmanager list avd` text output and finds the
// "Based on:" line for the AVD with the requested name. Returns empty
// string when not found.
func parseAVDTarget(out string, avdName string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		nameField := strings.TrimSpace(line)
		if !strings.HasPrefix(nameField, "Name:") {
			continue
		}
		got := strings.TrimSpace(strings.TrimPrefix(nameField, "Name:"))
		if got != avdName {
			continue
		}
		// Look for "Based on: <package id>" within the next 10 lines
		for j := i + 1; j < len(lines) && j < i+10; j++ {
			tl := strings.TrimSpace(lines[j])
			if strings.HasPrefix(tl, "Based on:") {
				return strings.TrimSpace(strings.TrimPrefix(tl, "Based on:"))
			}
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RunMatrix executes the matrix. Serial by default (Concurrent<=1);
// dispatches to a bounded worker pool when Concurrent>1. If any AVD
// fails to boot, its TestResult is recorded as Passed=false with a
// descriptive error; the matrix continues to the next AVD (per clause
// 6.I clause 4 "missing rows are missing evidence" — every AVD MUST
// get a row).
//
// MatrixResult.Gating is true ONLY when Concurrent==1 AND Dev==false —
// any other configuration produces a non-gating run that scripts/tag.sh
// MUST refuse.
//
// On exit RunMatrix writes a machine-readable attestation file at
// EvidenceDir/real-device-verification.json. Operators may also
// generate the human-readable .md form from this file.
func (r *AndroidMatrixRunner) RunMatrix(
	ctx context.Context,
	config MatrixConfig,
) (MatrixResult, error) {
	if len(config.AVDs) == 0 {
		return MatrixResult{}, fmt.Errorf("MatrixConfig.AVDs is empty")
	}
	if config.APKPath == "" {
		return MatrixResult{}, fmt.Errorf("MatrixConfig.APKPath is empty")
	}
	if config.TestClass == "" {
		return MatrixResult{}, fmt.Errorf("MatrixConfig.TestClass is empty")
	}
	if config.EvidenceDir == "" {
		return MatrixResult{}, fmt.Errorf("MatrixConfig.EvidenceDir is empty")
	}
	if err := os.MkdirAll(config.EvidenceDir, 0o755); err != nil {
		return MatrixResult{}, fmt.Errorf("create evidence dir: %w", err)
	}

	bootTimeout := defaultIfZero(config.BootTimeout, 5*time.Minute)
	testTimeout := defaultIfZero(config.TestTimeout, 10*time.Minute)
	concurrent := config.Concurrent
	if concurrent < 1 {
		concurrent = 1
	}

	if config.TestReportGlob != "" && concurrent > 1 {
		fmt.Fprintln(os.Stderr,
			"[matrix] warning: TestReportGlob is set AND Concurrent>1 — JUnit FailureSummaries may be misattributed across rows because the test-report directory is shared across workers. Consider --concurrent=1 for any run whose FailureSummaries are load-bearing.")
	}

	result := MatrixResult{
		Config:    config,
		StartedAt: time.Now(),
		Gating:    concurrent == 1 && !config.Dev,
	}

	if concurrent == 1 {
		// Serial path — preserves existing behaviour. emulatorForInvocation
		// yields a fresh instance per AVD under factory construction, or the
		// shared instance under legacy construction (serial is safe either
		// way).
		for _, avd := range config.AVDs {
			em := r.emulatorForInvocation()
			boot, test := r.runOne(ctx, em, avd, config, bootTimeout, testTimeout)
			result.Boots = append(result.Boots, boot)
			result.Tests = append(result.Tests, test)
		}
	} else {
		// Concurrent path — bounded worker pool. Each worker pulls
		// AVDs off a channel and runs runOne; results land on a
		// buffered channel that is drained after all workers exit.
		type pair struct {
			boot BootResult
			test TestResult
		}
		queue := make(chan AVD)
		results := make(chan pair, len(config.AVDs))
		var wg sync.WaitGroup
		for w := 0; w < concurrent; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for avd := range queue {
					// EMU-2: obtain the emulator instance PER invocation,
					// INSIDE the range loop, so a factory-constructed runner
					// hands each concurrent AVD its OWN fresh Emulator. Hoisting
					// this out of the loop (or before the worker goroutines)
					// would re-introduce the shared-instance defect — the exact
					// cross-container-teardown bug this fix removes.
					em := r.emulatorForInvocation()
					boot, test := r.runOne(ctx, em, avd, config, bootTimeout, testTimeout)
					results <- pair{boot: boot, test: test}
				}
			}()
		}
		for _, avd := range config.AVDs {
			queue <- avd
		}
		close(queue)
		wg.Wait()
		close(results)
		for p := range results {
			result.Boots = append(result.Boots, p.boot)
			result.Tests = append(result.Tests, p.test)
		}
	}

	result.FinishedAt = time.Now()
	attestationFile := filepath.Join(config.EvidenceDir, "real-device-verification.json")
	if err := writeAttestation(attestationFile, result); err == nil {
		result.AttestationFile = attestationFile
	}
	return result, nil
}

func writeAttestation(path string, r MatrixResult) error {
	type rowJSON struct {
		AVD              string           `json:"avd"`
		APILevel         int              `json:"api_level,omitempty"`
		FormFactor       string           `json:"form_factor,omitempty"`
		BootSeconds      float64          `json:"boot_seconds"`
		BootError        string           `json:"boot_error,omitempty"`
		TestClass        string           `json:"test_class"`
		TestPassed       bool             `json:"test_passed"`
		TestSeconds      float64          `json:"test_seconds"`
		TestError        string           `json:"test_error,omitempty"`
		GradleLogPath    string           `json:"gradle_log_path,omitempty"`
		Diag             DiagnosticInfo   `json:"diag"`
		FailureSummaries []FailureSummary `json:"failure_summaries"`
		Concurrent       int              `json:"concurrent"`
		// Phase 6 (Group C remaining) — per-row network conditions and
		// forensic screenshot path so post-hoc reviewers can attribute
		// network-sensitive failures and read the on-screen state at
		// the moment of failure.
		NetworkProfile string `json:"network_profile,omitempty"`
		ScreenshotPath string `json:"screenshot_path,omitempty"`
	}
	rows := make([]rowJSON, 0, len(r.Tests))
	for i, t := range r.Tests {
		var bootSec float64
		var bootErr string
		if i < len(r.Boots) {
			bootSec = r.Boots[i].BootDuration.Seconds()
			if r.Boots[i].Error != nil {
				bootErr = r.Boots[i].Error.Error()
			}
		}
		var testErr string
		if t.Error != nil {
			testErr = t.Error.Error()
		}
		summaries := t.FailureSummaries
		if summaries == nil {
			summaries = []FailureSummary{}
		}
		concurrent := t.Concurrent
		if concurrent < 1 {
			concurrent = 1
		}
		rows = append(rows, rowJSON{
			AVD:              t.AVD.Name,
			APILevel:         t.AVD.APILevel,
			FormFactor:       t.AVD.FormFactor,
			BootSeconds:      bootSec,
			BootError:        bootErr,
			TestClass:        t.TestClass,
			TestPassed:       t.Passed,
			TestSeconds:      t.Duration.Seconds(),
			TestError:        testErr,
			GradleLogPath:    filepath.Join(t.AVD.Name, "gradle.log"),
			Diag:             t.Diag,
			FailureSummaries: summaries,
			Concurrent:       concurrent,
			NetworkProfile:   t.NetworkProfile,
			ScreenshotPath:   t.ScreenshotPath,
		})
	}
	doc := map[string]any{
		"started_at":  r.StartedAt.Format(time.RFC3339),
		"finished_at": r.FinishedAt.Format(time.RFC3339),
		"all_passed":  r.AllPassed(),
		"gating":      r.Gating,
		"rows":        rows,
	}
	bytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, bytes, 0o644)
}

// parseJUnitFailures reads a single JUnit XML report file and returns
// every <failure> and <error> child of every <testcase> as a
// FailureSummary. Tolerates:
//   - Multiple <testsuite> siblings (Gradle's per-class XML output).
//   - <testcase> with both <failure> and <error> children — both are
//     captured as separate entries.
//   - <failure> / <error> with no message attribute (empty Message).
//   - <failure> / <error> with no text content (empty Body).
//
// Malformed XML returns a single synthetic FailureSummary with
// Type="<unparseable>". The synthetic entry is evidence corruption,
// NOT a row failure — the gating signal stays on TestResult.Passed
// per Sixth Law clause 3.
func parseJUnitFailures(xmlPath string) []FailureSummary {
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		return []FailureSummary{{
			Type:    "<unparseable>",
			Message: fmt.Sprintf("junit-xml read failed: %v", err),
		}}
	}
	type junitFailure struct {
		Message string `xml:"message,attr"`
		Type    string `xml:"type,attr"`
		Body    string `xml:",chardata"`
	}
	type junitTestcase struct {
		Class    string         `xml:"classname,attr"`
		Name     string         `xml:"name,attr"`
		Failures []junitFailure `xml:"failure"`
		Errors   []junitFailure `xml:"error"`
	}
	type junitTestsuite struct {
		Testcases []junitTestcase `xml:"testcase"`
	}
	type junitTestsuites struct {
		Suites []junitTestsuite `xml:"testsuite"`
	}
	// Decode into either <testsuites> or a single <testsuite>.
	var suites []junitTestsuite
	var ts junitTestsuites
	if err := xml.Unmarshal(data, &ts); err == nil && len(ts.Suites) > 0 {
		suites = ts.Suites
	} else {
		var single junitTestsuite
		if err := xml.Unmarshal(data, &single); err != nil {
			return []FailureSummary{{
				Type:    "<unparseable>",
				Message: fmt.Sprintf("junit-xml parse failed: %v", err),
			}}
		}
		suites = []junitTestsuite{single}
	}
	var out []FailureSummary
	for _, suite := range suites {
		for _, tc := range suite.Testcases {
			for _, f := range tc.Failures {
				out = append(out, FailureSummary{
					Class:   tc.Class,
					Name:    tc.Name,
					Type:    "failure",
					Message: f.Message,
					Body:    f.Body,
				})
			}
			for _, e := range tc.Errors {
				out = append(out, FailureSummary{
					Class:   tc.Class,
					Name:    tc.Name,
					Type:    "error",
					Message: e.Message,
					Body:    e.Body,
				})
			}
		}
	}
	if out == nil {
		return []FailureSummary{}
	}
	return out
}
