package emulator

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// avdresolve.go — LVA-014 fix #1 (2026-07-26): baked-AVD name resolution
// for the containerized runner.
//
// Forensic anchor: docs/CONTINUATION.md 2026-07-04 ("DEVICE GATE
// UNBLOCKED"). The containerized gate "boot hang" was diagnosed as an
// AVD-NAME MISMATCH: the baked emulator images
// (ghcr.io/vasic-digital/lava-android-emulator:apiNN-x86_64) bake exactly
// ONE AVD named "default" (see Containerfile: `avdmanager create avd
// --name "default"`), but the matrix runner passed the §6.AE.2 matrix
// names (CZ_API34_Phone etc.) straight through as ANDROID_AVD_NAME. The
// image entrypoint's pre-check exited in ~4s ("AVD not found"), the
// container's --rm reaped the log, and WaitForBoot polled the dead
// forwarded port to its deadline, misreporting the 4s exit as a boot
// timeout.
//
// This file makes the requested AVD name ADVISORY when the image carries
// a baked AVD for the requested api level: Boot queries the image's
// actual baked AVD list and maps the requested name:api:form entry to a
// real baked AVD. An exact name match is used verbatim (so
// `--avds "default:34:phone"` keeps its proven byte-compatible behavior);
// an api-level match substitutes the baked name with a stderr note; no
// match at all fails FAST, before any container is launched, with a
// clear error naming the available baked AVDs.
//
// API-level provenance (verified against the real api34-x86_64 image
// 2026-07-26): `avdmanager list avd` does NOT reliably print the numeric
// api level — this image's cmdline-tools version renders
// `Based on: Android 14.0 ("UpsideDownCake")` instead of
// `Based on: Android API 34`. The AUTHORITATIVE numeric level lives in
// each AVD's config.ini as
// `image.sysdir.1=system-images/android-<NN>/...`. The listing therefore
// runs a compound bash command inside the image: avdmanager output
// followed by a ==CONFIG== section grepping the sysdir key per baked
// AVD, and the parser joins the two on the AVD's Path line. Both forms
// of the Based-on line are parsed; the config.ini sysdir wins when the
// Based-on line carries no number.
//
// Anti-bluff posture (§6.J/§6.L):
//   - The resolution NEVER silently re-routes to a different api level:
//     api 28 requested against an api34-only image is an honest ERROR,
//     not a silent boot of api 34 labelled api 28.
//   - When the baked-AVD listing cannot be obtained (exec error) or
//     parses to zero entries, resolution FALLS BACK to the requested
//     name — the pre-fix behavior. This is deliberate fail-open: the
//     image entrypoint still pre-checks the name and the LVA-014 fix #2
//     container-liveness check in WaitForBoot now fails fast with the
//     container's logs, so a wrong name can no longer masquerade as a
//     boot timeout.
//
// Falsifiability: avdresolve_test.go pairs every rule with a mutation
// rehearsal (see the per-test Bluff-Audit comments).

// apiPlaceholder is the image-reference template token substituted with
// the AVD's API level at Boot time. Lets one --container-image value
// cover a whole multi-api matrix:
//
//	ghcr.io/vasic-digital/lava-android-emulator:api{api}-x86_64
//
// A reference WITHOUT the token is used verbatim for every AVD (the
// pre-LVA-014 behavior).
const apiPlaceholder = "{api}"

// listAVDsScript is the compound command executed INSIDE the emulator
// image to enumerate its baked AVDs. Section 1 is `avdmanager list avd`
// (names, devices, paths, and — on cmdline-tools versions that print
// it — the numeric api level). Section 2 (after ==CONFIG==) greps each
// baked AVD's config.ini for the authoritative
// `image.sysdir.1=system-images/android-<NN>/` key, because the observed
// api34 image's avdmanager prints the dessert name
// (`Android 14.0 ("UpsideDownCake")`) instead of the api number. The
// trailing `exit 0` keeps the container's exit status independent of the
// last grep's match result (grep exits 1 on no-match; a zero-AVD image
// must still parse as a successful listing of zero entries).
const listAVDsScript = `avdmanager list avd 2>/dev/null; ` +
	`echo "==CONFIG=="; ` +
	`for f in "$HOME"/.android/avd/*.avd/config.ini; do ` +
	`[ -f "$f" ] || continue; ` +
	`echo "CONFIGFILE: $f"; ` +
	`grep -E '^image\.sysdir\.1=system-images/android-[0-9]+/' "$f"; ` +
	`done; exit 0`

