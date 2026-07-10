package ctop

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"digital.vasic.containers/pkg/remote"
)

type Collector struct {
	runtime     string
	hostManager remote.HostManager
	sshExecutor *remote.SSHExecutor
	executor    CommandExecutor
	mu          sync.RWMutex
	lastUpdate  time.Time
	stats       CollectorStats
}

type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

type defaultCTOPExecutor struct{}

func (e *defaultCTOPExecutor) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), err
}

func NewCollector(runtime string, hostManager remote.HostManager) *Collector {
	return &Collector{
		runtime:     runtime,
		hostManager: hostManager,
		executor:    &defaultCTOPExecutor{},
	}
}

func NewCollectorWithExecutor(runtime string, hostManager remote.HostManager, exec CommandExecutor) *Collector {
	return &Collector{
		runtime:     runtime,
		hostManager: hostManager,
		executor:    exec,
	}
}

func NewCollectorWithSSH(runtime string, hostManager remote.HostManager, sshExecutor *remote.SSHExecutor) *Collector {
	return &Collector{
		runtime:     runtime,
		hostManager: hostManager,
		sshExecutor: sshExecutor,
		executor:    &defaultCTOPExecutor{},
	}
}

func (c *Collector) Collect(ctx context.Context) (*ContainerProcessList, error) {
	start := time.Now()

	var processes []ContainerProcess
	var errors int
	var localCount int

	local, err := c.collectLocal(ctx)
	if err != nil {
		errors++
	} else {
		processes = append(processes, local...)
		localCount = len(local)
	}

	if c.hostManager != nil {
		remoteProcs, err := c.collectRemote(ctx)
		if err != nil {
			errors++
		} else {
			processes = append(processes, remoteProcs...)
		}
	}

	running, stopped := 0, 0
	var totalCPU float64
	var totalMem uint64

	for _, p := range processes {
		if p.State == "running" {
			running++
		} else {
			stopped++
		}
		totalCPU += p.CPUPercent
		totalMem += p.MemoryUsage
	}

	c.mu.Lock()
	c.lastUpdate = time.Now()
	c.stats = CollectorStats{
		TotalContainers:  len(processes),
		LocalContainers:  localCount,
		RemoteContainers: len(processes) - localCount,
		HostCount:        c.countHosts(processes),
		LastUpdate:       c.lastUpdate,
		UpdateDuration:   time.Since(start),
		Errors:           errors,
	}
	c.mu.Unlock()

	return &ContainerProcessList{
		Processes:  processes,
		Total:      len(processes),
		Running:    running,
		Stopped:    stopped,
		UpdatedAt:  time.Now(),
		CPUSeconds: totalCPU,
		MemoryUsed: totalMem,
	}, nil
}

func (c *Collector) collectLocal(ctx context.Context) ([]ContainerProcess, error) {
	rt := c.runtime
	if rt == "" {
		rt = "podman"
	}

	out, err := c.executor.Execute(ctx, rt, "ps", "-a", "--format", "json")
	if err != nil {
		if rt == "podman" {
			out, err = c.executor.Execute(ctx, "docker", "ps", "-a", "--format", "json")
			if err != nil {
				return nil, fmt.Errorf("failed to list containers: %w", err)
			}
			rt = "docker"
		} else {
			return nil, fmt.Errorf("failed to list containers: %w", err)
		}
	}

	containers, err := parseContainerList(out, rt, "local")
	if err != nil {
		return nil, err
	}

	for i := range containers {
		stats, _ := c.getContainerStats(ctx, rt, containers[i].ID)
		if stats != nil {
			containers[i].CPUPercent = stats.CPUPercent
			containers[i].MemoryUsage = stats.MemoryUsage
			containers[i].MemoryLimit = stats.MemoryLimit
			containers[i].MemoryPercent = stats.MemoryPercent
			containers[i].NetworkRx = stats.NetworkRx
			containers[i].NetworkTx = stats.NetworkTx
			containers[i].BlockRead = stats.BlockRead
			containers[i].BlockWrite = stats.BlockWrite
			containers[i].PIDs = stats.PIDs
		}
	}

	return containers, nil
}

func (c *Collector) collectRemote(ctx context.Context) ([]ContainerProcess, error) {
	if c.hostManager == nil {
		return nil, nil
	}

	hosts := c.hostManager.ListHosts()
	var allProcesses []ContainerProcess
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range hosts {
		wg.Add(1)
		go func(host remote.RemoteHost) {
			defer wg.Done()

			processes, err := c.collectFromHost(ctx, host)
			if err != nil {
				return
			}

			mu.Lock()
			allProcesses = append(allProcesses, processes...)
			mu.Unlock()
		}(hosts[i])
	}

	wg.Wait()
	return allProcesses, nil
}

