package monitor

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/internal/platform"
)

// SystemCollector gathers host-level resource metrics.
type SystemCollector interface {
	// Collect returns the current system resource usage.
	Collect() SystemResources
}

// platformChecker abstracts platform detection for testing.
type platformChecker interface {
	isLinux() bool
}

// defaultPlatformChecker uses the actual platform package.
type defaultPlatformChecker struct{}

func (d defaultPlatformChecker) isLinux() bool {
	return platform.IsLinux()
}

// DefaultSystemCollector reads system metrics from /proc on Linux
// and falls back to Go runtime metrics on other platforms.
type DefaultSystemCollector struct {
	// mu serialises the CPU-delta read-modify-write of prevIdle/prevTotal
	// so concurrent Collect() callers do not race (CT-HARDEN-MON-1).
	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	platform  platformChecker
}

// NewDefaultSystemCollector creates a DefaultSystemCollector. On
// Linux it primes the CPU counters with an initial sample.
func NewDefaultSystemCollector() *DefaultSystemCollector {
	c := &DefaultSystemCollector{
		platform: defaultPlatformChecker{},
	}
	if c.platform.isLinux() {
		idle, total := readCPUSample()
		c.prevIdle = idle
		c.prevTotal = total
		// Allow a small window so the next Collect has a delta.
		time.Sleep(50 * time.Millisecond)
	}
	return c
}

// Collect returns the current system resource usage.
func (c *DefaultSystemCollector) Collect() SystemResources {
	var res SystemResources

	// Use the platform checker (allows testing of non-Linux paths)
	checker := c.platform
	if checker == nil {
		checker = defaultPlatformChecker{}
	}

	if checker.isLinux() {
		res.CPUPercent = c.collectCPULinux()
		c.collectMemoryLinux(&res)
		c.collectDiskLinux(&res)
	} else {
		// Fallback: use Go runtime for memory.
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		res.MemoryTotal = m.Sys
		res.MemoryUsed = m.Alloc
		if res.MemoryTotal > 0 {
			res.MemoryPercent = float64(res.MemoryUsed) /
				float64(res.MemoryTotal) * 100
		}
	}

	return res
}

// collectCPULinux reads /proc/stat and computes CPU usage since the
// previous sample.
func (c *DefaultSystemCollector) collectCPULinux() float64 {
	return c.collectCPULinuxFromFile("/proc/stat")
}

// collectCPULinuxFromFile reads the CPU sample from the given path and computes
// CPU usage since the previous sample. Separated for testability (mirrors
// collectMemoryLinuxFromFile) so the counter-guard is exercisable with an
// injected fixture (CT-HARDEN-MON-HARD MON-1).
func (c *DefaultSystemCollector) collectCPULinuxFromFile(path string) float64 {
	// Hold mu across the whole read-sample → delta → update-prev sequence so
	// it is atomic against concurrent Collect() callers (CT-HARDEN-MON-1). The
	// /proc/stat read is a small local file, so serialising it here does not
	// stall an unrelated hot path.
	c.mu.Lock()
	defer c.mu.Unlock()
	idle, total, ok := readCPUSampleOKFromFile(path)
	// Guard against (a) the read-error / malformed-line sentinel (!ok) and
	// (b) a counter that did not advance or ran BACKWARDS (total <= prevTotal
	// or idle < prevIdle). Without this, a (0,0) sentinel with a large primed
	// prevTotal underflows the uint64 delta (float64(0-prevTotal) ≈ 1.8e19),
	// yielding an out-of-[0,100] CPU%, AND clobbers prev to 0 so the NEXT good
	// sample computes a second bogus since-boot delta. On any guard hit return
	// 0 WITHOUT clobbering prev, preserving the last-good baseline
	// (CT-HARDEN-MON-HARD MON-1).
	if !ok || total <= c.prevTotal || idle < c.prevIdle {
		return 0
	}
	idleDelta := float64(idle - c.prevIdle)
	totalDelta := float64(total - c.prevTotal)
	// idle is one of the summands of total, so on monotonic counters
	// idleDelta <= totalDelta ALWAYS. The MON-1 guard above rejects the
	// read-error sentinel, total-not-advancing, and idle-running-backwards, but
	// NOT a PARTIAL rollback where the non-idle sub-counters roll back while
	// idle and the net total still advance (CPU hotplug / cgroup / namespace
	// view change) — there idleDelta > totalDelta, and (1 - idleDelta/totalDelta)
	// goes NEGATIVE, yielding an out-of-[0,100] CPU%. Reject that sample here,
	// returning 0 WITHOUT clobbering prev so the last-good baseline is preserved
	// (same philosophy as the MON-1 guard) (CT-HARDEN-MON2HARD MON2-1).
	if idleDelta > totalDelta {
		return 0
	}
	c.prevIdle = idle
	c.prevTotal = total
	return (1.0 - idleDelta/totalDelta) * 100
}

