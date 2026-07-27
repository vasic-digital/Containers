package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/cache"
)

// QEMUMatrixRunner is the production VMMatrixRunner.
// Mirror of pkg/emulator.AndroidMatrixRunner — same shape, same
// schema, same Gating semantics. Per §6.I clause 4 + Group B,
// every row carries diag + failure_summaries + concurrent so
// scripts/tag.sh's three Group B gates work unchanged.
type QEMUMatrixRunner struct {
	vm    VM
	store cache.Store
}

// vmNetworkAccessor is the seam runOne uses to apply network shaping
// in-guest. Production: *QEMUVM.ApplyNetworkConditions, which is self-
// authenticating and self-locking under guestMu (see CT-HARDEN-VM-1 +
// CT-HARDEN-VM-2 — the prior raw-sshClient exposer let the tc-qdisc
// command run on an UNAUTHENTICATED session). Tests substitute a fake
// recorder.
//
// The seam exists for the same reason pkg/emulator's adbAccessor exists:
// it lets the matrix-integration tests assert on observable side-effects
// without running a real QEMU + Linux kernel (which would not be
// hermetic on CI).
type vmNetworkAccessor interface {
	ApplyNetworkConditions(ctx context.Context, sshPort int, conditions NetworkConditions) error
}

// vmScreenshotAccessor is the analogous seam for QMP-driven forensic
// screenshot capture. Production: *QEMUVM.CaptureScreenshot, self-locking
// under guestMu so a concurrent target cannot corrupt the shared qmp
// connection mid-capture. The screenshot path goes to a host file
// directly (QMP runs in qemu-system on the host), so the test fake
// records the (monitorPort, dstPath) pair without spawning real QEMU.
type vmScreenshotAccessor interface {
	CaptureScreenshot(ctx context.Context, monitorPort int, dstPath string) error
}

// NewQEMUMatrixRunner constructs a runner. Pass cache.Store for image
// resolution; pass nil to skip image resolution (tests).
func NewQEMUMatrixRunner(vm VM, store cache.Store) *QEMUMatrixRunner {
	return &QEMUMatrixRunner{vm: vm, store: store}
}

