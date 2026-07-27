package emulator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// avdresolve_test.go — LVA-014 fix #1 test pack (baked-AVD name
// resolution). Forensic anchor: docs/CONTINUATION.md 2026-07-04 — the
// api34-x86_64 image bakes exactly ONE AVD named "default"; the runner
// passed CZ_API34_Phone verbatim, the entrypoint exited in ~4s, and
// WaitForBoot misreported it as a boot timeout. These tests pin:
//
//   - the avdmanager-output parser,
//   - the resolution rules (exact match → verbatim; api match →
//     substitute with note; no match → fast error naming available
//     AVDs; listing failure/zero-parse → fail-open to requested name),
//   - the Boot-level wiring (ANDROID_AVD_NAME carries the RESOLVED
//     name; a no-match AVD fails BEFORE any `run -d` boot call),
//   - the {api} image-reference template.
//
// Bluff-Audit mutation rehearsals are recorded per test; the commit
// body carries the executed rehearsal evidence.

// avdmanagerSample mirrors the REAL listAVDsScript output of the baked
// api34-x86_64 image (captured 2026-07-26): one AVD named "default",
// the dessert-name Based-on line (no numeric api), and the ==CONFIG==
// section carrying the authoritative sysdir api key.
const avdmanagerSample = `Available Android Virtual Devices:
    Name: default
  Device: pixel (Google)
    Path: /home/emulator/.android/avd/default.avd
  Target: Google APIs (Google Inc.)
          Based on: Android 14.0 ("UpsideDownCake") Tag/ABI: google_apis/x86_64
  Sdcard: 512 MB
==CONFIG==
CONFIGFILE: /home/emulator/.android/avd/default.avd/config.ini
image.sysdir.1=system-images/android-34/google_apis/x86_64/
`

// avdmanagerTwoAVDs exercises multi-entry parsing: entry 1 resolves its
// api from the ==CONFIG== join, entry 2 from the inline
// "Based on: Android API 28" form (older cmdline-tools output).
const avdmanagerTwoAVDs = `Available Android Virtual Devices:
    Name: default
  Device: pixel (Google)
    Path: /home/emulator/.android/avd/default.avd
  Target: Google APIs (Google Inc.)
          Based on: Android 14.0 ("UpsideDownCake") Tag/ABI: google_apis/x86_64
---------
    Name: legacy28
  Device: pixel_xl (Google)
    Path: /home/emulator/.android/avd/legacy28.avd
  Target: Google APIs (Google Inc.)
          Based on: Android API 28 Tag/ABI: google_apis/x86_64
==CONFIG==
CONFIGFILE: /home/emulator/.android/avd/default.avd/config.ini
image.sysdir.1=system-images/android-34/google_apis/x86_64/
CONFIGFILE: /home/emulator/.android/avd/legacy28.avd/config.ini
image.sysdir.1=system-images/android-28/google_apis/x86_64/
`

// listAVDKey returns the fakeExecutor script key for the baked-AVD
// listing exec against the given image.
func listAVDKey(image string) string {
	return "podman run --rm --entrypoint bash -- " + image + " -c " + listAVDsScript
}

// TestParseAvdmanagerListAVD pins the parser against realistic output.
//
//	Mutation rehearsal: drop the avdAPIRe-based APILevel assignment →
//	the api assertions fail ("APILevel = 0, want 34"). Reverted: yes.
func TestParseAvdmanagerListAVD(t *testing.T) {
	t.Run("single baked AVD", func(t *testing.T) {
		avds := parseAvdmanagerListAVD(avdmanagerSample)
		if len(avds) != 1 {
			t.Fatalf("expected 1 baked AVD, got %d: %+v", len(avds), avds)
		}
		if avds[0].Name != "default" {
			t.Errorf("Name = %q, want %q", avds[0].Name, "default")
		}
		if avds[0].APILevel != 34 {
			t.Errorf("APILevel = %d, want 34", avds[0].APILevel)
		}
		if avds[0].Device != "pixel (Google)" {
			t.Errorf("Device = %q, want %q", avds[0].Device, "pixel (Google)")
		}
	})
	t.Run("two baked AVDs keep per-entry api levels", func(t *testing.T) {
		avds := parseAvdmanagerListAVD(avdmanagerTwoAVDs)
		if len(avds) != 2 {
			t.Fatalf("expected 2 baked AVDs, got %d: %+v", len(avds), avds)
		}
		if avds[0].Name != "default" || avds[0].APILevel != 34 {
			t.Errorf("entry 0 = %+v, want default/34", avds[0])
		}
		if avds[1].Name != "legacy28" || avds[1].APILevel != 28 {
			t.Errorf("entry 1 = %+v, want legacy28/28", avds[1])
		}
	})
	t.Run("no Name lines parses to zero (fail-open signal)", func(t *testing.T) {
		if avds := parseAvdmanagerListAVD("container-id\n"); len(avds) != 0 {
			t.Errorf("expected zero AVDs from unparseable output, got %+v", avds)
		}
	})
}

