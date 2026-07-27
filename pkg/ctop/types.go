package ctop

import (
	"time"
)

// ContainerProcess represents a container as a "process" for top/htop-style display
type ContainerProcess struct {
	// Identity
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Runtime  string `json:"runtime"`
	Host     string `json:"host"`
	Location string `json:"location"` // "local" or "remote:<hostname>"

	// State
	State     string    `json:"state"`
	Status    string    `json:"status"`
	Created   time.Time `json:"created"`
	StartedAt time.Time `json:"started_at"`
	Uptime    string    `json:"uptime"`

	// Resource Usage
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsage   uint64  `json:"memory_usage"`
	MemoryLimit   uint64  `json:"memory_limit"`
	MemoryPercent float64 `json:"memory_percent"`

	// StatsUnavailable is true when the runtime's stats command failed (or
	// returned nothing decodable) for this container (CT3-7, §11.4.108):
	// CPUPercent/MemoryUsage/MemoryPercent above are the Go zero-value in
	// this case — NOT a confirmed 0% reading — and renderers MUST show a
	// distinct "N/A"-class marker rather than treating this identically to
	// a genuinely-idle container.
	StatsUnavailable bool `json:"stats_unavailable"`

	// I/O
	NetworkRx  uint64 `json:"network_rx"`
	NetworkTx  uint64 `json:"network_tx"`
	BlockRead  uint64 `json:"block_read"`
	BlockWrite uint64 `json:"block_write"`

	// Process Count
	PIDs int `json:"pids"`

	// Labels
	Labels map[string]string `json:"labels"`

	// Ports
	Ports []string `json:"ports"`
}

// ContainerProcessList is a sorted list of container processes
type ContainerProcessList struct {
	Processes  []ContainerProcess `json:"processes"`
	Total      int                `json:"total"`
	Running    int                `json:"running"`
	Stopped    int                `json:"stopped"`
	UpdatedAt  time.Time          `json:"updated_at"`
	CPUSeconds float64            `json:"cpu_seconds"` // Total CPU seconds
	MemoryUsed uint64             `json:"memory_used"` // Total memory used
}

// SortField defines how to sort container processes
type SortField string

const (
	SortByCPU     SortField = "cpu"
	SortByMemory  SortField = "mem"
	SortByName    SortField = "name"
	SortByState   SortField = "state"
	SortByUptime  SortField = "uptime"
	SortByRuntime SortField = "runtime"
	SortByHost    SortField = "host"
)

// SortOrder defines sort direction
type SortOrder string

const (
	SortDesc SortOrder = "desc"
	SortAsc  SortOrder = "asc"
)

// DisplayConfig configures the display
type DisplayConfig struct {
	SortBy      SortField `json:"sort_by"`
	SortOrder   SortOrder `json:"sort_order"`
	ShowStopped bool      `json:"show_stopped"`
	FilterHost  string    `json:"filter_host"`
	FilterName  string    `json:"filter_name"`
	RefreshRate int       `json:"refresh_rate"` // milliseconds
	NoColor     bool      `json:"no_color"`
	Compact     bool      `json:"compact"`
}

// DefaultDisplayConfig returns default display configuration
func DefaultDisplayConfig() DisplayConfig {
	return DisplayConfig{
		SortBy:      SortByCPU,
		SortOrder:   SortDesc,
		ShowStopped: false,
		RefreshRate: 1000,
		NoColor:     false,
		Compact:     false,
	}
}

// SystemInfo holds system-wide resource information
type SystemInfo struct {
	Hostname       string    `json:"hostname"`
	OSType         string    `json:"os_type"`
	TotalCPU       int       `json:"total_cpu"`
	TotalMemory    uint64    `json:"total_memory"`
	UsedCPU        float64   `json:"used_cpu"`
	UsedMemory     uint64    `json:"used_memory"`
	TotalLoad      string    `json:"total_load"` // e.g., "0.5, 0.8, 1.2"
	Uptime         string    `json:"uptime"`
	ContainerCount int       `json:"container_count"`
	Timestamp      time.Time `json:"timestamp"`
}

// HostSummary summarizes containers per host
type HostSummary struct {
	Host           string `json:"host"`
	Location       string `json:"location"`
	Runtime        string `json:"runtime"`
	ContainerCount int    `json:"container_count"`
	RunningCount   int    `json:"running_count"`
	StoppedCount   int    `json:"stopped_count"`
	CPUUsed        string `json:"cpu_used"`
	MemoryUsed     string `json:"memory_used"`
	MemoryTotal    string `json:"memory_total"`
}

// CollectorStats holds collector statistics
type CollectorStats struct {
	TotalContainers  int           `json:"total_containers"`
	LocalContainers  int           `json:"local_containers"`
	RemoteContainers int           `json:"remote_containers"`
	HostCount        int           `json:"host_count"`
	LastUpdate       time.Time     `json:"last_update"`
	UpdateDuration   time.Duration `json:"update_duration"`
	Errors           int           `json:"errors"`
}
