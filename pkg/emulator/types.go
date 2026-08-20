// Package emulator provides multi-target emulator orchestration for the
// vasic-digital container ecosystem. The package is the constitutional
// landing for clause 6.K (Builds-Inside-Containers Mandate) and clause
// 6.I (Multi-Emulator Container Matrix) — see the parent project
// CLAUDE.md for the full clauses; this package's CLAUDE.md inheritance
// applies them locally.
//
// The first supported target is Android. QEMU + non-Android OS emulators
// are roadmap items recorded in the parent project's plan doc
// (docs/superpowers/plans/2026-05-04-pending-completion-plan.md) under
// the 6.K-debt close criteria.
//
// Anti-bluff posture (clauses 6.J/6.L inherited from Containers' parent):
// every public function in this package MUST have at least one
// falsifiability-rehearsed test. The test files in this directory
// document the rehearsal protocol per function. A green test that
// cannot be made to fail by deliberate-mutation of the production
// path is, by definition, a bluff and MUST be either rewritten or
// removed.
package emulator

import (
	"context"
	"time"
)

// AVD describes a single Android Virtual Device entry as it appears in
// the host's `emulator -list-avds` output. The Name is the canonical
// identifier (e.g. "Pixel_9a"). The optional fields capture metadata
// the matrix runner uses to plan + report.
type AVD struct {
	// Name is the canonical AVD identifier (matches `emulator -list-avds`).
	Name string

	// APILevel is the Android API level the AVD targets (e.g. 28, 30,
	// 34). Zero means "unknown" — callers MAY supply it explicitly when
	// `emulator -list-avds` cannot be parsed for the level.
	APILevel int

	// FormFactor is one of "phone", "tablet", "tv", "wear",
	// "automotive", or "unknown". Used by the matrix planner to
	// satisfy clause 6.I clause 3 (Screen-size coverage).
	FormFactor string
}

// BootResult captures the outcome of a single emulator boot attempt.
// A successful boot requires `getprop sys.boot_completed == 1`.
type BootResult struct {
	AVD           AVD
	Started       bool
	BootCompleted bool
	BootDuration  time.Duration
	ConsolePort   int
	ADBPort       int
	Error         error
	// BootDiag is the forensic snapshot captured when Boot() or WaitForBoot()
	// times out. Non-nil only on timeout/error paths — never set on success.
	// Surfaced so the matrix runner can embed it in the attestation row per
	// clause 6.I Group-B diag intent.
	BootDiag *BootDiagnostic
	// ResolvedAVDName records which AVD name ACTUALLY booted inside the
	// container when it differs from AVD.Name (LVA-014 fix #1: the
	// containerized runner resolves the requested name:api:form entry
	// against the AVDs baked into the image; the §6.AE.2 matrix names
	// are advisory and the baked name — e.g. "default" — wins on an
	// api-level match). Empty when no substitution happened (exact name
	// match or the host-direct runner).
	ResolvedAVDName string
}

// DiagnosticInfo is the per-AVD forensic snapshot captured immediately
// before instrumentation invocation. Reviewer-facing — answers the
// question "is the AVD the matrix claims it ran the AVD that actually
// ran?". A divergence between Diag.SDK and AVD.APILevel is the
// canonical "AVD shadow" bluff that scripts/tag.sh's Gate 3 catches.
//
// Per clause 6.I clause 4: the AVD-row attestation MUST carry enough
// detail to verify the AVD identity post-hoc. Diag is that detail.
type DiagnosticInfo struct {
	// Target is the system-images package id the AVD was created
	// against, e.g. "system-images;android-34;google_apis;x86_64".
	// Empty when avdmanager is unavailable.
	Target string `json:"target,omitempty"`
	// SDK is ro.build.version.sdk reported by the booted emulator
	// (NOT the API level the AVD was created with — those usually
	// match but a misconfigured AVD can have a divergent runtime sdk).
	SDK int `json:"sdk,omitempty"`
	// Device is ro.product.model (preferred) or ro.product.device.
	Device string `json:"device,omitempty"`
	// ADBDevicesState is the line from `adb devices -l` for this
	// emulator's serial — the full record including "transport_id",
	// "product:", "model:", "device:" annotations.
	ADBDevicesState string `json:"adb_devices_state,omitempty"`
}