// newResolverContainerized builds a Containerized whose executor
// answers the baked-AVD listing with listingOut/listingErr and every
// other podman call with a bare success.
func newResolverContainerized(t *testing.T, image, listingOut string, listingErr error) (*Containerized, *fakeExecutor) {
	t.Helper()
	fake := &fakeExecutor{
		scripts: map[string]fakeScript{
			listAVDKey(image): {Out: []byte(listingOut), Err: listingErr},
			"podman":          {Out: []byte("container-id\n")},
		},
	}
	c, err := NewContainerized(ContainerizedConfig{
		RuntimeBinary: "podman",
		Image:         image,
		Executor:      fake,
	})
	if err != nil {
		t.Fatalf("NewContainerized: %v", err)
	}
	return c, fake
}

// TestContainerized_resolveAVDName_ExactNameMatchKeepsRequested pins
// rule 1 — `--avds "default:34:phone"` (the verified 2026-07-04
// workaround) stays byte-compatible.
func TestContainerized_resolveAVDName_ExactNameMatchKeepsRequested(t *testing.T) {
	c, _ := newResolverContainerized(t, "reg.test/emu:api34-x86_64", avdmanagerSample, nil)
	resolved, note, err := c.resolveAVDName(context.Background(),
		AVD{Name: "default", APILevel: 34, FormFactor: "phone"}, "reg.test/emu:api34-x86_64")
	if err != nil {
		t.Fatalf("resolveAVDName: %v", err)
	}
	if resolved != "default" {
		t.Errorf("resolved = %q, want %q (exact match must be verbatim)", resolved, "default")
	}
	if note != "" {
		t.Errorf("note must be empty on exact match; got %q", note)
	}
}

// TestContainerized_resolveAVDName_APIMatchSubstitutesBakedName pins
// rule 2 — the §6.AE.2 matrix name CZ_API34_Phone is ADVISORY: the
// image's baked api-34 AVD ("default") wins, with an operator note.
//
//	Mutation rehearsal: return avd.Name instead of b.Name in the
//	api-match branch → resolved assertion fails
//	("resolved = CZ_API34_Phone, want default"). Reverted: yes.
func TestContainerized_resolveAVDName_APIMatchSubstitutesBakedName(t *testing.T) {
	c, _ := newResolverContainerized(t, "reg.test/emu:api34-x86_64", avdmanagerSample, nil)
	resolved, note, err := c.resolveAVDName(context.Background(),
		AVD{Name: "CZ_API34_Phone", APILevel: 34, FormFactor: "phone"}, "reg.test/emu:api34-x86_64")
	if err != nil {
		t.Fatalf("resolveAVDName: %v", err)
	}
	if resolved != "default" {
		t.Errorf("resolved = %q, want %q (baked AVD wins on api match)", resolved, "default")
	}
	if !strings.Contains(note, "CZ_API34_Phone") || !strings.Contains(note, "default") {
		t.Errorf("note must name both requested and baked AVD; got %q", note)
	}
}

