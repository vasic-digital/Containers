package scheduler

import (
	"testing"

	"digital.vasic.containers/pkg/remote"
)

// TestScorer_CanFit_GPU_NegativeCount_DoesNotBypassGPUCheck is the §11.4.115
// guard for CT-HARDEN-SCHED-1: a negative GPURequirement.Count must NOT silently
// disable the "does this host have enough matching GPUs" check. Pre-fix CanFit
// special-cased only Count == 0; any Count < 0 fell through to `len(matches) <
// need` with need negative — never true for a non-negative len — so a host with
// ZERO matching GPUs reported as able to fit a GPU-requiring container. Fixed:
// `need <= 0` defaults to 1 (the documented "zero defaults to 1" contract).
func TestScorer_CanFit_GPU_NegativeCount_DoesNotBypassGPUCheck(t *testing.T) {
	s := NewResourceScorer(Options{ReservePercent: 0, OvercommitRatio: 1})
	res := &remote.HostResources{
		CPUCores:      8,
		MemoryTotalMB: 16_000,
		// zero GPUs on this host at all.
	}
	req := ContainerRequirements{
		CPUCores: 1,
		GPU:      &GPURequirement{Count: -1, MinVRAMMB: 1024},
	}
	if s.CanFit(res, req) {
		t.Fatalf("CanFit returned true for a GPU requirement (Count=-1) " +
			"against a host with zero GPUs; a negative Count bypassed the " +
			"len(matches) < need check")
	}
}

// TestScorer_CanFit_GPU_PositiveCount_StillFits is the negative-control for
// SCHED-1: a normal Count against a host with enough matching GPUs must still
// fit (the fix must not over-correct into always-false).
func TestScorer_CanFit_GPU_PositiveCount_StillFits(t *testing.T) {
	s := NewResourceScorer(Options{ReservePercent: 0, OvercommitRatio: 1})
	res := &remote.HostResources{
		CPUCores:      8,
		MemoryTotalMB: 16_000,
		GPU: []remote.GPUDevice{
			{Vendor: "nvidia", VRAMFreeMB: 8000, CUDASupported: true},
		},
	}
	req := ContainerRequirements{
		CPUCores: 1,
		GPU:      &GPURequirement{Count: 1, Vendor: "nvidia"},
	}
	if !s.CanFit(res, req) {
		t.Fatalf("CanFit returned false for Count=1 against a host with one " +
			"matching nvidia GPU; the fix must not over-correct")
	}
}

// TestScorer_Score_GPU_NegativeFreeVRAM_DoesNotInflateScore is the §11.4.115
// guard for CT-HARDEN-SCHED-2: a GPU reporting negative free VRAM (over-
// subscribed / bad telemetry) must NOT invert scoreGPU's ratio into a false
// maximum. Pre-fix only the exact-zero case was guarded; negative VRAMFreeMB
// with MinVRAMMB==0 gave (negative)/(negative) == 1.0, clamped to max score.
// Fixed: `best.VRAMFreeMB <= 0` scores 0 (matching the <= 0 guards used by
// scoreCPU/scoreMemory/scoreDisk in the same file).
func TestScorer_Score_GPU_NegativeFreeVRAM_DoesNotInflateScore(t *testing.T) {
	s := NewResourceScorer(Options{
		ReservePercent: 0, OvercommitRatio: 1,
		CPUWeight: 0, MemoryWeight: 0, DiskWeight: 0, NetworkWeight: 0,
		GPUWeight: 1,
	})
	res := &remote.HostResources{
		CPUCores:      8,
		MemoryTotalMB: 16_000,
		GPU: []remote.GPUDevice{
			{Vendor: "nvidia", VRAMFreeMB: -100, CUDASupported: true},
		},
	}
	req := ContainerRequirements{
		CPUCores: 1,
		GPU:      &GPURequirement{Count: 1, MinVRAMMB: 0, Vendor: "nvidia"},
	}
	got := s.Score(res, req)
	if got >= 1.0 {
		t.Fatalf("Score()=%.3f for a GPU with NEGATIVE free VRAM (-100MB); "+
			"sign inversion in scoreGPU produced a false maximum score", got)
	}
}

// TestMatchingGPUs_MinCompute_DoubleDigitMajorVersion is the §11.4.115 guard for
// CT-HARDEN-SCHED-3: a GPU whose compute-capability major reaches double digits
// (e.g. "10.0", real on current-generation hardware) must NOT be wrongly excluded
// against a single-digit MinCompute (e.g. "8.0"). Pre-fix the compare was a plain
// string `<`, and "10.0" < "8.0" is TRUE lexicographically (because '1' < '8')
// even though 10.0 > 8.0 numerically — silently rejecting a strictly newer GPU.
func TestMatchingGPUs_MinCompute_DoubleDigitMajorVersion(t *testing.T) {
	host := []remote.GPUDevice{
		{Vendor: "nvidia", VRAMFreeMB: 8000, ComputeCapability: "10.0"},
	}
	req := GPURequirement{Vendor: "nvidia", MinCompute: "8.0"}
	got := matchingGPUs(host, req)
	if len(got) != 1 {
		t.Fatalf("matchingGPUs excluded a GPU with ComputeCapability=10.0 "+
			"against MinCompute=8.0 (lexicographic string compare treats "+
			"\"10.0\" as less than \"8.0\"); got %d matches, want 1", len(got))
	}
}

// TestMatchingGPUs_MinCompute_StillExcludesTrulyLowerCapability is the
// negative-control for SCHED-3: a genuinely lower compute capability ("6.1")
// must still be excluded against a higher MinCompute ("8.0") — the fix must not
// simply always-match.
func TestMatchingGPUs_MinCompute_StillExcludesTrulyLowerCapability(t *testing.T) {
	host := []remote.GPUDevice{
		{Vendor: "nvidia", VRAMFreeMB: 8000, ComputeCapability: "6.1"},
	}
	req := GPURequirement{Vendor: "nvidia", MinCompute: "8.0"}
	got := matchingGPUs(host, req)
	if len(got) != 0 {
		t.Fatalf("matchingGPUs matched a GPU with ComputeCapability=6.1 " +
			"against MinCompute=8.0; want 0 matches (6.1 < 8.0 numerically)")
	}
}
