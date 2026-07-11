package ctop

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
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
		remoteProcs, failedHosts, err := c.collectRemote(ctx)
		if err != nil {
			errors++
		} else {
			processes = append(processes, remoteProcs...)
		}
		// CT2-5: a per-host collection failure (host unreachable, SSH
		// executor unavailable, remote command failure) must be visible in
		// CollectorStats.Errors — silently dropping it made a total remote
		// wipeout indistinguishable from a healthy zero-remote-hosts run.
		errors += failedHosts
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
		stats, statsErr := c.getContainerStats(ctx, rt, containers[i].ID)
		if statsErr != nil || stats == nil {
			// CT3-7 (§11.4.108): a failed/empty stats collection leaves
			// CPUPercent/MemoryUsage at the Go zero-value, which renders
			// identically to a genuinely-idle 0% container unless flagged —
			// StatsUnavailable is the distinct signal the renderer needs to
			// show "N/A" instead of a confirmed "0.0%".
			containers[i].StatsUnavailable = true
			continue
		}
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

	return containers, nil
}

// collectRemote fans out container collection to every registered remote
// host in parallel. It returns the aggregated processes AND the count of
// hosts that failed (CT2-5) — a failed host is no longer silently dropped;
// its failure is counted so the caller can surface it via
// CollectorStats.Errors instead of a total remote wipeout looking identical
// to a healthy zero-remote-hosts run.
func (c *Collector) collectRemote(ctx context.Context) ([]ContainerProcess, int, error) {
	if c.hostManager == nil {
		return nil, 0, nil
	}

	hosts := c.hostManager.ListHosts()
	var allProcesses []ContainerProcess
	var failed int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range hosts {
		wg.Add(1)
		go func(host remote.RemoteHost) {
			defer wg.Done()

			processes, err := c.collectFromHost(ctx, host)
			if err != nil {
				log.Printf("ctop: collectRemote: host %s: %v", host.Name, err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}

			mu.Lock()
			allProcesses = append(allProcesses, processes...)
			mu.Unlock()
		}(hosts[i])
	}

	wg.Wait()
	return allProcesses, failed, nil
}

func (c *Collector) collectFromHost(ctx context.Context, host remote.RemoteHost) ([]ContainerProcess, error) {
	if c.sshExecutor == nil {
		return nil, fmt.Errorf("SSH executor not configured")
	}

	rt := host.Runtime
	if rt == "" {
		rt = "podman"
	}

	cmd := buildRemoteListCommand(rt)
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

		// CT3-9: containers[i].ID is dynamic data parsed from the PRIOR
		// `ps -a --format json` command's output (see parseContainerList),
		// not static configuration. buildRemoteStatsCommand refuses to
		// interpolate anything outside the safe container-ID charset into a
		// string that is about to be handed to sshExecutor.Execute — which
		// runs it through a REMOTE SHELL with no argv-style escaping,
		// unlike the LOCAL path (getContainerStats above), which passes the
		// id as a discrete exec.CommandContext argv element (no shell
		// involved at all).
		statsCmd, cmdErr := buildRemoteStatsCommand(rt, containers[i].ID)
		if cmdErr != nil {
			log.Printf("ctop: collectFromHost: host %s: %v", host.Name, cmdErr)
			containers[i].StatsUnavailable = true
			continue
		}

		statsResult, err := c.sshExecutor.Execute(ctx, host, statsCmd)
		if err != nil {
			// CT3-7: distinguish a failed remote stats call from a
			// genuinely-idle 0% container (see collectLocal above).
			containers[i].StatsUnavailable = true
			continue
		}
		stats := parseContainerStats([]byte(statsResult.Stdout))
		if stats == nil {
			containers[i].StatsUnavailable = true
			continue
		}
		containers[i].CPUPercent = stats.CPUPercent
		containers[i].MemoryUsage = stats.MemoryUsage
		containers[i].MemoryLimit = stats.MemoryLimit
		containers[i].MemoryPercent = stats.MemoryPercent
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

// containerIDSafePattern is the safe charset for docker/podman container IDs
// (hex digests and short IDs alike): letters, digits, underscore, dot, and
// hyphen. Anything outside this set is refused by buildRemoteStatsCommand
// rather than interpolated into a remote-shell command string (CT3-9).
var containerIDSafePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// shellQuote wraps s in single quotes so the REMOTE login shell (which
// sshExecutor.Execute hands the command string to, unlike the argv-based LOCAL
// path that passes rt as a discrete exec.CommandContext argv[0]) treats every
// byte of s literally. rt = host.Runtime is an unvalidated
// CONTAINERS_REMOTE_HOST_N_RUNTIME env-config value; a value like
// "docker;evilcmd" would inject a second remote command (CT3-ARGSWEEP,
// §11.4.108). Single-quoting is the canonical POSIX neutralisation ('\” splice
// for an embedded quote); it is functionally non-breaking because the remote
// shell strips the quotes, so 'docker'/'podman' resolve identically.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildRemoteListCommand builds the `<runtime> ps -a --format json` command
// handed to sshExecutor.Execute (remote shell, NO argv escaping). rt is
// unvalidated host-config and MUST be shell quoted — leaving it raw was the
// CT3-ARGSWEEP §11.4.108 injection sibling of buildRemoteStatsCommand's
// (already-validated) id. Pure function so the built string is unit testable
// without a live executor (§11.4.115).
func buildRemoteListCommand(rt string) string {
	return fmt.Sprintf("%s ps -a --format json", shellQuote(rt))
}

// buildRemoteStatsCommand builds the `<runtime> stats --no-stream --format
// json <id>` command line later handed to sshExecutor.Execute, which runs it
// through the remote user's shell with NO argv-style escaping (CT3-9). The
// id originates from a PRIOR command's parsed JSON output — dynamic data,
// not static configuration — so it MUST be validated against the safe
// container-ID charset before interpolation; an id outside that charset is
// refused rather than silently escaped, since a rejected stats call is
// vastly preferable to a shell-injection-shaped string ever reaching a
// remote shell. rt (host.Runtime, unvalidated env config) is shell quoted for
// the same reason (CT3-ARGSWEEP, §11.4.108) — the CT3-9 fix validated the id
// but left rt raw.
func buildRemoteStatsCommand(rt, id string) (string, error) {
	if !containerIDSafePattern.MatchString(id) {
		return "", fmt.Errorf("refusing to build remote stats command: unsafe container id %q", id)
	}
	return fmt.Sprintf("%s stats --no-stream --format json %s", shellQuote(rt), id), nil
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
	if d < 0 {
		// CT3-11: clock skew (StartedAt slightly ahead of local wall-clock)
		// otherwise yields a negative duration and renders a nonsensical
		// "-5m" uptime; clamp to zero ("just started") instead.
		d = 0
	}
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