// TestContainerized_resolveAVDName_NoMatchFailsNamingAvailable pins
// rule 3 — api 28 requested against an api34-only image is an honest
// fast ERROR, never a silent re-route to the wrong api (that would be
// an AVD-shadow bluff: a row labelled api 28 that actually ran api 34).
func TestContainerized_resolveAVDName_NoMatchFailsNamingAvailable(t *testing.T) {
	c, _ := newResolverContainerized(t, "reg.test/emu:api34-x86_64", avdmanagerSample, nil)
	_, _, err := c.resolveAVDName(context.Background(),
		AVD{Name: "CZ_API28_Phone", APILevel: 28, FormFactor: "phone"}, "reg.test/emu:api34-x86_64")
	if err == nil {
		t.Fatal("resolveAVDName must fail when no baked AVD matches the requested api")
	}
	for _, want := range []string{"CZ_API28_Phone", "api 28", "default", "api 34", "available"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q for operator diagnosis; got: %v", want, err)
		}
	}
}

// TestContainerized_resolveAVDName_FailOpenFallbacks pins the two
// fail-open paths: listing exec error AND zero-parse output both fall
// back to the requested name (the entrypoint pre-check + the WaitForBoot
// liveness check are the backstop; resolution must not invent a hard
// error when it has no evidence of what is baked).
func TestContainerized_resolveAVDName_FailOpenFallbacks(t *testing.T) {
	t.Run("listing exec error falls back to requested name", func(t *testing.T) {
		c, _ := newResolverContainerized(t, "reg.test/emu:missing", "",
			errors.New("exit 125: image not found"))
		resolved, note, err := c.resolveAVDName(context.Background(),
			AVD{Name: "CZ_API34_Phone", APILevel: 34}, "reg.test/emu:missing")
		if err != nil {
			t.Fatalf("must fall back, got error: %v", err)
		}
		if resolved != "CZ_API34_Phone" || note != "" {
			t.Errorf("fallback must return requested name verbatim with no note; got %q note %q", resolved, note)
		}
	})
	t.Run("zero parsed AVDs falls back to requested name", func(t *testing.T) {
		c, _ := newResolverContainerized(t, "reg.test/emu:api34-x86_64", "container-id\n", nil)
		resolved, _, err := c.resolveAVDName(context.Background(),
			AVD{Name: "CZ_API34_Phone", APILevel: 34}, "reg.test/emu:api34-x86_64")
		if err != nil {
			t.Fatalf("must fall back, got error: %v", err)
		}
		if resolved != "CZ_API34_Phone" {
			t.Errorf("fallback must return requested name; got %q", resolved)
		}
	})
}

// bootCallArgs finds the `podman run -d …` boot invocation (the
// baked-AVD listing call is also a `podman run` — distinguished by -d).
func bootCallArgs(t *testing.T, calls []fakeCall) []string {
	t.Helper()
	for _, c := range calls {
		if c.Name != "podman" {
			continue
		}
		for _, a := range c.Args {
			if a == "-d" {
				return c.Args
			}
		}
	}
	t.Fatalf("no `podman run -d` boot call in %d calls: %+v", len(calls), calls)
	return nil
}

// TestContainerized_Boot_SubstitutesBakedAVDNameIntoEnv is the
// end-to-end pin of fix #1: Boot with the §6.AE.2 matrix name
// CZ_API34_Phone against an api34 image whose only baked AVD is
// "default" MUST launch the container with ANDROID_AVD_NAME=default —
// the exact mismatch that produced the 2026-07-04 "boot hang".
//
//	Bluff-Audit: removing the runAVD substitution in Boot (passing avd
//	verbatim to buildContainerRunArgs) fails this test with
//	"ANDROID_AVD_NAME=CZ_API34_Phone" on the wire. Rehearsal executed;
//	reverted.
func TestContainerized_Boot_SubstitutesBakedAVDNameIntoEnv(t *testing.T) {
	image := "reg.test/emu:api34-x86_64"
	c, fake := newResolverContainerized(t, image, avdmanagerSample, nil)

	result, err := c.Boot(context.Background(),
		AVD{Name: "CZ_API34_Phone", APILevel: 34, FormFactor: "phone"}, true)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	args := bootCallArgs(t, fake.calls)
	foundEnv := ""
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "ANDROID_AVD_NAME=") {
			foundEnv = strings.TrimPrefix(args[i+1], "ANDROID_AVD_NAME=")
		}
	}
	if foundEnv != "default" {
		t.Errorf("ANDROID_AVD_NAME = %q, want %q (baked AVD substituted)", foundEnv, "default")
	}
	if result.ResolvedAVDName != "default" {
		t.Errorf("BootResult.ResolvedAVDName = %q, want %q", result.ResolvedAVDName, "default")
	}
	if result.AVD.Name != "CZ_API34_Phone" {
		t.Errorf("BootResult.AVD.Name must keep the REQUESTED matrix identity; got %q", result.AVD.Name)
	}
}