// FailureSummary is one parsed JUnit <failure> or <error> entry.
// Empty slice on TestResult.Passed=true. A synthetic entry with
// Type="<unparseable>" appears when the JUnit XML is missing or
// malformed — that is evidence corruption, NOT a row failure (the
// gating signal stays on TestResult.Passed per Sixth Law clause 3).
type FailureSummary struct {
	Class   string `json:"class,omitempty"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type"` // "failure" | "error" | "<unparseable>"
	Message string `json:"message,omitempty"`
	Body    string `json:"body,omitempty"`
}

// TestResult captures the outcome of a single instrumentation-test
// execution against a booted emulator. Failed indicates whether the
// gradle task reported a non-zero exit AND/OR the JUnit XML reported
// any test class as failed.
type TestResult struct {
	AVD       AVD
	TestClass string
	Started   time.Time
	Duration  time.Duration
	Passed    bool
	// Output is the captured stdout+stderr of the test runner. May be
	// truncated for the matrix-report file; the full output stays in
	// the per-AVD log directory under EvidenceDir.
	Output string
	Error  error
	// Diag is the per-AVD forensic snapshot captured immediately
	// before instrumentation invocation. Group B clause 6.I extension.
	Diag DiagnosticInfo
	// FailureSummaries is the parsed JUnit XML <failure>/<error>
	// list for this AVD's run. Empty slice on Passed=true. Group B.
	FailureSummaries []FailureSummary
	// Concurrent is the matrix runner's --concurrent setting at the
	// time this test ran. 1 = serial (gating-eligible). Group B.
	Concurrent int
	// NetworkProfile is the active --network-profile name at the time
	// this row ran ("edge", "4g", etc., or "" for no shaping).
	// Phase 6 (Group C remaining); recorded into the attestation row
	// so post-hoc reviewers can attribute network-sensitive failures.
	NetworkProfile string `json:"network_profile,omitempty"`
	// ScreenshotPath is the path (relative to EvidenceDir) of the
	// forensic screenshot captured when Passed=false AND
	// CaptureScreenshotOnFailure=true. Empty when Passed=true OR the
	// capture flag is false OR the screencap command failed (the
	// failure is logged to stderr and does NOT flip the row).
	ScreenshotPath string `json:"screenshot_path,omitempty"`
}