// bakedAVD describes one AVD baked into an emulator container image,
// parsed from the listAVDsScript output captured inside the image.
type bakedAVD struct {
	// Name is the AVD identifier (`Name: <value>` line).
	Name string
	// APILevel is the Android API level, from the
	// `Based on: Android API <N>` line when present, otherwise from the
	// config.ini `image.sysdir.1=system-images/android-<N>/` key joined
	// via Path. 0 when unparseable.
	APILevel int
	// Device is the device profile (`Device: pixel (Google)` line),
	// kept for diagnostic messages only.
	Device string
	// Path is the on-disk AVD directory (`Path:` line) — the join key
	// for the ==CONFIG== section.
	Path string
}

// avdAPIRe matches the "Based on: Android API 34" fragment of
// `avdmanager list avd` output (older/cmdline-tools formats that print
// the numeric level inline).
var avdAPIRe = regexp.MustCompile(`Based on:\s*Android API\s+(\d+)`)

// avdSysdirRe matches the authoritative api-level key in a baked AVD's
// config.ini: image.sysdir.1=system-images/android-34/google_apis/x86_64/
var avdSysdirRe = regexp.MustCompile(`^image\.sysdir\.1=system-images/android-(\d+)/`)

// configSectionMarker separates the avdmanager section from the
// config.ini grep section in listAVDsScript output.
const configSectionMarker = "==CONFIG=="

// parseAvdmanagerListAVD parses listAVDsScript output into the list of
// baked AVDs. Tolerates the "Available Android Virtual Devices:" header,
// blank lines, entries missing any api-level signal (APILevel stays 0),
// and output without the ==CONFIG== section entirely. Returns nil when
// no `Name:` line is present at all — callers treat that as "could not
// determine baked AVDs" and fall back to the requested name (fail-open,
// see file header).
func parseAvdmanagerListAVD(out string) []bakedAVD {
	sections := strings.SplitN(out, configSectionMarker, 2)
	var avds []bakedAVD
	for _, line := range strings.Split(sections[0], "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Name:"):
			avds = append(avds, bakedAVD{
				Name: strings.TrimSpace(strings.TrimPrefix(t, "Name:")),
			})
		case strings.HasPrefix(t, "Device:") && len(avds) > 0:
			avds[len(avds)-1].Device = strings.TrimSpace(strings.TrimPrefix(t, "Device:"))
		case strings.HasPrefix(t, "Path:") && len(avds) > 0:
			avds[len(avds)-1].Path = strings.TrimSpace(strings.TrimPrefix(t, "Path:"))
		default:
			if len(avds) > 0 {
				if m := avdAPIRe.FindStringSubmatch(t); m != nil {
					if lvl, err := strconv.Atoi(m[1]); err == nil {
						avds[len(avds)-1].APILevel = lvl
					}
				}
			}
		}
	}
	if len(sections) == 2 {
		// ==CONFIG== section: CONFIGFILE lines set the current AVD
		// directory; image.sysdir.1 lines carry the authoritative api.
		apiByDir := map[string]int{}
		currentDir := ""
		for _, line := range strings.Split(sections[1], "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "CONFIGFILE: ") {
				currentDir = strings.TrimSuffix(
					strings.TrimSpace(strings.TrimPrefix(t, "CONFIGFILE: ")),
					"/config.ini",
				)
				continue
			}
			if m := avdSysdirRe.FindStringSubmatch(t); m != nil && currentDir != "" {
				if lvl, err := strconv.Atoi(m[1]); err == nil {
					apiByDir[currentDir] = lvl
				}
			}
		}
		for i := range avds {
			if avds[i].APILevel == 0 && avds[i].Path != "" {
				if lvl, ok := apiByDir[avds[i].Path]; ok {
					avds[i].APILevel = lvl
				}
			}
		}
	}
	return avds
}

