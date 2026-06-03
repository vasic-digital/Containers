package crossbuild

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSELinux swaps the package-level selinuxEnabled seam for the
// duration of a test and restores it afterwards. Returning the restore
// via t.Cleanup keeps each table row hermetic even under -race.
func withSELinux(t *testing.T, fn func() bool) {
	t.Helper()
	prev := selinuxEnabled
	selinuxEnabled = fn
	t.Cleanup(func() { selinuxEnabled = prev })
}

// TestMountArg_RelabelSuffix_AllThreeBranches is the load-bearing
// exact-string table test. It covers ALL THREE relabel branches by
// injecting the SELinux detector + SharedMount flag and asserting the
// EXACT `-v` argument. A wrong suffix (e.g. forcing ":Z" always) flips
// at least one assertion — this is the §1.1 paired-mutation seam.
//
// §11.4.81 per-OS: the linux-SELinux (suffix ":Z"/":z") and
// linux-non-SELinux (suffix "") branches are exercised here via the
// injected detector; the darwin real-host branch has positive evidence
// in TestDetectSELinux_DarwinRealHost_ReturnsFalse below.
func TestMountArg_RelabelSuffix_AllThreeBranches(t *testing.T) {
	const src = "/host/work"
	const dst = "/work"

	cases := []struct {
		name        string
		selinuxOn   bool
		sharedMount bool
		want        string
	}{
		{
			name:        "selinux_active_private_uses_uppercase_Z",
			selinuxOn:   true,
			sharedMount: false,
			want:        "/host/work:/work:Z",
		},
		{
			name:        "selinux_active_shared_uses_lowercase_z",
			selinuxOn:   true,
			sharedMount: true,
			want:        "/host/work:/work:z",
		},
		{
			name:        "selinux_inactive_private_no_suffix",
			selinuxOn:   false,
			sharedMount: false,
			want:        "/host/work:/work",
		},
		{
			name:        "selinux_inactive_shared_no_suffix",
			selinuxOn:   false,
			sharedMount: true,
			want:        "/host/work:/work",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSELinux(t, func() bool { return tc.selinuxOn })
			got := mountArg(containerRunSpec{
				MountSource: src,
				MountTarget: dst,
				SharedMount: tc.sharedMount,
			})
			assert.Equal(t, tc.want, got,
				"exact -v string for selinuxOn=%v shared=%v", tc.selinuxOn, tc.sharedMount)
		})
	}
}

// TestDetectSELinux_DarwinRealHost_ReturnsFalse is the §11.4.81
// positive-evidence-on-the-host test for the darwin branch. It runs the
// REAL detector (not an injected fake) on whatever host executes the
// suite. On darwin the runtime.GOOS short-circuit MUST make detectSELinux
// return false and the produced suffix MUST be empty — proving the macOS
// branch is correct on the machine actually running the test. On a real
// Linux-SELinux host the same call returns true (selinuxfs present); the
// assertion is therefore gated on GOOS so each OS gets its own honest
// positive evidence rather than a blanket skip.
func TestDetectSELinux_DarwinRealHost_ReturnsFalse(t *testing.T) {
	got := detectSELinux()

	switch runtime.GOOS {
	case "darwin", "windows":
		// CLAUDE-2: no SELinux equivalent exists; the detector MUST
		// short-circuit to false BEFORE touching the filesystem.
		require.False(t, got,
			"detectSELinux MUST return false on %s (no SELinux); host=%s",
			runtime.GOOS, runtime.GOOS)
		// And the real-detector suffix must be empty on this host.
		prev := selinuxEnabled
		selinuxEnabled = detectSELinux
		t.Cleanup(func() { selinuxEnabled = prev })
		arg := mountArg(containerRunSpec{MountSource: "/a", MountTarget: "/b"})
		assert.Equal(t, "/a:/b", arg,
			"real detector on %s must yield no relabel suffix", runtime.GOOS)
	case "linux":
		// On Linux the result reflects reality: true iff selinuxfs is
		// mounted. We assert the detector agrees with the filesystem
		// oracle so the linux branch also carries positive evidence.
		_, statErr := os.Stat("/sys/fs/selinux/enforce")
		want := statErr == nil
		assert.Equal(t, want, got,
			"detectSELinux on linux must match presence of /sys/fs/selinux/enforce")
	default:
		t.Skipf("SKIP-OK: no SELinux-oracle defined for GOOS=%s", runtime.GOOS)
	}
}

// TestMountArg_StressDeterministicConcurrent is the §11.4.85 STRESS
// test: N>=100 iterations across >=10 goroutines, each computing the
// suffix under a fixed injected detector, asserting deterministic output
// and (under -race) no data race on the injected seam.
func TestMountArg_StressDeterministicConcurrent(t *testing.T) {
	const goroutines = 16
	const iterations = 200

	withSELinux(t, func() bool { return true }) // private => ":Z"
	const want = "/host/work:/work:Z"

	var wg sync.WaitGroup
	var mismatches int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				got := mountArg(containerRunSpec{
					MountSource: "/host/work",
					MountTarget: "/work",
				})
				if got != want {
					atomic.AddInt64(&mismatches, 1)
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(0), atomic.LoadInt64(&mismatches),
		"suffix computation must be deterministic across %d concurrent goroutines x %d iters",
		goroutines, iterations)
}

// TestMountArg_ChaosDetectorFaults is the §11.4.85 CHAOS test. It injects
// faulty detectors (one that panics, simulating an unreadable selinux fs
// handler that blows up) and asserts the run path SAFELY DEFAULTS to no
// relabel (suffix "") and never crashes. Failing safe is correct: a
// missing/unreadable selinux fs means SELinux is not active.
func TestMountArg_ChaosDetectorFaults(t *testing.T) {
	t.Run("panicking_detector_fails_safe_to_no_suffix", func(t *testing.T) {
		withSELinux(t, func() bool { panic("simulated unreadable selinuxfs") })
		var got string
		require.NotPanics(t, func() {
			got = mountArg(containerRunSpec{MountSource: "/a", MountTarget: "/b"})
		}, "a panicking detector must not crash the run path")
		assert.Equal(t, "/a:/b", got,
			"panicking detector must fail safe to no relabel suffix")
	})

	t.Run("detector_returns_false_for_missing_fs", func(t *testing.T) {
		// Simulates os.Stat failing (selinuxfs not mounted): detector
		// returns false, suffix empty.
		withSELinux(t, func() bool { return false })
		got := mountArg(containerRunSpec{MountSource: "/a", MountTarget: "/b"})
		assert.Equal(t, "/a:/b", got)
	})
}
