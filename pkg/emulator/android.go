package emulator

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"digital.vasic.containers/pkg/cache"
)

// NOTE: tests that override any of the package-level seams below MUST
// NOT call t.Parallel() — the swap-and-restore pattern
// (`prev := X; X = ...; defer func() { X = prev }()`) is not safe
// against concurrent test functions racing on the package-level var.
// All current callers respect this; future test authors must too.
//
// Current package-level mutable seams (every entry MUST be listed here
// so the discoverable surface stays exhaustive — adding a new seam
// without extending this list is itself a clause-6.J bluff vector):
//   - killByPortHook         (Teardown's KillByPort fast-path injection)
//   - teardownGracePeriod    (Teardown's wall-clock wait for emulator exit)
//   - loadManifestHook       (ensureSystemImageViaCache's cache.Manifest loader)
//   - cacheStoreFactory      (ensureSystemImageViaCache's cache.Store factory)

// killByPortHook is the package-level seam tests use to substitute a
// fake KillByPort implementation. Production Teardown uses the real
// KillByPort; tests override this so they don't have to spawn real
// QEMU processes to test the fast-path branch.
var killByPortHook = KillByPort

// adbHygieneHook is the package-level seam that lets tests bypass the
// pre-boot ADB hygiene reset (ResetADBHygiene) without needing to add
// kill-server/start-server entries to every test's fakeExecutor.scripts.
//
// Production Boot() calls the real ResetADBHygiene. Tests that are not
// specifically testing ADB hygiene set this to a no-op:
//
//	prev := adbHygieneHook
//	adbHygieneHook = func(ctx context.Context, adbBinary string, exec CommandExecutor) ADBHygieneResult {
//	    return ADBHygieneResult{} // no-op
//	}
//	defer func() { adbHygieneHook = prev }()
//
// Anti-bluff: the seam exists only for testing. Production Boot() always
// calls the real implementation. Tests that ARE testing ADB hygiene bypass
// this seam entirely and call ResetADBHygiene directly.
var adbHygieneHook = ResetADBHygiene

// loadManifestHook is the package-level seam tests use to substitute a
// fake manifest loader. Production code uses cache.LoadManifest; tests
// override to inject a manifest without writing a JSON file.
//
// Anti-bluff posture (clauses 6.J/6.L): the seam exists ONLY for
// testing. Production code uses the real cache.LoadManifest. A test
// that uses the fake to assert "the routing decision was reached"
// is asserting on observable behaviour — did the missing-image path
// consult the cache? — not on internal state.
var loadManifestHook = cache.LoadManifest

// cacheStoreFactory is the package-level seam tests use to substitute
// a fake Store. Production code constructs a real FilesystemStore
// rooted at defaultCacheRoot(). Tests override to record Get() calls.
var cacheStoreFactory = func(root string) cache.Store {
	return cache.NewFilesystemStore(root)
}

// defaultCacheRoot returns the production cache root directory:
//
//	$XDG_CACHE_HOME/vasic-digital/containers-images/
//
// Mirrors cmd/vm-matrix's resolution so a single XDG_CACHE_HOME
// honours both the VM and the emulator paths. See pkg/cache/store.go
// KDoc for the on-disk layout.
func defaultCacheRoot() string {
	root := os.Getenv("XDG_CACHE_HOME")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(root, "vasic-digital", "containers-images")
}

// teardownGracePeriod is the wall-clock time Teardown waits after
// `adb emu kill` before invoking the KillByPort fast-path. Set short
// in tests so the suite stays fast; defaults to 30 seconds in
// production (matches the 2026-05-05 grace already in the file).
var teardownGracePeriod = 30 * time.Second

// CommandExecutor is the seam through which the AndroidEmulator runs
// host commands. The production impl shells out via os/exec; tests
// substitute a fake that records invocations and returns canned output.
//
// Anti-bluff posture (clause 6.J): the seam exists ONLY for testing.
// Production code uses the real os/exec impl. A test that uses the
// fake to assert "real adb was invoked with these args" is not a bluff
// because it asserts on observable host-shell behaviour, not on internal
// state.
//
// `Execute` is for short-lived synchronous commands (adb, getprop).
// `Start` is for long-running detached processes (the emulator itself
// is a long-lived QEMU-backed process; the matrix runner needs Boot
// to return without blocking on it).
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
	Start(ctx context.Context, name string, args ...string) error
}

type osExecutor struct{}

