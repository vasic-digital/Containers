package scheduler

import (
	"strconv"
	"strings"

	"digital.vasic.containers/pkg/remote"
)

// ResourceScorer scores a host from 0.0 to 1.0 based on available
// resources and configured weights.
type ResourceScorer struct {
	opts Options
}

// NewResourceScorer creates a scorer with the given options.
func NewResourceScorer(opts Options) *ResourceScorer {
	return &ResourceScorer{opts: opts}
}

// Score evaluates a host's suitability for a container. Returns a
// value between 0.0 (worst) and 1.0 (best).
func (s *ResourceScorer) Score(
	resources *remote.HostResources,
	req ContainerRequirements,
) float64 {
	if resources == nil {
		return 0
	}

	reserve := s.opts.ReservePercent
	overcommit := s.opts.OvercommitRatio
	if overcommit <= 0 {
		overcommit = 1.0
	}

	// CPU score: available CPU relative to requirement.
	cpuScore := s.scoreCPU(resources, req, reserve, overcommit)

	// Memory score: available memory relative to requirement.
	memScore := s.scoreMemory(resources, req, reserve, overcommit)

	// Disk score: available disk relative to requirement.
	diskScore := s.scoreDisk(resources, req, reserve)

	// Network score: inversely proportional to network usage.
	netScore := s.scoreNetwork(resources)

	// GPU score: VRAM headroom above requirement.
	gpuScore := s.scoreGPU(resources, req)

	// Weighted sum.
	total := cpuScore*s.opts.CPUWeight +
		memScore*s.opts.MemoryWeight +
		diskScore*s.opts.DiskWeight +
		netScore*s.opts.NetworkWeight +
		gpuScore*s.opts.GPUWeight

	return clamp(total, 0, 1)
}

// CanFit returns true if the host has enough resources for the
// container, accounting for reserves.
func (s *ResourceScorer) CanFit(
	resources *remote.HostResources,
	req ContainerRequirements,
) bool {
	if resources == nil {
		return false
	}

	reserve := s.opts.ReservePercent
	overcommit := s.opts.OvercommitRatio
	if overcommit <= 0 {
		overcommit = 1.0
	}

	// Check CPU.
	if req.CPUCores > 0 {
		availCores := float64(resources.CPUCores) *
			(resources.AvailableCPUPercent() - reserve) / 100.0 *
			overcommit
		if availCores < req.CPUCores {
			return false
		}
	}

	// Check memory.
	if req.MemoryMB > 0 {
		availMB := float64(resources.MemoryTotalMB) *
			(resources.AvailableMemoryPercent() - reserve) / 100.0 *
			overcommit
		if availMB < 0 || uint64(availMB) < req.MemoryMB {
			return false
		}
	}

	// Check disk.
	if req.DiskMB > 0 {
		availDiskMB := float64(resources.DiskTotalMB) *
			(resources.AvailableDiskPercent() - reserve) / 100.0
		if availDiskMB < 0 || uint64(availDiskMB) < req.DiskMB {
			return false
		}
	}

	// Check GPU.
	if req.GPU != nil {
		matches := matchingGPUs(resources.GPU, *req.GPU)
		need := req.GPU.Count
		// A negative Count is nonsensical; treat it (like zero) as the
		// documented "defaults to 1", so it cannot make len(matches) < need
		// vacuously false and bypass the availability check (CT-HARDEN-SCHED-1).
		if need <= 0 {
			need = 1
		}
		if len(matches) < need {
			return false
		}
	}

	return true
}

func (s *ResourceScorer) scoreCPU(
	r *remote.HostResources,
	req ContainerRequirements,
	reserve, overcommit float64,
) float64 {
	avail := r.AvailableCPUPercent() - reserve
	if avail <= 0 {
		return 0
	}
	score := avail / 100.0
	if req.CPUCores > 0 && r.CPUCores > 0 {
		availCores := float64(r.CPUCores) * avail / 100.0 * overcommit
		if availCores < req.CPUCores {
			return 0
		}
		score = (availCores - req.CPUCores) / float64(r.CPUCores)
	}
	return clamp(score, 0, 1)
}

func (s *ResourceScorer) scoreMemory(
	r *remote.HostResources,
	req ContainerRequirements,
	reserve, overcommit float64,
) float64 {
	avail := r.AvailableMemoryPercent() - reserve
	if avail <= 0 {
		return 0
	}
	score := avail / 100.0
	if req.MemoryMB > 0 && r.MemoryTotalMB > 0 {
		availMB := float64(r.MemoryTotalMB) * avail / 100.0 * overcommit
		if uint64(availMB) < req.MemoryMB {
			return 0
		}
		score = (availMB - float64(req.MemoryMB)) /
			float64(r.MemoryTotalMB)
	}
	return clamp(score, 0, 1)
}