// readCPUSample parses the first cpu line from /proc/stat and
// returns the idle ticks and total ticks.
func readCPUSample() (idle, total uint64) {
	return readCPUSampleFromFile("/proc/stat")
}

// readCPUSampleFromFile reads a CPU sample from the specified file path,
// dropping the ok flag. Preserved for existing callers/tests that only need
// the (idle, total) pair; here the sentinel (0, 0) is indistinguishable from a
// genuine reading — callers needing that distinction MUST use
// readCPUSampleOKFromFile. Separated for testability.
func readCPUSampleFromFile(path string) (idle, total uint64) {
	idle, total, _ = readCPUSampleOKFromFile(path)
	return idle, total
}

// readCPUSampleOKFromFile reads a CPU sample from the specified file path and
// returns ok=false when the file cannot be opened OR the cpu line is
// malformed/short (len(fields) < 5). This lets collectCPULinuxFromFile
// distinguish a real (0-tick) reading from the read-error sentinel and refuse
// to poison prevIdle/prevTotal (CT-HARDEN-MON-HARD MON-1). Separated for
// testability.
func readCPUSampleOKFromFile(path string) (idle, total uint64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		var vals [10]uint64
		for i := 1; i < len(fields) && i <= 10; i++ {
			v, _ := strconv.ParseUint(fields[i], 10, 64)
			vals[i-1] = v
			total += v
		}
		// idle is the 4th value (index 3).
		idle = vals[3]
		return idle, total, true
	}
	return 0, 0, false
}

// collectMemoryLinux reads /proc/meminfo for total and available
// memory.
func (c *DefaultSystemCollector) collectMemoryLinux(
	res *SystemResources,
) {
	c.collectMemoryLinuxFromFile(res, "/proc/meminfo")
}

// collectMemoryLinuxFromFile reads memory stats from the specified file.
// Separated for testability.
func (c *DefaultSystemCollector) collectMemoryLinuxFromFile(
	res *SystemResources,
	path string,
) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var memTotal, memAvailable, memFree, buffers, cached uint64
	var sawAvailable, sawFree bool
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = parseMemInfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvailable = parseMemInfoKB(line)
			sawAvailable = true
		case strings.HasPrefix(line, "MemFree:"):
			memFree = parseMemInfoKB(line)
			sawFree = true
		case strings.HasPrefix(line, "Buffers:"):
			buffers = parseMemInfoKB(line)
		case strings.HasPrefix(line, "Cached:"):
			cached = parseMemInfoKB(line)
		}
	}

	res.MemoryTotal = memTotal * 1024 // convert KB to bytes

	// Determine available memory WITHOUT ever assuming 0-available. A meminfo
	// with MemTotal but NO MemAvailable line (pre-3.14 kernels / some container
	// views) previously left memAvailable=0, so the `memAvailable <= memTotal`
	// guard passed and reported MemoryUsed=memTotal / MemoryPercent=100 on an
	// idle host. Prefer MemAvailable; else approximate with
	// MemFree+Buffers+Cached (the classic pre-3.14 formula); else leave
	// MemoryUsed/MemoryPercent at 0 (genuinely unknown), never a false 100%
	// (CT-HARDEN-MON-HARD MON-2).
	var available uint64
	switch {
	case sawAvailable:
		available = memAvailable
	case sawFree:
		available = memFree + buffers + cached
	default:
		return // available unknown — leave MemoryUsed/MemoryPercent at 0
	}

	if memTotal > 0 && available <= memTotal {
		res.MemoryUsed = (memTotal - available) * 1024
		res.MemoryPercent = float64(memTotal-available) /
			float64(memTotal) * 100
	}
}

// parseMemInfoKB extracts the numeric kB value from a /proc/meminfo
// line such as "MemTotal:       16384000 kB".
func parseMemInfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// collectDiskLinux reads disk usage for the root filesystem using
// syscall.Statfs. Falls back gracefully on failure.
func (c *DefaultSystemCollector) collectDiskLinux(
	res *SystemResources,
) {
	c.collectDiskLinuxFromPath(res, "/")
}

// collectDiskLinuxFromPath reads disk stats from the specified path.
// Separated for testability.
// Implementation is in system_disk_linux.go (via syscall.Statfs) and
// system_disk_other.go (no-op stub for non-Linux platforms).