func (osExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (osExecutor) Start(_ context.Context, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// Detach: redirect stdio to /dev/null; setsid (POSIX) so the
	// emulator survives the test runner's process group.
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release the child's resources so the runner doesn't accumulate
	// zombies. We don't Wait() — the emulator process lives until
	// Teardown sends `adb emu kill`.
	go func() { _ = cmd.Wait() }()
	return nil
}

// NewOSExecutor returns the real os/exec-based executor used by
// production code.
func NewOSExecutor() CommandExecutor { return osExecutor{} }

// AndroidEmulator implements [Emulator] by shelling out to the Android
// SDK's emulator + adb binaries. The runner does NOT itself manage a
// container — clause 6.I says the matrix runs INSIDE a container, and
// the caller-supplied AndroidSdkRoot is the path to the SDK that's
// already mounted into the container (or available on the host for
// development iteration).
//
// Methods follow the Emulator interface; see types.go for the contract.
type AndroidEmulator struct {
	executor       CommandExecutor
	androidSdkRoot string

	// gradleModule is the bare gradle module whose
	// connectedDebugAndroidTest task RunInstrumentation runs. Empty
	// defaults to "app". Generic per the Decoupled Reusable
	// Architecture rule — no consumer module is special-cased.
	gradleModule string
}

// NewAndroidEmulator constructs an AndroidEmulator that uses the real
// host shell to invoke the SDK binaries. The gradle module defaults to
// "app"; use NewAndroidEmulatorWithModule to target a different module.
func NewAndroidEmulator(androidSdkRoot string) *AndroidEmulator {
	return &AndroidEmulator{
		executor:       osExecutor{},
		androidSdkRoot: androidSdkRoot,
	}
}

// NewAndroidEmulatorWithModule constructs an AndroidEmulator targeting a
// specific gradle module's connectedDebugAndroidTest task. Empty module
// defaults to "app" (identical to NewAndroidEmulator). Production code
// that needs a non-default module (the CLI's --gradle-module flag) uses
// this constructor.
func NewAndroidEmulatorWithModule(androidSdkRoot, gradleModule string) *AndroidEmulator {
	return &AndroidEmulator{
		executor:       osExecutor{},
		androidSdkRoot: androidSdkRoot,
		gradleModule:   gradleModule,
	}
}

// NewAndroidEmulatorWithExecutor is the test-injection constructor.
// Production code uses NewAndroidEmulator.
func NewAndroidEmulatorWithExecutor(
	androidSdkRoot string,
	executor CommandExecutor,
) *AndroidEmulator {
	return &AndroidEmulator{executor: executor, androidSdkRoot: androidSdkRoot}
}

// ErrExtractionNotImplemented was the v0.1 sentinel signalling that
// fetch+verify succeeded but extraction was not yet implemented. The
// v0.2 flow (this package, post Phase 3) implements the extraction
// fully; the sentinel is retained as an exported symbol for backward
// compatibility with any v0.1 caller that still does
// `errors.Is(err, ErrExtractionNotImplemented)` to identify the v0.1
// gap. New code MUST NOT consult this sentinel — the production path
// no longer returns it. New callers wanting to detect a structural
// extraction failure should use ErrSystemImageMalformed.
//
// Anti-bluff posture: keeping the export prevents downstream compile
// breakage; production code never returns this error so green tests
// against ErrExtractionNotImplemented in v0.1 callers naturally stop
// firing — which is the desired outcome (the v0.1 gap is closed).
var ErrExtractionNotImplemented = errors.New("cache-routed system-image extraction: not implemented in v0.1; operator end-to-end run only")

// ErrSystemImageMalformed signals that the cache-fetched ZIP did not
// contain the minimum set of files an Android system-image MUST carry
// (canonically: system.img + build.prop). Callers — the matrix runner
// in particular — use errors.Is(err, ErrSystemImageMalformed) to
// distinguish a transport-level failure (network, SHA mismatch) from
// a content-level failure (the ZIP fetched fine but is not a valid
// Android system-image).
//
// Anti-bluff posture (clauses 6.J/6.L): this is the load-bearing
// post-extraction validation. Without it, an empty or trimmed ZIP
// would silently extract zero useful files into ANDROID_SDK_ROOT and
// the next emulator boot would fail with a confusing "system.img not
// found" error far from the root cause. Returning a typed error at
// the extraction boundary keeps the failure attributable.
var ErrSystemImageMalformed = errors.New("system-image ZIP is malformed: missing required files")

// ensureSystemImageViaCache routes the missing-system-image fallback
// path through pkg/cache when manifestPath is non-empty, and (Phase 3)
// extracts the fetched ZIP into ANDROID_SDK_ROOT/system-images/...
//
// Behaviour matrix (anti-bluff: every branch is observable AND the
// fine-grained idempotence check is reachable in production — there
// is no coarser short-circuit that would mask a Branch 4 bug):
//
//  1. manifestPath == "" → no-op, returns nil. Pre-Phase-B behaviour
//     preserved byte-for-byte. The matrix runner's existing
//     fail-fast on missing system-image runs unchanged.
//  2. manifestPath != "" → load the manifest, derive the per-AVD
//     target dir (system-images/android-<api>/<tag>/<abi>/), compute
//     imageID = "android-<api>-<formFactor>", parse <tag> + <abi>
//     from the manifest entry's URL.
//  3. Idempotence: if the per-AVD target dir already contains the
//     canonical pair (system.img + build.prop), the helper is a no-op
//     and returns nil without consulting the cache (avoids redundant
//     SHA-256 verify work on subsequent matrix iterations). This is
//     the SOLE on-disk short-circuit — there is intentionally no
//     coarser API-level dir check, because such a check would mask
//     bugs in this fine-grained idempotence path.
//  4. Otherwise: call cache.Store.Get(...) to fetch + verify the
//     bytes, then unzip into the target dir stripping any single-
//     leading-component prefix (the upstream ZIP stores files under
//     <abi>/...). Post-extraction, validate that system.img +
//     build.prop exist; otherwise return an error wrapping
//     ErrSystemImageMalformed and leave NO partial files under the
//     target dir (cleanup-on-failure for retry idempotence).
//
// Anti-bluff posture (Phase 3, clauses 6.J/6.L): the validation in
// step 4 is the load-bearing safety property. A silently-empty ZIP
// would otherwise extract zero files and the next emulator boot
// would fail with a confusing "system.img not found" error far from
// the root cause. The ErrSystemImageMalformed sentinel keeps the
// failure attributable; the on-disk cleanup keeps the failure mode
// idempotent (the next retry sees a clean target dir, not a
// half-extracted one).
func (a *AndroidEmulator) ensureSystemImageViaCache(
	ctx context.Context,
	avd AVD,
	manifestPath string,
) error {
	// Branch 1: empty manifest path → pre-Phase-B no-op.
	if manifestPath == "" {
		return nil
	}

	// Branch 2 + 3 + 4: image MAY be missing → consult manifest first,
	// then fine-grained idempotence, then cache + extract.
	manifest, err := loadManifestHook(manifestPath)
	if err != nil {
		return fmt.Errorf("ensureSystemImageViaCache: load manifest %s: %w", manifestPath, err)
	}
	imageID := fmt.Sprintf("android-%d-%s", avd.APILevel, avd.FormFactor)
	entry, err := manifest.FindByID(imageID)
	if err != nil {
		return fmt.Errorf("ensureSystemImageViaCache: %w", err)
	}
	tag, abi, err := parseSystemImageURL(entry.URL)
	if err != nil {
		return fmt.Errorf("ensureSystemImageViaCache: %w", err)
	}
	targetDir := filepath.Join(
		a.androidSdkRoot,
		"system-images",
		fmt.Sprintf("android-%d", avd.APILevel),
		tag,
		abi,
	)

	// Branch 4: fine-grained idempotence — target dir already has the
	// canonical pair. Skip the cache fetch entirely; no SHA work needed.
	if hasExtractedSystemImage(targetDir) {
		return nil
	}

	store := cacheStoreFactory(defaultCacheRoot())
	zipPath, err := store.Get(ctx, manifest, imageID)
	if err != nil {
		return fmt.Errorf("ensureSystemImageViaCache: fetch %s: %w", imageID, err)
	}

	if err := extractSystemImageZip(zipPath, targetDir); err != nil {
		return fmt.Errorf(
			"ensureSystemImageViaCache: extract %s into %s: %w",
			imageID, targetDir, err,
		)
	}
	return nil
}

// parseSystemImageURL extracts <tag> + <abi> from an Android system-image
// repository URL. Canonical pattern:
//
//	https://dl.google.com/android/repository/sys-img/<tag>/<abi>-<api>_r<rev>.zip
//
// Examples:
//
//	.../sys-img/google_apis/x86_64-28_r12.zip          → tag=google_apis, abi=x86_64
//	.../sys-img/google_apis_playstore/arm64-v8a-34_r3.zip → tag=google_apis_playstore, abi=arm64-v8a
//	.../sys-img/android-tv/x86-30_r4.zip               → tag=android-tv, abi=x86
//
// The function is strict: it requires the URL path to contain a
// "/sys-img/<tag>/<filename>" segment, and the filename to be
// "<abi>-<api>_r<rev>.zip". Anything else returns an error so the
// caller surfaces a typed failure rather than mis-extracting into the
// wrong directory.
func parseSystemImageURL(rawURL string) (tag, abi string, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", "", fmt.Errorf("parse url %q: %w", rawURL, perr)
	}
	// Use path.Clean to strip ".."/"."/double-slashes; the URL path
	// is "/" separated regardless of host OS.
	cleaned := path.Clean(u.Path)
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	// Locate the "sys-img" segment; tag is the next part, filename
	// is the part after that. Expecting at least: <prefix>/sys-img/<tag>/<file>.
	idx := -1
	for i, p := range parts {
		if p == "sys-img" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+2 >= len(parts) {
		return "", "", fmt.Errorf(
			"system-image URL %q does not match .../sys-img/<tag>/<abi>-<api>_r<rev>.zip",
			rawURL,
		)
	}
	tag = parts[idx+1]
	filename := parts[idx+2]
	if !strings.HasSuffix(filename, ".zip") {
		return "", "", fmt.Errorf(
			"system-image URL %q filename %q does not end in .zip",
			rawURL, filename,
		)
	}
	stem := strings.TrimSuffix(filename, ".zip")
	// stem looks like "x86_64-28_r12" or "arm64-v8a-34_r3". The api
	// + revision suffix is "<api>_r<rev>"; the abi is everything to
	// the LEFT of the "<digits>_r<digits>" suffix. We split on
	// "-<digits>_r" pattern.
	dashIdx := -1
	for i := len(stem) - 1; i > 0; i-- {
		if stem[i] != '-' {
			continue
		}
		// Candidate split point. Validate the suffix is "<api>_r<rev>".
		suffix := stem[i+1:]
		ur := strings.Index(suffix, "_r")
		if ur <= 0 || ur == len(suffix)-2 {
			continue
		}
		left := suffix[:ur]
		right := suffix[ur+2:]
		if !isAllDigits(left) || !isAllDigits(right) {
			continue
		}
		dashIdx = i
		break
	}
	if dashIdx < 0 {
		return "", "", fmt.Errorf(
			"system-image URL %q filename %q does not match <abi>-<api>_r<rev>.zip",
			rawURL, filename,
		)
	}
	abi = stem[:dashIdx]
	if tag == "" || abi == "" {
		return "", "", fmt.Errorf("system-image URL %q yielded empty tag/abi", rawURL)
	}
	return tag, abi, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// hasExtractedSystemImage returns true iff targetDir already contains
// the canonical pair of files an Android system-image MUST carry
// (system.img + build.prop). Used by ensureSystemImageViaCache for the
// fine-grained idempotence check (Branch 4).
func hasExtractedSystemImage(targetDir string) bool {
	if _, err := os.Stat(filepath.Join(targetDir, "system.img")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(targetDir, "build.prop")); err != nil {
		return false
	}
	return true
}

// extractSystemImageZip unzips the file at zipPath into targetDir,
// stripping a single leading directory component if every entry shares
// the same first segment (the upstream Android ZIP stores files under
// "<abi>/..." so we want to flatten that into targetDir).
//
// Post-extraction validation: targetDir MUST contain system.img AND
// build.prop. If the validation fails, ALL extracted files are removed
// (the target dir is left in the same not-yet-installed state it was
// before the call) and an error wrapping ErrSystemImageMalformed is
// returned.
//
// Anti-bluff posture: the validation is the difference between a
// silent-success-on-empty-zip (clause 6.J bluff vector) and an honest
// error that names the missing files. The cleanup-on-failure is the
// difference between an idempotent retry (next call sees a clean dir)
// and a poisoned-state retry (next call sees a half-extracted dir
// whose missing files would now be misattributed to the cache layer).
func extractSystemImageZip(zipPath, targetDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer r.Close()

	// Detect a single shared leading directory prefix. If every entry
	// is under "<prefix>/" with the same <prefix>, strip it; otherwise
	// extract entries verbatim.
	prefix := commonZipPrefix(r.File)

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir target %s: %w", targetDir, err)
	}

	// Track the files we wrote so we can clean up on validation failure.
	written := make([]string, 0, len(r.File))
	cleanup := func() {
		// Remove files first, then prune empty directories. Best
		// effort — we don't fail extraction over cleanup errors.
		for _, p := range written {
			_ = os.Remove(p)
		}
		// Walk the tree pruning empty dirs (post-order). targetDir is
		// removed only if it ends up empty.
		_ = filepath.Walk(targetDir, func(_ string, _ os.FileInfo, _ error) error { return nil })
		// Two passes are sufficient for typical depths.
		for i := 0; i < 4; i++ {
			_ = pruneEmptyDirs(targetDir)
		}
	}

	for _, f := range r.File {
		name := f.Name
		// Strip the common prefix (with trailing slash) if any.
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue
			}
		}
		// Defence-in-depth against zip-slip: refuse any entry whose
		// cleaned path escapes targetDir.
		dest := filepath.Join(targetDir, filepath.FromSlash(name))
		relCheck, err := filepath.Rel(targetDir, dest)
		if err != nil || strings.HasPrefix(relCheck, "..") {
			cleanup()
			return fmt.Errorf("zip entry %q escapes target dir", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, f.Mode().Perm()|0o700); err != nil {
				cleanup()
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
			continue
		}

		// Regular file: ensure parent + open + copy.
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			cleanup()
			return fmt.Errorf("mkdir parent of %s: %w", dest, err)
		}
		if err := copyZipEntry(f, dest); err != nil {
			cleanup()
			return err
		}
		written = append(written, dest)
	}

	// Post-extraction validation. The system-image is unusable to the
	// emulator without these two files; missing either is an honest
	// extraction failure, not a "warning".
	if !hasExtractedSystemImage(targetDir) {
		cleanup()
		return fmt.Errorf(
			"%w (target=%s; require system.img + build.prop)",
			ErrSystemImageMalformed, targetDir,
		)
	}
	return nil
}

