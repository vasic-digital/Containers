// SPDX-License-Identifier: Apache-2.0
package ios

// Wave-20 VM3-8-ARGSWEEP permanent regression guard (§11.4.115 GREEN polarity):
// every xcrun/simctl wrapper hands a caller-supplied identifier/path to simctl
// as a POSITIONAL argument. simctl (getopt-style) parses a leading-'-' token as
// an OPTION/FLAG, so a crafted UDID / .app path / bundle id / output path
// beginning with '-' is an argv flag-injection vector. checkSimctlArgs must
// refuse it BEFORE any spawn at every wrapper (Boot/Shutdown/Install/Launch/
// Screenshot/Recording).
//
// Anti-tautology anchor: checkSimctlArgs's `if strings.HasPrefix(a, "-") {` —
// disabling it (`if false && …`) lets every poison call fall through to the
// non-darwin ErrXcodeNotAvailable, flipping every subtest RED; restore → GREEN.

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWave20_VM3_8_SimctlWrappersRefuseLeadingDashArg(t *testing.T) {
	b := NewIOSBuilder()
	ctx := context.Background()
	to := time.Second

	cases := []struct {
		name string
		call func() error
	}{
		{"BootSimulator/udid", func() error { return b.BootSimulator(ctx, "-oProxyCommand=x", to) }},
		{"ShutdownSimulator/udid", func() error { return b.ShutdownSimulator(ctx, "-x", to) }},
		{"InstallApp/appPath", func() error { return b.InstallApp(ctx, "udid", "-evil.app", to) }},
		{"LaunchApp/bundleID", func() error { return b.LaunchApp(ctx, "udid", "-com.evil", to) }},
		{"Screenshot/outPath", func() error { return b.Screenshot(ctx, "udid", "-out.png", to) }},
		{"Recording/outPath", func() error { return b.Recording(ctx, "udid", "-out.mov", to) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.ErrorIs(t, c.call(), ErrUnsafeArg,
				"%s must refuse a leading-dash positional (simctl argv flag-injection) before spawning", c.name)
		})
	}

	// Benign positive control: a legitimate UDID passes the guard; on a
	// non-darwin host it then fails with ErrXcodeNotAvailable — NOT ErrUnsafeArg
	// — proving the guard is narrow to the flag-injection shape (not a blanket
	// reject).
	if runtime.GOOS != "darwin" {
		err := b.BootSimulator(ctx, "1A2B3C4D-0000-1111-2222-333344445555", to)
		require.ErrorIs(t, err, ErrXcodeNotAvailable)
		require.NotErrorIs(t, err, ErrUnsafeArg)
	}
}
