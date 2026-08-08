//go:build linux

package monitor

import "syscall"

// collectDiskLinuxFromPath reads disk stats from the specified path
// using syscall.Statfs. Separated for testability.
func (c *DefaultSystemCollector) collectDiskLinuxFromPath(
	res *SystemResources,
	path string,
) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		// SW2-1: the statfs probe FAILED — flag it so the leftover 0%
		// DiskPercent is not read as a genuinely-empty disk. Without this a
		// `system.disk > 90 → page` rule NEVER fires when statfs is broken while
		// the disk is actually full (resolveMetric returns ok=false instead).
		res.DiskError = true
		return
	}
	// Total and free in bytes using the filesystem block size.
	res.DiskTotal = stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	res.DiskUsed = res.DiskTotal - free
	if res.DiskTotal > 0 {
		res.DiskPercent = float64(res.DiskUsed) / float64(res.DiskTotal) * 100
	}
}