// commonZipPrefix returns the single shared leading directory of all
// entries (with trailing slash), or "" if there is no shared prefix.
// E.g. ["x86_64/system.img", "x86_64/build.prop"] → "x86_64/".
func commonZipPrefix(entries []*zip.File) string {
	if len(entries) == 0 {
		return ""
	}
	var first string
	for _, e := range entries {
		// Skip explicit directory entries to avoid spurious prefixes.
		if e.FileInfo().IsDir() {
			continue
		}
		first = e.Name
		break
	}
	if first == "" {
		return ""
	}
	slash := strings.IndexByte(first, '/')
	if slash <= 0 {
		return ""
	}
	candidate := first[:slash+1]
	for _, e := range entries {
		if e.FileInfo().IsDir() {
			// Directory entries can be the prefix itself ("x86_64/")
			// or sub-directories under it; both are fine. If a dir
			// entry doesn't start with candidate, there's no common
			// prefix.
			if !strings.HasPrefix(e.Name, candidate) && e.Name != strings.TrimSuffix(candidate, "/") {
				return ""
			}
			continue
		}
		if !strings.HasPrefix(e.Name, candidate) {
			return ""
		}
	}
	return candidate
}

// copyZipEntry writes the bytes of f to dest with f's mode bits.
func copyZipEntry(f *zip.File, dest string) error {
	src, err := f.Open()
	if err != nil {
		return fmt.Errorf("open zip entry %s: %w", f.Name, err)
	}
	defer src.Close()
	mode := f.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	return nil
}