func defaultIfZero(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

func defaultBootTimeoutForArch(arch string) time.Duration {
	switch arch {
	case "x86_64":
		return 60 * time.Second
	case "aarch64":
		return 240 * time.Second
	case "riscv64":
		return 360 * time.Second
	default:
		return 120 * time.Second
	}
}

// RunMatrix is the main entry point. It validates config, resolves
// every image up front (cache miss → fetch + verify), then dispatches
// rows either serially (Concurrent==1) or via a worker pool. The
// Gating field is the constitutional eligibility flag — true ⇔
// Concurrent==1 AND !Dev. scripts/tag.sh refuses non-Gating
// attestations as the Group B Gate 2 + 1 enforcement.
func (r *QEMUMatrixRunner) RunMatrix(ctx context.Context, config VMMatrixConfig) (VMMatrixResult, error) {
	if len(config.Targets) == 0 {
		return VMMatrixResult{}, fmt.Errorf("VMMatrixConfig.Targets is empty")
	}
	if config.Script == "" {
		return VMMatrixResult{}, fmt.Errorf("VMMatrixConfig.Script is empty")
	}
	if config.EvidenceDir == "" {
		return VMMatrixResult{}, fmt.Errorf("VMMatrixConfig.EvidenceDir is empty")
	}
	if config.ImageManifest == "" {
		return VMMatrixResult{}, fmt.Errorf("VMMatrixConfig.ImageManifest is empty")
	}
	// I1 fix: reject CaptureSpec.HostSubpath that escapes EvidenceDir.
	// runOne does filepath.Join(EvidenceDir, target.ID, cap.HostSubpath);
	// without this guard, a malicious or fat-fingered "../../etc/shadow"
	// or "/absolute/path" would land outside the evidence sandbox. Per
	// §6.J ("tests must guarantee the product works"), the matrix runner
	// MUST refuse to operate on path-traversal inputs rather than
	// silently writing files where the operator did not intend.
	for i, c := range config.Captures {
		cleaned := filepath.Clean(c.HostSubpath)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
			return VMMatrixResult{}, fmt.Errorf(
				"VMMatrixConfig.Captures[%d].HostSubpath %q escapes EvidenceDir (path traversal forbidden)",
				i, c.HostSubpath,
			)
		}
	}
	// VM2-7 (§11.4.118 — the I1 guard above covers only
	// CaptureSpec.HostSubpath, but runOne feeds target.ID into the SAME
	// filepath.Join(EvidenceDir, target.ID, ...) for every captured file
	// AND the screenshot-on-failure path; an unguarded target.ID like
	// "../../etc" escapes the evidence sandbox exactly as a bad
	// HostSubpath would). Reject any target whose ID resolves outside
	// EvidenceDir, symmetric to the HostSubpath guard.
	for i, target := range config.Targets {
		cleaned := filepath.Clean(target.ID)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
			return VMMatrixResult{}, fmt.Errorf(
				"VMMatrixConfig.Targets[%d].ID %q escapes the evidence sandbox (target-id path traversal forbidden)",
				i, target.ID,
			)
		}
	}
	if err := os.MkdirAll(config.EvidenceDir, 0o755); err != nil {
		return VMMatrixResult{}, fmt.Errorf("create evidence dir: %w", err)
	}
	scriptTimeout := defaultIfZero(config.ScriptTimeout, 10*time.Minute)
	concurrent := config.Concurrent
	if concurrent < 1 {
		concurrent = 1
	}
	result := VMMatrixResult{
		Config:    config,
		StartedAt: time.Now(),
		Gating:    concurrent == 1 && !config.Dev,
	}

	// Resolve every image up front (cache miss → fetch + verify).
	// In tests where r.store is nil, we skip resolution and use a
	// fake path; the stubVM doesn't actually consume the path.
	qcowPaths := make(map[string]string, len(config.Targets))
	if r.store != nil {
		manifest, err := cache.LoadManifest(config.ImageManifest)
		if err != nil {
			return result, fmt.Errorf("load manifest: %w", err)
		}
		for _, target := range config.Targets {
			path, err := r.store.Get(ctx, manifest, target.ID)
			if err != nil {
				return result, fmt.Errorf("resolve image %q: %w", target.ID, err)
			}
			qcowPaths[target.ID] = path
		}
	} else {
		for _, target := range config.Targets {
			qcowPaths[target.ID] = "/tmp/" + target.ID + ".qcow2"
		}
	}

	if concurrent == 1 {
		for _, target := range config.Targets {
			row := r.runOne(ctx, target, qcowPaths[target.ID], config, scriptTimeout)
			result.Rows = append(result.Rows, row)
		}
	} else {
		queue := make(chan VMTarget)
		results := make(chan VMTestResult, len(config.Targets))
		var wg sync.WaitGroup
		for w := 0; w < concurrent; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for target := range queue {
					results <- r.runOne(ctx, target, qcowPaths[target.ID], config, scriptTimeout)
				}
			}()
		}
		for _, target := range config.Targets {
			queue <- target
		}
		close(queue)
		wg.Wait()
		close(results)
		for row := range results {
			result.Rows = append(result.Rows, row)
		}
	}

	result.FinishedAt = time.Now()
	attestationFile := filepath.Join(config.EvidenceDir, "real-device-verification.json")
	if err := writeVMAttestation(attestationFile, result); err == nil {
		result.AttestationFile = attestationFile
	}
	return result, nil
}