// describeBakedAVDs renders the baked-AVD list for operator-facing
// error messages, e.g. "default (api 34, device pixel (Google))".
func describeBakedAVDs(avds []bakedAVD) string {
	parts := make([]string, 0, len(avds))
	for _, a := range avds {
		desc := a.Name
		var meta []string
		if a.APILevel > 0 {
			meta = append(meta, fmt.Sprintf("api %d", a.APILevel))
		}
		if a.Device != "" {
			meta = append(meta, "device "+a.Device)
		}
		if len(meta) > 0 {
			desc += " (" + strings.Join(meta, ", ") + ")"
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, ", ")
}

// resolveImageForAVD substitutes the {api} template token in the
// configured image reference with the AVD's API level. A reference
// without the token is returned verbatim. A templated reference paired
// with an AVD whose APILevel is unknown (<= 0) is a configuration error
// — substituting nothing would produce a reference that either does not
// exist or (worse) resolves to a different api than the caller labelled
// the row with.
func resolveImageForAVD(image string, avd AVD) (string, error) {
	if !strings.Contains(image, apiPlaceholder) {
		return image, nil
	}
	if avd.APILevel <= 0 {
		return "", fmt.Errorf(
			"container image %q contains the %s template but AVD %q has no api level — supply name:api:form in --avds",
			image, apiPlaceholder, avd.Name,
		)
	}
	return strings.ReplaceAll(image, apiPlaceholder, strconv.Itoa(avd.APILevel)), nil
}

// listBakedAVDs runs listAVDsScript INSIDE the emulator image (via a
// throwaway `--rm --entrypoint bash` container) and parses the result.
// The listing runs the image's own tooling against its own filesystem,
// so it reports exactly what the image's entrypoint pre-check will see —
// no host-side guessing about baked state.
//
// The image reference flows after an explicit end-of-options "--"
// (EMU2-1 argv flag-injection hardening, same class as
// buildContainerRunArgs). The script itself is a compile-time constant
// (no caller input is interpolated into it).
func (c *Containerized) listBakedAVDs(
	ctx context.Context,
	image string,
) ([]bakedAVD, error) {
	out, err := c.executor.Execute(
		ctx, c.runtimeBinary,
		"run", "--rm", "--entrypoint", "bash", "--", image, "-c", listAVDsScript,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"%s run --entrypoint bash %s -c listAVDs: %w (output: %s)",
			c.runtimeBinary, image, err, string(out),
		)
	}
	return parseAvdmanagerListAVD(string(out)), nil
}

// resolveAVDName maps the requested AVD (name:api:form from the matrix
// spec) to a name actually baked into the emulator image. Rules, in
// order:
//
//  1. Exact name match → the requested name is used verbatim
//     (byte-compatible with the proven `--avds "default:34:phone"`
//     workaround).
//  2. Otherwise, a baked AVD whose api level equals avd.APILevel → the
//     baked name is substituted (the requested name was advisory) and a
//     human-readable note is returned for the operator log.
//  3. Otherwise → an error naming the requested AVD AND every available
//     baked AVD. No container is launched; the failure is immediate and
//     honest instead of a 4s entrypoint exit misreported as a boot
//     timeout.
//
// Fail-open fallbacks (return requested name, no note, no error):
//   - the baked-AVD listing exec itself failed (the subsequent
//     `podman run` in Boot surfaces the real image problem), OR
//   - the listing parsed to zero AVDs (unrecognised avdmanager output —
//     we have no evidence of what is baked; the image entrypoint
//     pre-check + the WaitForBoot liveness check are the backstop).
func (c *Containerized) resolveAVDName(
	ctx context.Context,
	avd AVD,
	image string,
) (resolved string, note string, err error) {
	baked, lerr := c.listBakedAVDs(ctx, image)
	if lerr != nil || len(baked) == 0 {
		return avd.Name, "", nil
	}
	for _, b := range baked {
		if b.Name == avd.Name {
			return avd.Name, "", nil
		}
	}
	if avd.APILevel > 0 {
		for _, b := range baked {
			if b.APILevel == avd.APILevel {
				return b.Name, fmt.Sprintf(
					"requested AVD %q is not baked into image %s; using baked AVD %q (api %d matches requested api %d; requested name was advisory)",
					avd.Name, image, b.Name, b.APILevel, avd.APILevel,
				), nil
			}
		}
	}
	return "", "", fmt.Errorf(
		"requested AVD %q (api %d, form %s) is not baked into image %s and no baked AVD matches api %d; available baked AVDs: %s",
		avd.Name, avd.APILevel, avd.FormFactor, image, avd.APILevel,
		describeBakedAVDs(baked),
	)
}

// noteToStderr emits a resolution note to stderr. Containerized has no
// logger field; matrix.go already uses fmt.Fprintf(os.Stderr, ...) for
// operator-facing warnings, and this follows the same convention.
func noteToStderr(note string) {
	fmt.Fprintf(os.Stderr, "[containerized] %s\n", note)
}