func (c *Collector) collectFromHost(ctx context.Context, host remote.RemoteHost) ([]ContainerProcess, error) {
	if c.sshExecutor == nil {
		return nil, fmt.Errorf("SSH executor not configured")
	}

	rt := host.Runtime
	if rt == "" {
		rt = "podman"
	}

	cmd := fmt.Sprintf("%s ps -a --format json", rt)
	result, err := c.sshExecutor.Execute(ctx, host, cmd)
	if err != nil {
		cmd = "docker ps -a --format json"
		result, err = c.sshExecutor.Execute(ctx, host, cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to list containers on %s: %w", host.Name, err)
		}
		rt = "docker"
	}

	containers, err := parseContainerList([]byte(result.Stdout), rt, "remote:"+host.Name)
	if err != nil {
		return nil, err
	}

	for i := range containers {
		containers[i].Host = host.Name
		containers[i].Location = "remote:" + host.Name

		statsCmd := fmt.Sprintf("%s stats --no-stream --format json %s", rt, containers[i].ID)
		statsResult, err := c.sshExecutor.Execute(ctx, host, statsCmd)
		if err == nil {
			stats := parseContainerStats([]byte(statsResult.Stdout))
			if stats != nil {
				containers[i].CPUPercent = stats.CPUPercent
				containers[i].MemoryUsage = stats.MemoryUsage
				containers[i].MemoryLimit = stats.MemoryLimit
				containers[i].MemoryPercent = stats.MemoryPercent
			}
		}
	}

	return containers, nil
}

func (c *Collector) getContainerStats(ctx context.Context, rt, id string) (*ContainerProcess, error) {
	out, err := c.executor.Execute(ctx, rt, "stats", "--no-stream", "--format", "json", id)
	if err != nil {
		return nil, err
	}
	return parseContainerStats(out), nil
}

func shortenID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func extractName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	name := names[0]
	name = strings.TrimPrefix(name, "/")
	return name
}

func parsePercent(s string) float64 {
	s = strings.TrimSuffix(s, "%")
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseMemoryBytes(s string) uint64 {
	parts := strings.Split(s, "/")
	if len(parts) < 1 {
		return 0
	}
	return parseSize(parts[0])
}

func parseMemoryLimit(s string) uint64 {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return 0
	}
	return parseSize(parts[1])
}

func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "GIB"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GIB")
	case strings.HasSuffix(s, "GB"):
		mult = 1000 * 1000 * 1000
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MIB"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(s, "MIB")
	case strings.HasSuffix(s, "MB"):
		mult = 1000 * 1000
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KIB"):
		mult = 1024
		s = strings.TrimSuffix(s, "KIB")
	case strings.HasSuffix(s, "KB"):
		mult = 1000
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return uint64(f * float64(mult))
}

func parseNetIO(s string, rx bool) uint64 {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return 0
	}
	if rx {
		return parseSize(parts[0])
	}
	return parseSize(parts[1])
}

func parseBlockIO(s string, read bool) uint64 {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return 0
	}
	if read {
		return parseSize(parts[0])
	}
	return parseSize(parts[1])
}

func parsePIDs(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func (c *Collector) countHosts(processes []ContainerProcess) int {
	hosts := make(map[string]bool)
	for _, p := range processes {
		hosts[p.Host] = true
	}
	return len(hosts)
}

func (c *Collector) GetStats() CollectorStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

func (list *ContainerProcessList) Sort(by SortField, order SortOrder) {
	sort.Slice(list.Processes, func(i, j int) bool {
		var less bool
		switch by {
		case SortByCPU:
			less = list.Processes[i].CPUPercent > list.Processes[j].CPUPercent
		case SortByMemory:
			less = list.Processes[i].MemoryUsage > list.Processes[j].MemoryUsage
		case SortByName:
			less = list.Processes[i].Name < list.Processes[j].Name
		case SortByState:
			less = list.Processes[i].State < list.Processes[j].State
		case SortByUptime:
			less = list.Processes[i].StartedAt.Before(list.Processes[j].StartedAt)
		case SortByRuntime:
			less = list.Processes[i].Runtime < list.Processes[j].Runtime
		case SortByHost:
			less = list.Processes[i].Host < list.Processes[j].Host
		default:
			less = list.Processes[i].CPUPercent > list.Processes[j].CPUPercent
		}

		if order == SortAsc {
			return !less
		}
		return less
	})
}

func (list *ContainerProcessList) Filter(host, name string, showStopped bool) {
	var filtered []ContainerProcess
	for _, p := range list.Processes {
		if !showStopped && p.State != "running" {
			continue
		}
		if host != "" && !strings.Contains(strings.ToLower(p.Host), strings.ToLower(host)) {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(name)) {
			continue
		}
		filtered = append(filtered, p)
	}
	list.Processes = filtered
	list.Total = len(filtered)
}