// MatrixConfig describes a single matrix-run invocation: which AVDs to
// boot, what APK/test class to run against each, where to record the
// per-AVD attestation rows that satisfy clause 6.I clause 4.
type MatrixConfig struct {
	// AVDs to run in sequence. Per clause 6.I clause 6, each MUST be
	// cold-booted (no snapshot reload) for the gating run.
	AVDs []AVD

	// AndroidSdkRoot is the host path to the Android SDK. The runner
	// uses `${AndroidSdkRoot}/emulator/emulator` and
	// `${AndroidSdkRoot}/platform-tools/adb`.
	AndroidSdkRoot string

	// APKPath is the host path to the debug APK to install on each AVD.
	APKPath string

	// ExtraAPKPaths are additional host paths to APKs installed on each
	// AVD AFTER APKPath, in order, before instrumentation runs. Empty
	// (nil or zero-length) is the default and preserves all pre-existing
	// single-APK behavior byte-for-byte — no consumer that doesn't set
	// this field observes any change. This exists for Challenges that
	// require a second, sibling app already installed on the SAME device
	// (e.g. a same-device mDNS discovery test that needs a companion
	// on-device API app running alongside the app under test) — a case
	// the single-APKPath field cannot express. A failure installing any
	// entry fails the row exactly like an APKPath install failure does
	// (clause 6.J — no silent partial-install false-green).
	ExtraAPKPaths []string

	// TestClass is the fully-qualified instrumentation test class to
	// run. Empty means "run the default suite" (the project's
	// connectedAndroidTest task discovers tests).
	TestClass string

	// GradleModule is the bare gradle module whose
	// connectedDebugAndroidTest task runs (e.g. "app", "api-app").
	// Empty defaults to "app", preserving all prior behavior. The
	// RunMatrix caller propagates this onto the Emulator before
	// instrumentation so a consumer whose tests live outside the
	// default "app" module is not silently run against :app (which
	// discovers zero of its tests — a false-green by construction).
	// Generic per the Decoupled Reusable Architecture rule: no
	// consumer module name is baked in as a default or special case.
	GradleModule string

	// EvidenceDir is where the matrix runner writes its per-AVD
	// attestation rows + log files. Per clause 6.I clause 4: one row
	// per AVD-test pair.
	EvidenceDir string

	// BootTimeout is the per-AVD cold-boot timeout. Default 5 minutes.
	BootTimeout time.Duration

	// TestTimeout is the per-test execution timeout. Default 10 minutes.
	TestTimeout time.Duration

	// ColdBoot enforces no-snapshot-reload (clause 6.I clause 6) when
	// true. The gating-run convention is true.
	ColdBoot bool

	// Concurrent is the maximum number of emulators booted in parallel
	// by RunMatrix. 1 = serial (gating-eligible; preserves all
	// existing behaviour). >1 = worker pool; sets MatrixResult.Gating
	// to false. Group B.
	Concurrent int

	// Dev marks the run as developer-iteration mode. Permits snapshot
	// reload (caller's choice) and sets MatrixResult.Gating to false.
	// Group B.
	Dev bool

	// ImageManifestPath is the optional path to a vm-images.json
	// manifest. When non-empty, Boot's missing-system-image path
	// falls through to pkg/cache.Store.Get instead of failing.
	// Empty (the pre-Phase-B default) preserves the previous behavior.
	ImageManifestPath string

	// TestReportGlob is the host-glob pattern (relative to CWD) the
	// matrix runner uses to discover JUnit XML test-report files
	// produced by RunInstrumentation. Empty means "skip JUnit parsing"
	// — TestResult.FailureSummaries will be the empty slice.
	//
	// The convention's leakage point: the consuming project's connectedDebugAndroidTest
	// writes to "app/build/outputs/androidTest-results/connected/debug/
	// TEST-*.xml" — that path is consumer-domain, not Containers-domain,
	// per the Decoupled Reusable Architecture rule. The consumer-side
	// thin-glue script (scripts/run-emulator-tests.sh) is the right
	// place for the consuming-project-specific value; the Containers package
	// stays project-agnostic.
	//
	// SAFETY under Concurrent>1: when this glob is non-empty AND
	// Concurrent>1, the matrix runner emits a one-time stderr warning
	// because Gradle's per-build test-report directory is shared
	// across concurrent workers and FailureSummaries can be silently
	// misattributed across rows. Concurrent mode + JUnit-summary
	// capture against a shared output dir is undefined behavior;
	// developer-iteration runs with --concurrent should treat the
	// FailureSummaries field as best-effort, not authoritative.
	TestReportGlob string

	// NetworkProfile names a predefined network conditions profile.
	// Valid values: "edge", "2g", "3g", "4g", "lte", "wifi", "ethernet",
	// "none", or "" (no shaping). See pkg/emulator/network.go for the
	// profile→conditions mapping. Phase 6 (Group C remaining): per-row
	// network simulation lets a matrix run exercise a UI flow under
	// realistic mobile-network conditions; without this, every row
	// runs over the host's wired link, missing slow-network bugs.
	NetworkProfile string

	// NetworkOverride supplies custom network conditions; any non-zero
	// field overrides the profile's value for that field. Allows the
	// operator to use "4g" as a baseline AND override loss to 0.5%.
	NetworkOverride NetworkConditions

	// CaptureScreenshotOnFailure enables forensic screenshot capture
	// when a per-AVD test row fails (Passed=false). Default: true (the
	// operator opts OUT, not in, because forensic gold is high-value).
	// Phase 6 (Group C remaining): a screenshot at the moment of failure
	// is the load-bearing forensic artifact for diagnosing a Compose-UI
	// regression after the fact.
	CaptureScreenshotOnFailure bool
}