// TestContainerized_Boot_NoMatchingBakedAVDFailsFast pins the honest
// fast failure: Boot with an api the image does not carry errors BEFORE
// any `podman run -d` boot call, naming the available baked AVDs —
// instead of launching a container that exits in ~4s and gets
// misreported as a boot timeout.
func TestContainerized_Boot_NoMatchingBakedAVDFailsFast(t *testing.T) {
	image := "reg.test/emu:api34-x86_64"
	c, fake := newResolverContainerized(t, image, avdmanagerSample, nil)

	result, err := c.Boot(context.Background(),
		AVD{Name: "CZ_API28_Phone", APILevel: 28, FormFactor: "phone"}, true)
	if err == nil {
		t.Fatal("Boot must fail fast when no baked AVD matches the requested api")
	}
	if result.Started {
		t.Error("BootResult.Started must be false on resolution failure")
	}
	if !strings.Contains(err.Error(), "default") || !strings.Contains(err.Error(), "available") {
		t.Errorf("error must name the available baked AVDs; got: %v", err)
	}
	for _, call := range fake.calls {
		for _, a := range call.Args {
			if a == "-d" {
				t.Errorf("no `run -d` boot call may be issued on resolution failure; got: %v", call.Args)
			}
		}
	}
}

// TestContainerized_Boot_ResolvesImageAPITemplate pins the {api} image
// template: one --container-image value covers a whole multi-api
// matrix; Boot substitutes the AVD's api level into BOTH the baked-AVD
// listing call and the boot call.
func TestContainerized_Boot_ResolvesImageAPITemplate(t *testing.T) {
	tmpl := "reg.test/emu:api{api}-x86_64"
	resolved34 := "reg.test/emu:api34-x86_64"
	c, fake := newResolverContainerized(t, tmpl, avdmanagerSample, nil)

	if _, err := c.Boot(context.Background(),
		AVD{Name: "CZ_API34_Phone", APILevel: 34, FormFactor: "phone"}, true); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	args := bootCallArgs(t, fake.calls)
	foundImage := false
	for _, a := range args {
		if a == resolved34 {
			foundImage = true
		}
		if strings.Contains(a, apiPlaceholder) {
			t.Errorf("template token must be fully substituted; got arg %q", a)
		}
	}
	if !foundImage {
		t.Errorf("boot call must use the substituted image %q; args: %v", resolved34, args)
	}
	// The listing call must have used the substituted image too — the
	// fake answered listAVDKey(resolved34), so reaching Boot at all
	// proves it, but assert the wire call explicitly.
	foundListing := false
	for _, call := range fake.calls {
		if call.Name == "podman" && strings.Contains(strings.Join(call.Args, " "),
			"--entrypoint bash -- "+resolved34) {
			foundListing = true
		}
	}
	if !foundListing {
		t.Errorf("baked-AVD listing must query the substituted image %q; calls: %+v", resolved34, fake.calls)
	}
}

// TestResolveImageForAVD pins the pure template helper: no token →
// verbatim; token + unknown api level → honest configuration error.
func TestResolveImageForAVD(t *testing.T) {
	t.Run("no template returned verbatim", func(t *testing.T) {
		got, err := resolveImageForAVD("reg.test/emu:api34-x86_64", AVD{Name: "x", APILevel: 34})
		if err != nil || got != "reg.test/emu:api34-x86_64" {
			t.Errorf("got %q err %v; want verbatim, nil", got, err)
		}
	})
	t.Run("template without api level fails", func(t *testing.T) {
		_, err := resolveImageForAVD("reg.test/emu:api{api}-x86_64", AVD{Name: "x"})
		if err == nil {
			t.Fatal("must fail when the AVD carries no api level for the {api} template")
		}
		if !strings.Contains(err.Error(), "{api}") {
			t.Errorf("error must name the template token; got: %v", err)
		}
	})
}