// runOne executes one (target, qcowPath) pair: Boot, WaitForReady,
// captureDiagnostic, Upload[s], Run, Download[s], Teardown — and
// builds the VMTestResult row.
func (r *QEMUMatrixRunner) runOne(ctx context.Context, target VMTarget, qcowPath string, config VMMatrixConfig, scriptTimeout time.Duration) VMTestResult {
	bootTimeout := defaultIfZero(config.BootTimeout, defaultBootTimeoutForArch(target.Arch))
	concurrent := config.Concurrent
	if concurrent < 1 {
		concurrent = 1
	}
	row := VMTestResult{
		Target:           target,
		Started:          time.Now(),
		FailureSummaries: []FailureSummary{},
		Concurrent:       concurrent,
	}
	boot, err := r.vm.Boot(ctx, VMConfig{
		Target:        target,
		QCowPath:      qcowPath,
		Uploads:       config.Uploads,
		Script:        config.Script,
		Captures:      config.Captures,
		BootTimeout:   bootTimeout,
		ScriptTimeout: scriptTimeout,
		ColdBoot:      config.ColdBoot,
	})
	row.BootSeconds = boot.BootDuration.Seconds()
	if err != nil {
		row.BootError = err.Error()
		row.Passed = false
		row.Duration = time.Since(row.Started)
		return row
	}
	if err := r.vm.WaitForReady(ctx, boot.SSHPort, bootTimeout); err != nil {
		row.BootError = err.Error()
		row.Passed = false
		row.Duration = time.Since(row.Started)
		stampTeardownError(&row, r.vm.Teardown(ctx, boot.MonitorPort, boot.SSHPort))
		return row
	}

	// Phase 6 (Group C remaining): apply network conditions in-guest
	// via tc qdisc. Failures are logged via the row's FailureSummaries
	// but DO NOT flip Passed (Sixth Law clause 3 — gating signal stays
	// on the script outcome).
	if config.NetworkProfile != "" || config.NetworkOverride != (NetworkConditions{}) {
		profile, perr := LookupNetworkProfile(config.NetworkProfile)
		if perr != nil {
			row.FailureSummaries = append(row.FailureSummaries, FailureSummary{
				Type:    "network-profile-error",
				Message: perr.Error(),
			})
			row.Passed = false
			row.Duration = time.Since(row.Started)
			stampTeardownError(&row, r.vm.Teardown(ctx, boot.MonitorPort, boot.SSHPort))
			return row
		}
		conditions := MergeNetworkConditions(profile, config.NetworkOverride)
		if accessor, ok := r.vm.(vmNetworkAccessor); ok {
			// ApplyNetworkConditions authenticates boot.SSHPort before the
			// tc-qdisc command (CT-HARDEN-VM-2) and holds guestMu across
			// the whole sequence (CT-HARDEN-VM-1).
			if applyErr := accessor.ApplyNetworkConditions(ctx, boot.SSHPort, conditions); applyErr != nil {
				// Best-effort enrichment; log as a non-fatal failure
				// summary, do NOT flip Passed.
				row.FailureSummaries = append(row.FailureSummaries, FailureSummary{
					Type:    "network-shaping-warning",
					Message: applyErr.Error(),
				})
			}
		}
		row.NetworkProfile = config.NetworkProfile
	}
	row.Diag = r.captureDiagnostic(ctx, boot.SSHPort, target)
	for _, up := range config.Uploads {
		if err := r.vm.Upload(ctx, boot.SSHPort, up.HostPath, up.VMPath); err != nil {
			row.FailureSummaries = append(row.FailureSummaries, FailureSummary{
				Type: "upload-failed", Message: err.Error(),
			})
			row.Passed = false
			row.Duration = time.Since(row.Started)
			stampTeardownError(&row, r.vm.Teardown(ctx, boot.MonitorPort, boot.SSHPort))
			return row
		}
	}
	stdout, stderr, exit, runErr := r.vm.Run(ctx, boot.SSHPort, config.Script, nil, scriptTimeout)
	row.ScriptExitCode = exit
	row.ScriptStderr = stderr
	row.Passed = (runErr == nil) && (exit == 0)
	if !row.Passed {
		summary := FailureSummary{
			Type:    "exit-non-zero",
			Message: fmt.Sprintf("script exit=%d", exit),
			Body:    truncate(stdout+stderr, 2048),
		}
		if runErr != nil {
			summary.Type = "run-error"
			summary.Message = runErr.Error()
		}
		row.FailureSummaries = append(row.FailureSummaries, summary)
	}
	for _, cap := range config.Captures {
		dst := filepath.Join(config.EvidenceDir, target.ID, cap.HostSubpath)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := r.vm.Download(ctx, boot.SSHPort, cap.VMPath, dst); err == nil {
			row.CapturedFiles = append(row.CapturedFiles, dst)
		}
	}

	// Phase 6 (Group C remaining): forensic screenshot capture on
	// failure. Default OFF unless the operator opts in via
	// CaptureScreenshotOnFailure=true. Failures here log to
	// FailureSummaries but DO NOT flip Passed (it is already false to
	// reach this branch). The QMP screendump writes to a HOST path
	// directly — qemu-system runs on the host, so no SCP step.
	if !row.Passed && config.CaptureScreenshotOnFailure {
		if accessor, ok := r.vm.(vmScreenshotAccessor); ok {
			screenshotPath := filepath.Join(config.EvidenceDir, target.ID, "screenshot-on-failure.ppm")
			// CaptureScreenshot self-locks under guestMu (CT-HARDEN-VM-1).
			if scErr := accessor.CaptureScreenshot(ctx, boot.MonitorPort, screenshotPath); scErr == nil {
				row.ScreenshotPath = filepath.Join(target.ID, "screenshot-on-failure.ppm")
			} else {
				row.FailureSummaries = append(row.FailureSummaries, FailureSummary{
					Type:    "screenshot-capture-warning",
					Message: scErr.Error(),
				})
			}
		}
	}

	stampTeardownError(&row, r.vm.Teardown(ctx, boot.MonitorPort, boot.SSHPort))
	row.Duration = time.Since(row.Started)
	return row
}