// pruneEmptyDirs removes empty directories under root (best-effort,
// post-order). Used by extractSystemImageZip's cleanup path so a
// failed extraction leaves no stale skeleton behind.
func pruneEmptyDirs(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(root, e.Name())
		_ = pruneEmptyDirs(sub)
		if subEntries, err := os.ReadDir(sub); err == nil && len(subEntries) == 0 {
			_ = os.Remove(sub)
		}
	}
	if rootEntries, err := os.ReadDir(root); err == nil && len(rootEntries) == 0 {
		_ = os.Remove(root)
	}
	return nil
}

func (a *AndroidEmulator) emulatorBinary() string {
	return a.androidSdkRoot + "/emulator/emulator"
}

func (a *AndroidEmulator) adbBinary() string {
	return a.androidSdkRoot + "/platform-tools/adb"
}

// emulatorSerials parses `adb devices` output and returns the set of
// emulator console ports currently registered (e.g. emulator-5554 →
// {5554}). Used by Boot() to discover the port the newly-launched
// emulator actually binds to. Multi-AVD matrix runs MUST NOT assume
// every emulator lands on 5554/5555 — when a previous emulator's
// Teardown is still in flight (or failed silently), the next launch
// lands on 5556/5557, 5558/5559, etc.
//
// Forensic anchor (2026-05-04 evening, exposed by ultrathink-driven
// diagnostic instrumentation): the prior Boot() hardcoded ADBPort=5555
// regardless of actual binding, causing every iteration of a multi-AVD
// matrix to test against whichever emulator happened to bind 5554/5555
// FIRST — the subsequent AVDs' emulators silently ran their tests
// against the FIRST AVD's process, then died at the next Teardown
// call. Recorded as a clause-6.I clause-7 architecture bluff.
func (a *AndroidEmulator) emulatorSerials(ctx context.Context) (map[int]bool, error) {
	out, err := a.executor.Execute(ctx, a.adbBinary(), "devices")
	if err != nil {
		return nil, fmt.Errorf("adb devices failed: %w", err)
	}
	serials := make(map[int]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like:
		//   emulator-5554\tdevice
		//   emulator-5556\toffline
		//   localhost:5555\tdevice          (ignore — that's a network alias)
		// We capture every emulator-<port> regardless of state, because
		// even an offline emulator is taking up that port.
		if !strings.HasPrefix(line, "emulator-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var port int
		if _, scanErr := fmt.Sscanf(fields[0], "emulator-%d", &port); scanErr == nil && port > 0 {
			serials[port] = true
		}
	}
	return serials, nil
}

// discoverNewSerial polls `adb devices` until a console port appears
// that wasn't in `before`, or the timeout elapses. The returned port
// is the CONSOLE port (e.g. 5554); callers compute ADB port = console + 1.
func (a *AndroidEmulator) discoverNewSerial(
	ctx context.Context,
	before map[int]bool,
	timeout time.Duration,
) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := a.emulatorSerials(ctx)
		if err == nil {
			for port := range current {
				if !before[port] {
					return port, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("emulator port discovery cancelled: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("no new emulator serial appeared in adb devices within %s", timeout)
}

// Boot starts the AVD in headless mode. The emulator process runs
// asynchronously; this method returns once the new emulator's serial
// is observable in `adb devices` (typically 1-3 seconds after the
// underlying QEMU process binds its sockets) — NOT once Android has
// fully booted. Use WaitForBoot to wait for sys.boot_completed=1.
//
// Per clause 6.I clause 6, coldBoot=true SHOULD be used for any gating
// matrix run — it disables snapshot reload, ensuring reproducibility
// across runs.
//
// Boot dynamically discovers the console/ADB port the new emulator
// binds to by diffing `adb devices` before and after the launch. This
// is the constitutional fix for the 2026-05-04 ultrathink-discovered
// bluff (see emulatorSerials KDoc above). Without dynamic discovery,
// multi-AVD matrix runs silently test against the FIRST emulator
// every iteration.
//
// Robustness fixes (2026-06-03 forensic findings):
//
//  1. ADB hygiene: before launching the emulator, reset adb state by
//     disconnecting phantom TCP entries and cycling kill-server/start-server.
//     Phantom offline TCP entries (e.g. "localhost:5555 offline") wedge adb's
//     device tracking so discoverNewSerial never sees the new emulator.
//
//  2. Reap on timeout: when discoverNewSerial times out, the launched
//     qemu-system-* process is still alive consuming CPU/RAM. Boot now calls
//     Cleanup() to reap any orphaned qemu-system-* processes so they cannot
//     cause CPU starvation on the next boot attempt.
//
//  3. Diagnostic capture: on port-discovery timeout, a BootDiagnostic is
//     captured and embedded in the returned BootResult so the matrix runner
//     can record WHY the boot timed out in the attestation row.
func (a *AndroidEmulator) Boot(
	ctx context.Context,
	avd AVD,
	coldBoot bool,
) (BootResult, error) {
	// Pre-boot ADB hygiene: disconnect phantom TCP entries and cycle
	// kill-server / start-server so device tracking starts from a clean
	// state. This prevents the "phantom localhost:5555 offline" wedge that
	// caused discoverNewSerial to never see a new emulator.
	//
	// Best-effort: a hygiene failure is logged but does NOT abort the boot —
	// the emulator may still succeed if the phantom entry was already gone.
	//
	// adbHygieneHook is a package-level seam; tests that don't test hygiene
	// itself can swap it to a no-op so they don't need kill-server/start-server
	// entries in their fakeExecutor. See the seam declaration above.
	hygieneResult := adbHygieneHook(ctx, a.adbBinary(), a.executor)
	if hygieneResult.Err != nil {
		fmt.Fprintf(os.Stderr,
			"[boot] adb hygiene reset failed (continuing): %v\n",
			hygieneResult.Err,
		)
	} else if len(hygieneResult.PhantomTCPDisconnected) > 0 {
		fmt.Fprintf(os.Stderr,
			"[boot] disconnected phantom TCP entries: %v\n",
			hygieneResult.PhantomTCPDisconnected,
		)
	}

	// Snapshot existing emulator ports BEFORE launch so we can detect
	// the new one after launch. Errors here are non-fatal — empty map
	// is a safe baseline (we'll just claim the first emulator we see).
	before, _ := a.emulatorSerials(ctx)

	args := []string{
		"-avd", avd.Name,
		"-no-window",
		"-no-audio",
		"-no-boot-anim",
		"-gpu", "swiftshader_indirect",
	}
	if coldBoot {
		args = append(args, "-no-snapshot")
	}

	// We launch the emulator detached. Start (vs Execute) means the
	// underlying process keeps running after this call returns; the
	// caller must invoke Teardown via `adb emu kill` to stop it.
	startedAt := time.Now()
	if err := a.executor.Start(ctx, a.emulatorBinary(), args...); err != nil {
		return BootResult{
			AVD:          avd,
			Started:      false,
			BootDuration: time.Since(startedAt),
			Error:        fmt.Errorf("emulator launch failed: %w", err),
		}, err
	}

	// Discover the actual port the new emulator bound to. Bounded by a
	// 60s timeout — if adb doesn't see the new emulator within that,
	// something is structurally wrong (kvm denied, zygote crash, etc.)
	// and we fail loudly rather than silently mis-target later calls.
	newPort, derr := a.discoverNewSerial(ctx, before, 60*time.Second)
	if derr != nil {
		// Reap the launched emulator process to prevent a hot zombie from
		// consuming CPU/RAM and holding AVD lock files, which would cause
		// the NEXT boot attempt to also time out.
		//
		// We use Cleanup() here (not KillByPort) because we don't know the
		// port — the discovery timeout means we never learned which port the
		// emulator bound to. Cleanup() uses the strict "qemu-system-" comm
		// prefix so only Android emulator processes are targeted.
		cleanupReport, cleanupErr := Cleanup(ctx)
		if cleanupErr != nil {
			fmt.Fprintf(os.Stderr,
				"[boot] cleanup after port-discovery timeout: %v\n",
				cleanupErr,
			)
		} else {
			fmt.Fprintf(os.Stderr,
				"[boot] cleanup after port-discovery timeout: "+
					"found=%v terminated=%v killed=%v surviving=%v\n",
				cleanupReport.Found,
				cleanupReport.TerminatedTERM,
				cleanupReport.KilledKILL,
				cleanupReport.Surviving,
			)
		}

		// Capture diagnostic snapshot so the operator can see WHY the
		// boot timed out (per clause 6.I Group-B diag intent).
		diag := CaptureBootDiagnostic(ctx, a.adbBinary(), a.executor, 0, "")
		return BootResult{
			AVD:          avd,
			Started:      true,
			BootDuration: time.Since(startedAt),
			Error:        fmt.Errorf("emulator port discovery failed: %w", derr),
			BootDiag:     &diag,
		}, derr
	}

	return BootResult{
		AVD:          avd,
		Started:      true,
		BootDuration: time.Since(startedAt),
		ConsolePort:  newPort,
		ADBPort:      newPort + 1,
	}, nil
}

// WaitForBoot polls `getprop sys.boot_completed` via adb until it
// returns "1" or the timeout elapses. Returns the elapsed duration.
//
// The poll interval is 5 seconds (matches Lava's
// scripts/run-emulator-tests.sh contract before this package shipped,
// so the new package does not change observable behaviour).
func (a *AndroidEmulator) WaitForBoot(
	ctx context.Context,
	port int,
	timeout time.Duration,
) (time.Duration, error) {
	startedAt := time.Now()
	deadline := startedAt.Add(timeout)
	target := fmt.Sprintf("localhost:%d", port)

	// Forensic anchor (2026-05-04 evening): the previous form called
	// `adb connect localhost:<port>` ONCE before the poll loop.
	// On cold boot the emulator's ADB socket is not ready for ~30-60s
	// after the emulator process starts, so the pre-loop connect failed
	// silently (its err was discarded with `_, _`). Subsequent
	// `adb -s localhost:<port> shell getprop` calls then all returned
	// "device not found", the loop swallowed those errors as expected
	// "boot not yet ready" signals, and the timeout fired even though
	// the emulator booted successfully a few minutes in. Recorded as a
	// 6.A real-binary contract bug class — script's expectation of the
	// adb binary did not match the binary's reality.
	//
	// Fix: retry `adb connect` on every poll iteration. Connect is
	// idempotent (returns "already connected to ..." on second+ call)
	// so retrying carries no cost. The first iteration after the ADB
	// socket comes up actually establishes the connection; subsequent
	// `-s` calls then succeed and the boot-completed prop is read.
	for time.Now().Before(deadline) {
		connectOut, connectErr := a.executor.Execute(ctx, a.adbBinary(), "connect", target)
		// Classify the transport state returned by `adb connect` so we can
		// surface offline vs not-yet-visible in the diagnostic log.
		connectOutStr := strings.TrimSpace(string(connectOut))
		if connectErr != nil {
			fmt.Fprintf(os.Stderr,
				"[wait-for-boot] adb connect %s: %v\n", target, connectErr)
		} else if strings.Contains(connectOutStr, "offline") {
			fmt.Fprintf(os.Stderr,
				"[wait-for-boot] adb connect %s: offline (not yet ready)\n", target)
		}

		out, err := a.executor.Execute(
			ctx, a.adbBinary(), "-s", target,
			"shell", "getprop", "sys.boot_completed",
		)
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return time.Since(startedAt), nil
		}
		select {
		case <-ctx.Done():
			return time.Since(startedAt), ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	// Boot timed out. Reap the emulator process to prevent a hot zombie:
	// the qemu-system-* process is still alive consuming ~370% CPU and
	// holding AVD lock files. KillByPort targets the specific emulator by
	// its console port (port-1 of the ADB port) to avoid touching sibling
	// emulators on other ports.
	//
	// consolePort = adbPort - 1 (the canonical relationship between the
	// emulator's console port and its ADB port).
	consolePort := port - 1
	killReport, killErr := KillByPort(ctx, consolePort)
	if killErr != nil {
		fmt.Fprintf(os.Stderr,
			"[wait-for-boot] KillByPort(%d) after timeout: %v\n",
			consolePort, killErr,
		)
	} else if killReport.Matched > 0 {
		fmt.Fprintf(os.Stderr,
			"[wait-for-boot] KillByPort(%d): matched=%d sigtermed=%v sigkilled=%v surviving=%v\n",
			consolePort,
			killReport.Matched,
			killReport.Sigtermed,
			killReport.Sigkilled,
			killReport.Surviving,
		)
	} else {
		// Matched=0 means KillByPort found no process with "-port <consolePort>".
		// This is possible if the emulator used a different port or if the
		// process already exited on its own. Fall back to Cleanup() which uses
		// the broader "qemu-system-" comm prefix.
		cleanupReport, cleanupErr := Cleanup(ctx)
		if cleanupErr != nil {
			fmt.Fprintf(os.Stderr,
				"[wait-for-boot] Cleanup() fallback after KillByPort matched=0: %v\n",
				cleanupErr,
			)
		} else if len(cleanupReport.Found) > 0 {
			fmt.Fprintf(os.Stderr,
				"[wait-for-boot] Cleanup() fallback: found=%v terminated=%v killed=%v\n",
				cleanupReport.Found,
				cleanupReport.TerminatedTERM,
				cleanupReport.KilledKILL,
			)
		}
	}

	return time.Since(startedAt),
		fmt.Errorf("boot not completed within %s", timeout)
}

// Install installs the APK on the running emulator via `adb -s
// localhost:<port> install -r <apkPath>`.
func (a *AndroidEmulator) Install(
	ctx context.Context,
	port int,
	apkPath string,
) error {
	if _, err := os.Stat(apkPath); err != nil {
		return fmt.Errorf("apk not found at %s: %w", apkPath, err)
	}
	target := fmt.Sprintf("localhost:%d", port)
	out, err := a.executor.Execute(
		ctx, a.adbBinary(), "-s", target, "install", "-r", apkPath,
	)
	if err != nil {
		return fmt.Errorf("adb install failed: %w; output=%s", err, out)
	}
	if !strings.Contains(string(out), "Success") {
		return fmt.Errorf("adb install reported non-Success output: %s", out)
	}
	return nil
}

// RunInstrumentation runs `connectedDebugAndroidTest` for the named
// test class via gradle. The runner expects to be invoked from a
// project root that has a gradlew + the matching `:app:connected*`
// task wired (Lava's case). The current implementation shells out via
// gradlew; future versions MAY drive `adb shell am instrument`
// directly for less wrapper overhead.
//
// Diagnostic instrumentation (clause 6.I clause 7 forensics): before
// kicking off the test we log adb-devices state + the device's
// ro.product.model so a future operator can verify the test ran
// against the AVD the matrix runner intended.
func (a *AndroidEmulator) RunInstrumentation(
	ctx context.Context,
	port int,
	testClass string,
	timeout time.Duration,
) (string, bool, error) {
	if testClass == "" {
		return "", false, fmt.Errorf("testClass MUST be non-empty")
	}
	target := fmt.Sprintf("localhost:%d", port)

	// Forensic diagnostics — see clause 6.I architecture audit.
	devicesOut, _ := a.executor.Execute(ctx, a.adbBinary(), "devices")
	sdkOut, _ := a.executor.Execute(
		ctx, a.adbBinary(), "-s", target,
		"shell", "getprop", "ro.build.version.sdk",
	)
	deviceOut, _ := a.executor.Execute(
		ctx, a.adbBinary(), "-s", target,
		"shell", "getprop", "ro.product.device",
	)
	fmt.Fprintf(os.Stderr,
		"[matrix-diag] target=%s sdk=%q device=%q\n",
		target,
		strings.TrimSpace(string(sdkOut)),
		strings.TrimSpace(string(deviceOut)),
	)
	fmt.Fprintf(os.Stderr,
		"[matrix-diag-devices] %s\n",
		strings.ReplaceAll(strings.TrimSpace(string(devicesOut)), "\n", " | "),
	)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	gradleArgs := append(
		[]string{"./gradlew"},
		gradleConnectedTestArgs(a.gradleModule, testClass)...,
	)
	cmd := exec.CommandContext(runCtx, gradleArgs[0], gradleArgs[1:]...)
	cmd.Env = append(os.Environ(), "ANDROID_SERIAL="+target)
	out, err := cmd.CombinedOutput()
	output := string(out)
	passed := err == nil && strings.Contains(output, "BUILD SUCCESSFUL")
	if !passed && err == nil {
		err = fmt.Errorf("gradle exit zero but BUILD SUCCESSFUL not in output")
	}
	return output, passed, err
}

// setGradleModule sets the bare gradle module RunInstrumentation
// targets. Empty is a no-op (the constructor default stands). The
// matrix runner calls this to propagate MatrixConfig.GradleModule.
func (a *AndroidEmulator) setGradleModule(module string) {
	if module != "" {
		a.gradleModule = module
	}
}

// Teardown stops the emulator via `adb -s localhost:<port> emu kill`,
// then waits for the emulator process to actually exit before returning.
//
// Forensic anchor (2026-05-05): `adb emu kill` returns "OK: killing
// emulator, bye bye" almost immediately, but the underlying qemu-system
// process can take 10-30 seconds to actually exit. The pre-fix Teardown
// returned as soon as the kill command came back — so the next iteration's
// Boot started before the previous emulator's port (5554/5555) was freed.
// The new emulator landed on 5556/5557, and after the discovery-fix in
// commit 648a4bb the matrix correctly tested it, but accumulated 5
// concurrently-running emulators by iteration 5 — causing CPU/RAM
// pressure that produced flakes in the API 35 row of the 5-AVD matrix
// (whose standalone single-AVD run passed cleanly).
//
// Fix: after `adb emu kill`, poll `adb devices` until the localhost:<port>
// entry transitions out of "device" state (typically becomes "offline"
// or is removed entirely). Bound the wait at 30 seconds. If the process
// is still alive past the timeout, return an error so the matrix
// runner's caller can decide whether to escalate (SIGKILL the qemu pid).
func (a *AndroidEmulator) Teardown(ctx context.Context, port int) error {
	target := fmt.Sprintf("localhost:%d", port)
	out, err := a.executor.Execute(
		ctx, a.adbBinary(), "-s", target, "emu", "kill",
	)
	if err != nil {
		return fmt.Errorf("adb emu kill failed: %w; output=%s", err, out)
	}

	// Poll for the emulator to actually exit. Bound by teardownGracePeriod
	// (30s in production; tests override to keep the suite fast). "Exit"
	// means: the localhost:<port> entry is no longer in `adb devices`
	// output as "device" (it may briefly show "offline" while
	// disconnecting; that's fine — we treat that as gone).
	deadline := time.Now().Add(teardownGracePeriod)
	for time.Now().Before(deadline) {
		devicesOut, derr := a.executor.Execute(ctx, a.adbBinary(), "devices")
		if derr != nil {
			// Best effort; if adb itself fails, treat as kill-success
			// so we don't deadlock the matrix runner.
			return nil
		}
		stillAlive := false
		for _, line := range strings.Split(string(devicesOut), "\n") {
			if !strings.HasPrefix(line, target) {
				continue
			}
			fields := strings.Fields(line)
			// "localhost:5555\tdevice" → still alive
			// "localhost:5555\toffline" → transitioning, treat as gone
			if len(fields) >= 2 && fields[1] == "device" {
				stillAlive = true
				break
			}
		}
		if !stillAlive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	// Group B fast-path: the adb-emu-kill grace expired but the
	// emulator is still in /proc. Try a port-strict force-kill via
	// emulator.KillByPort. Matched==0 means no /proc entry passed
	// the strict adjacent-token check — concurrent emulators on
	// other ports are untouched, and we surface the original
	// "did not exit" error so the matrix runner records an honest
	// row failure.
	report, kerr := killByPortHook(ctx, port)
	if kerr != nil {
		// Forensic-only: log the KillByPort error but fall through
		// to the "did not exit" return. KillByPort errors are
		// best-effort signals, not gating ones.
		fmt.Fprintf(os.Stderr,
			"[teardown] KillByPort fast-path failed for port %d: %v\n",
			port, kerr,
		)
	}
	if report.Matched == 0 {
		return fmt.Errorf(
			"emulator on %s did not exit within %s after `adb emu kill`; KillByPort matched 0 processes (skip-on-mismatch safety)",
			target, teardownGracePeriod,
		)
	}
	// Re-poll briefly for /proc clearing.
	postDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(postDeadline) {
		devicesOut, derr := a.executor.Execute(ctx, a.adbBinary(), "devices")
		if derr != nil {
			return nil
		}
		stillAlive := false
		for _, line := range strings.Split(string(devicesOut), "\n") {
			if !strings.HasPrefix(line, target) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "device" {
				stillAlive = true
				break
			}
		}
		if !stillAlive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf(
		"emulator on %s did not exit within %s + KillByPort grace; %d process(es) still alive (sigtermed=%v sigkilled=%v surviving=%v)",
		target, teardownGracePeriod,
		report.Matched, report.Sigtermed, report.Sigkilled, report.Surviving,
	)
}
