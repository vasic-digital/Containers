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
	// Hold mu across the whole read-sample → delta → update-prev sequence so
	// it is atomic against concurrent Collect() callers (CT-HARDEN-MON-1). The
	// /proc/stat read is a small local file, so serialising it here does not
	// stall an unrelated hot path.
	c.mu.Lock()
	defer c.mu.Unlock()
	idle, total := readCPUSample()
	if total == c.prevTotal {
		return 0
	}
	idleDelta := float64(idle - c.prevIdle)
	totalDelta := float64(total - c.prevTotal)
	c.prevIdle = idle
	c.prevTotal = total
	return (1.0 - idleDelta/totalDelta) * 100
}

// readCPUSample parses the first cpu line from /proc/stat and
// returns the idle ticks and total ticks.
func readCPUSample() (idle, total uint64) {
	return readCPUSampleFromFile("/proc/stat")
}

// readCPUSampleFromFile reads CPU sample from the specified file path.
// Separated for testability.
func readCPUSampleFromFile(path string) (idle, total uint64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
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
			return 0, 0
		}
		var vals [10]uint64
		for i := 1; i < len(fields) && i <= 10; i++ {
			v, _ := strconv.ParseUint(fields[i], 10, 64)
			vals[i-1] = v
			total += v
		}
		// idle is the 4th value (index 3).
		idle = vals[3]
		return idle, total
	}
	return 0, 0
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

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoKB(line)
		}
	}

	res.MemoryTotal = memTotal * 1024 // convert KB to bytes
	if memTotal > 0 && memAvailable <= memTotal {
		res.MemoryUsed = (memTotal - memAvailable) * 1024
		res.MemoryPercent = float64(memTotal-memAvailable) /
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