// NetworkConditions parameterises a network-shaping profile.
// All fields are optional; a zero value means "no shaping for this
// dimension". Phase 6 (Group C remaining).
type NetworkConditions struct {
	// DownKbps is the downlink bandwidth in kilobits/sec.
	DownKbps int `json:"down_kbps,omitempty"`
	// UpKbps is the uplink bandwidth in kilobits/sec.
	UpKbps int `json:"up_kbps,omitempty"`
	// LatencyMS is the one-way packet latency in milliseconds.
	LatencyMS int `json:"latency_ms,omitempty"`
	// LossPercent is the simulated packet-loss percentage [0, 100].
	// Note: the Android emulator console does NOT support loss; loss
	// is applied for VM (in-guest tc) targets only. The field is
	// recorded in the attestation row regardless.
	LossPercent float64 `json:"loss_percent,omitempty"`
}

// MatrixResult holds the per-AVD outcomes from a single RunMatrix call.
type MatrixResult struct {
	Config     MatrixConfig
	Boots      []BootResult
	Tests      []TestResult
	StartedAt  time.Time
	FinishedAt time.Time
	// AttestationFile is the on-disk path the matrix runner wrote a
	// machine-readable attestation file to (typically
	// EvidenceDir/real-device-verification.json). Empty if the run
	// errored before the file could be written.
	AttestationFile string
	// Gating is true iff this matrix run is eligible to gate a
	// release tag. False when --concurrent != 1 OR --dev was set.
	// Group B clause 6.I extension; scripts/tag.sh refuses to
	// operate on attestations whose run-level Gating is false.
	Gating bool
}

// AllPassed returns true iff every BootResult succeeded AND every
// TestResult passed. Used by the gating signal that decides whether
// scripts/tag.sh may proceed (clause 6.I clause 7).
func (r MatrixResult) AllPassed() bool {
	for _, b := range r.Boots {
		if !b.BootCompleted {
			return false
		}
	}
	for _, t := range r.Tests {
		if !t.Passed {
			return false
		}
	}
	return true
}

// Emulator is the contract a target-specific emulator implementation
// satisfies. Today the only implementation is the Android emulator
// (android.go); QEMU and other-OS emulators implement the same shape
// when they ship.
//
// Methods MUST be safe to call sequentially (Boot → WaitForBoot →
// Install → RunInstrumentation → Teardown). Concurrent calls against
// the same instance are NOT supported.
type Emulator interface {
	// Boot launches the AVD in the background and returns once the
	// emulator process is started (NOT once it has booted Android).
	// Use WaitForBoot to wait for Android boot completion.
	Boot(ctx context.Context, avd AVD, coldBoot bool) (BootResult, error)

	// WaitForBoot polls `getprop sys.boot_completed` until it returns
	// "1" or the timeout elapses. Returns the elapsed duration.
	WaitForBoot(ctx context.Context, port int, timeout time.Duration) (time.Duration, error)

	// Install installs the APK onto the running emulator.
	Install(ctx context.Context, port int, apkPath string) error

	// RunInstrumentation runs the named instrumentation test class via
	// `adb shell am instrument` against the specified port. Returns
	// the captured output and pass/fail signal.
	RunInstrumentation(
		ctx context.Context,
		port int,
		testClass string,
		timeout time.Duration,
	) (output string, passed bool, err error)

	// Teardown stops the emulator and frees its resources.
	Teardown(ctx context.Context, port int) error

	// RunADBCommand runs `adb -s localhost:<port> <args...>` against
	// the booted instance and returns its combined output. This is
	// the generic primitive that runner-agnostic verification flows
	// (e.g. RunCanary in canary.go, which needs `am start` / `dumpsys`
	// / `logcat` and cannot use RunInstrumentation because a
	// release-signed APK cannot be instrumented) build on, so they
	// work uniformly across every Emulator implementation without
	// needing runner-specific knowledge of the adb target format.
	RunADBCommand(ctx context.Context, port int, args ...string) ([]byte, error)
}

// MatrixRunner orchestrates a sequence of (AVD, test) pairs against an
// Emulator implementation. The matrix runner is the constitutional
// landing for clause 6.I; one MatrixRunner satisfies one matrix-run
// gate.
type MatrixRunner interface {
	RunMatrix(ctx context.Context, config MatrixConfig) (MatrixResult, error)
}
