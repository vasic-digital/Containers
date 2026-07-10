package monitor

import (
	"time"

	"digital.vasic.containers/pkg/remote"
)

// ResourceSnapshot captures a point-in-time view of system and
// container resource usage.
type ResourceSnapshot struct {
	// Timestamp is when the snapshot was taken.
	Timestamp time.Time
	// System holds host-level resource metrics.
	System SystemResources
	// Containers holds per-container resource metrics keyed by
	// container name.
	Containers map[string]ContainerResources
}

// SystemResources holds host-level CPU, memory, and disk metrics.
type SystemResources struct {
	CPUPercent    float64
	MemoryPercent float64
	DiskPercent   float64
	MemoryTotal   uint64
	MemoryUsed    uint64
	DiskTotal     uint64
	DiskUsed      uint64
}

// ContainerResources holds resource usage for a single container.
type ContainerResources struct {
	Name          string
	CPUPercent    float64
	MemoryPercent float64
	MemoryUsage   uint64
	MemoryLimit   uint64
}

// ThresholdRule defines a condition on a metric that, when met,
// triggers the associated action.
type ThresholdRule struct {
	// Metric is the name of the metric to evaluate
	// (e.g., "system.cpu", "system.memory",
	// "container.<name>.memory").
	Metric string
	// Threshold is the numeric boundary.
	Threshold float64
	// Operator is the comparison operator: ">", ">=", "<", "<=".
	Operator string
	// Action is called when the threshold condition is met.
	Action func(snapshot *ResourceSnapshot)
}

// ClusterSnapshot aggregates resource snapshots from the local
// host and all remote hosts, providing a unified cluster view.
type ClusterSnapshot struct {
	// Timestamp is when the cluster snapshot was taken.
	Timestamp time.Time
	// Local is the local host resource snapshot.
	Local *ResourceSnapshot
	// RemoteHosts maps host names to their resource snapshots.
	RemoteHosts map[string]*remote.HostResources
	// TotalCPUCores is the aggregate CPU core count across all
	// hosts.
	TotalCPUCores int
	// TotalMemoryMB is the aggregate memory in MB across all
	// hosts.
	TotalMemoryMB uint64
	// TotalDiskMB is the aggregate disk space in MB across all
	// hosts.
	TotalDiskMB uint64
	// HostCount is the total number of hosts (local + remote).
	HostCount int
}

// NewClusterSnapshot creates a ClusterSnapshot with the given
// local snapshot and remote host resources.
func NewClusterSnapshot(
	local *ResourceSnapshot,
	remoteHosts map[string]*remote.HostResources,
) *ClusterSnapshot {
	cs := &ClusterSnapshot{
		Timestamp:   time.Now(),
		Local:       local,
		RemoteHosts: remoteHosts,
		HostCount:   1, // local host
	}
	if local != nil {
		cs.TotalMemoryMB = local.System.MemoryTotal / (1024 * 1024)
		cs.TotalDiskMB = local.System.DiskTotal / (1024 * 1024)
	}
	for _, hr := range remoteHosts {
		// Skip a nil host-resources value: the map values are pointers, so a
		// caller may legitimately map a host name to nil (a host with no
		// resource data). This mirrors the `if local != nil` guard above and
		// avoids a nil deref on hr's fields (CT-HARDEN-MON-2). A nil host
		// contributes nothing and is not counted toward HostCount.
		if hr == nil {
			continue
		}
		cs.HostCount++
		cs.TotalCPUCores += hr.CPUCores
		cs.TotalMemoryMB += hr.MemoryTotalMB
		cs.TotalDiskMB += hr.DiskTotalMB
	}
	return cs
}