// stampTeardownError records a Teardown failure into the row's
// FailureSummaries (per §6.I clause 4) and flips Passed to false.
// Silent-swallowing Teardown errors (the prior `_ = r.vm.Teardown(...)`
// pattern) was a §6.J bluff vector: the matrix runner would record a
// passing row even when the VM was still running and consuming a
// monitor port the next iteration would collide with. After this fix,
// every Teardown failure lands in the attestation file as a
// "teardown-failed" FailureSummary the operator can act on.
func stampTeardownError(row *VMTestResult, err error) {
	if err == nil {
		return
	}
	row.FailureSummaries = append(row.FailureSummaries, FailureSummary{
		Type:    "teardown-failed",
		Message: err.Error(),
	})
	row.Passed = false
}

// captureDiagnostic gathers the per-VM forensic snapshot for the
// attestation row. Reviewer-facing per §6.I clause 4: answers
// "is the VM the matrix claims it ran the VM that actually ran?".
func (r *QEMUMatrixRunner) captureDiagnostic(ctx context.Context, sshPort int, target VMTarget) DiagnosticInfo {
	d := DiagnosticInfo{Target: target.ID}
	if out, _, _, err := r.vm.Run(ctx, sshPort, "uname -m", nil, 5*time.Second); err == nil {
		d.Arch = trimSpace(out)
	}
	if out, _, _, err := r.vm.Run(ctx, sshPort, "uname -r", nil, 5*time.Second); err == nil {
		d.Kernel = trimSpace(out)
	}
	if out, _, _, err := r.vm.Run(ctx, sshPort, "cat /etc/os-release | grep '^ID=' | head -n1 | cut -d= -f2", nil, 5*time.Second); err == nil {
		d.Distro = trimSpace(out)
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...<truncated>"
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// writeVMAttestation serializes the matrix result to a JSON file
// matching the pkg/emulator attestation schema (gating, diag,
// failure_summaries, concurrent). scripts/tag.sh's three Group B
// gates consume this schema unchanged for VM matrices.
func writeVMAttestation(path string, r VMMatrixResult) error {
	type rowJSON struct {
		Target           VMTarget         `json:"target"`
		Passed           bool             `json:"test_passed"`
		ScriptExitCode   int              `json:"script_exit_code"`
		BootSeconds      float64          `json:"boot_seconds"`
		BootError        string           `json:"boot_error,omitempty"`
		Duration         float64          `json:"duration_seconds"`
		Diag             DiagnosticInfo   `json:"diag"`
		FailureSummaries []FailureSummary `json:"failure_summaries"`
		Concurrent       int              `json:"concurrent"`
		// api_level emitted to keep tag.sh's clause-6.I-clause-7
		// helper happy; for VM rows the "api_level" is the diag SDK-
		// equivalent (we use 0 + diag-only matching since VMs aren't
		// Android — operators inspect diag.target instead).
		APILevel int `json:"api_level,omitempty"`
		// Phase 6 (Group C remaining) — per-row network conditions
		// and forensic screenshot path.
		NetworkProfile string `json:"network_profile,omitempty"`
		ScreenshotPath string `json:"screenshot_path,omitempty"`
	}
	rows := make([]rowJSON, 0, len(r.Rows))
	for _, row := range r.Rows {
		rows = append(rows, rowJSON{
			Target:           row.Target,
			Passed:           row.Passed,
			ScriptExitCode:   row.ScriptExitCode,
			BootSeconds:      row.BootSeconds,
			BootError:        row.BootError,
			Duration:         row.Duration.Seconds(),
			Diag:             row.Diag,
			FailureSummaries: row.FailureSummaries,
			Concurrent:       row.Concurrent,
			NetworkProfile:   row.NetworkProfile,
			ScreenshotPath:   row.ScreenshotPath,
		})
	}
	doc := map[string]any{
		"started_at":  r.StartedAt.Format(time.RFC3339),
		"finished_at": r.FinishedAt.Format(time.RFC3339),
		"all_passed":  r.AllPassed(),
		"gating":      r.Gating,
		"rows":        rows,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