func (s *ResourceScorer) scoreDisk(
	r *remote.HostResources,
	req ContainerRequirements,
	reserve float64,
) float64 {
	avail := r.AvailableDiskPercent() - reserve
	if avail <= 0 {
		return 0
	}
	score := avail / 100.0
	if req.DiskMB > 0 && r.DiskTotalMB > 0 {
		availMB := float64(r.DiskTotalMB) * avail / 100.0
		if uint64(availMB) < req.DiskMB {
			return 0
		}
		score = (availMB - float64(req.DiskMB)) /
			float64(r.DiskTotalMB)
	}
	return clamp(score, 0, 1)
}

func (s *ResourceScorer) scoreNetwork(
	r *remote.HostResources,
) float64 {
	// Simple heuristic: lower network usage is better.
	// Normalize to a reasonable range (1 Gbps = 125MB/s).
	const maxBytesPerSec = 125_000_000
	totalUsage := float64(
		r.NetworkRxBytesPerSec + r.NetworkTxBytesPerSec,
	)
	if totalUsage >= maxBytesPerSec {
		return 0
	}
	return 1.0 - (totalUsage / maxBytesPerSec)
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func (s *ResourceScorer) scoreGPU(
	r *remote.HostResources,
	req ContainerRequirements,
) float64 {
	if req.GPU == nil {
		return 0
	}
	matches := matchingGPUs(r.GPU, *req.GPU)
	if len(matches) == 0 {
		return 0
	}
	// Pick the matching GPU with the most free VRAM.
	best := matches[0]
	for _, g := range matches[1:] {
		if g.VRAMFreeMB > best.VRAMFreeMB {
			best = g
		}
	}
	// Non-positive free VRAM (0, or negative from an over-subscribed / bad
	// telemetry reading) scores 0 (worst), never a false maximum: with
	// MinVRAMMB==0 a negative best.VRAMFreeMB would give negative/negative ==
	// 1.0. This matches the `<= 0` headroom guards in scoreCPU/scoreMemory/
	// scoreDisk (CT-HARDEN-SCHED-2).
	if best.VRAMFreeMB <= 0 {
		return 0
	}
	ratio := float64(best.VRAMFreeMB-req.GPU.MinVRAMMB) /
		float64(best.VRAMFreeMB)
	return clamp(ratio, 0, 1)
}

// matchingGPUs returns the subset of host GPUs that satisfy req.
func matchingGPUs(
	host []remote.GPUDevice,
	req GPURequirement,
) []remote.GPUDevice {
	out := make([]remote.GPUDevice, 0, len(host))
	for _, g := range host {
		if req.Vendor != "" && g.Vendor != req.Vendor {
			continue
		}
		if req.MinVRAMMB > 0 && g.VRAMFreeMB < req.MinVRAMMB {
			continue
		}
		if req.MinCompute != "" && computeCapabilityLess(g.ComputeCapability, req.MinCompute) {
			continue
		}
		if !capabilitiesMatch(g, req.Capabilities) {
			continue
		}
		out = append(out, g)
	}
	return out
}

func capabilitiesMatch(g remote.GPUDevice, req []string) bool {
	for _, c := range req {
		switch c {
		case "cuda":
			if !g.CUDASupported {
				return false
			}
		case "nvenc":
			if !g.NVENCSupported {
				return false
			}
		case "tensorrt":
			// TensorRT requires CUDA at minimum. Refine per model in
			// later sub-projects.
			if !g.CUDASupported {
				return false
			}
		case "vulkan":
			if !g.VulkanSupported {
				return false
			}
		case "opencl":
			if !g.OpenCLSupported {
				return false
			}
		case "rocm":
			if !g.ROCmSupported {
				return false
			}
		}
	}
	return true
}

// computeCapabilityLess reports whether compute-capability a is less than b,
// comparing "major.minor" version strings (e.g. "8.6", "10.0") numerically
// component-by-component. A pure lexicographic string compare breaks the moment
// either side reaches a double-digit major version — "10.0" < "8.0" is true
// lexicographically but false numerically — which is real on current-generation
// GPU hardware (compute capability 10.x/12.x). Falls back to the original
// lexicographic compare when either string does not parse as numeric
// major[.minor], so malformed input keeps its prior (already-documented)
// behavior rather than panicking or guessing (CT-HARDEN-SCHED-3).
func computeCapabilityLess(a, b string) bool {
	aMajor, aMinor, aOK := parseComputeCapability(a)
	bMajor, bMinor, bOK := parseComputeCapability(b)
	if !aOK || !bOK {
		return a < b
	}
	if aMajor != bMajor {
		return aMajor < bMajor
	}
	return aMinor < bMinor
}

// parseComputeCapability parses a "major.minor" or bare "major" version string
// into integer components. ok is false if the string does not parse cleanly, so
// the caller can fall back to a lexicographic compare instead of guessing.
func parseComputeCapability(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 2)
	m, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	if len(parts) == 1 {
		return m, 0, true
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return m, n, true
}
