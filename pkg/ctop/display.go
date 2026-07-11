package ctop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
	colorBold   = "\033[1m"
)

type Display struct {
	config     DisplayConfig
	collector  *Collector
	output     io.Writer
	width      int
	height     int
	mu         sync.Mutex
	running    bool
	cancel     context.CancelFunc
	sortBy     SortField
	sortOrder  SortOrder
	filterHost string
	filterName string
}

func NewDisplay(collector *Collector, config DisplayConfig) *Display {
	config.RefreshRate = sanitizeRefreshRate(config.RefreshRate)
	return &Display{
		collector: collector,
		config:    config,
		output:    os.Stdout,
		sortBy:    config.SortBy,
		sortOrder: config.SortOrder,
	}
}

func NewDisplayWithWriter(collector *Collector, config DisplayConfig, w io.Writer) *Display {
	config.RefreshRate = sanitizeRefreshRate(config.RefreshRate)
	return &Display{
		collector: collector,
		config:    config,
		output:    w,
		sortBy:    config.SortBy,
		sortOrder: config.SortOrder,
	}
}

// sanitizeRefreshRate clamps a configured refresh rate (milliseconds) to a
// usable positive value. CT2-4: `time.NewTicker` panics on a duration <= 0,
// so a zero-value (or negative) `DisplayConfig.RefreshRate` — e.g. from a
// partial `DisplayConfig{}` literal — must never reach `time.NewTicker`
// directly; fall back to `DefaultDisplayConfig()`'s rate instead.
func sanitizeRefreshRate(ms int) int {
	if ms <= 0 {
		return DefaultDisplayConfig().RefreshRate
	}
	return ms
}

func (d *Display) Run(ctx context.Context) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return fmt.Errorf("display already running")
	}
	d.running = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
		d.clearScreen()
		d.showCursor()
	}()

	ctx, cancel := context.WithCancel(ctx)
	// CT2-3: publish d.cancel under d.mu — Stop() reads it under d.mu, and an
	// unsynchronized write here is a data race against a concurrent Stop()
	// call in the canonical `go d.Run(ctx)` + `d.Stop()` pattern.
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
	defer cancel()

	// CT2-2: SIGWINCH (terminal resize) MUST NOT be treated as a quit signal.
	// It shares no channel with the quit signals below; `render()` already
	// calls `d.updateSize()` on every refresh tick, so a resize is picked up
	// at the next tick without any dedicated redraw plumbing.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	d.hideCursor()
	d.clearScreen()

	d.updateSize()

	refreshRate := sanitizeRefreshRate(d.config.RefreshRate)
	renderTimeout := boundedRenderTimeout(refreshRate)

	ticker := time.NewTicker(time.Duration(refreshRate) * time.Millisecond)
	defer ticker.Stop()

	d.renderBounded(ctx, renderTimeout)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sigChan:
			return nil
		case <-ticker.C:
			d.renderBounded(ctx, renderTimeout)
		}
	}
}

// renderBounded runs render() under a per-refresh timeout derived from the
// display's refresh rate (CT2-1). Without this bound, `render()` calls
// `collector.Collect(ctx)` synchronously, which can block on a wedged
// subprocess (podman/docker ps|stats); while blocked, Run()'s single
// goroutine is NOT in its `select`, so a SIGINT/SIGTERM sitting in the
// (unread) `sigChan` cannot be translated into a `cancel()` call and the
// whole TUI — including Ctrl+C — freezes. Bounding each refresh guarantees
// Run() returns to its `select` (and can react to `sigChan`/`ctx.Done()`)
// within `timeout` regardless of how long the underlying command wedges.
// Cancelling the parent ctx (via Stop() or an already-cancelled caller
// context) still unblocks it immediately, since renderCtx is a child of ctx.
func (d *Display) renderBounded(ctx context.Context, timeout time.Duration) {
	renderCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d.render(renderCtx)
}

// boundedRenderTimeout derives a per-refresh Collect timeout from the
// configured refresh rate, with a floor (a very fast refresh rate must not
// starve a legitimately-slow but healthy collector call) and a ceiling (a
// very slow refresh rate must not let a wedged call block the render loop —
// and hence Ctrl+C — for an unreasonable duration).
func boundedRenderTimeout(refreshRateMS int) time.Duration {
	const floor = 500 * time.Millisecond
	const ceiling = 10 * time.Second

	timeout := time.Duration(refreshRateMS) * time.Millisecond
	if timeout < floor {
		timeout = floor
	}
	if timeout > ceiling {
		timeout = ceiling
	}
	return timeout
}

func (d *Display) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Display) SetSortBy(field SortField) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sortBy = field
}

func (d *Display) SetSortOrder(order SortOrder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sortOrder = order
}

func (d *Display) SetFilterHost(host string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filterHost = host
}

func (d *Display) SetFilterName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filterName = name
}

func (d *Display) ToggleSortOrder() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sortOrder == SortAsc {
		d.sortOrder = SortDesc
	} else {
		d.sortOrder = SortAsc
	}
}

func (d *Display) render(ctx context.Context) {
	d.updateSize()

	list, err := d.collector.Collect(ctx)
	if err != nil {
		d.renderError(err)
		return
	}

	d.mu.Lock()
	sortBy := d.sortBy
	sortOrder := d.sortOrder
	filterHost := d.filterHost
	filterName := d.filterName
	showStopped := d.config.ShowStopped
	d.mu.Unlock()

	list.Sort(sortBy, sortOrder)
	list.Filter(filterHost, filterName, showStopped)

	d.clearScreen()
	d.moveCursor(1, 1)

	var sb strings.Builder

	sb.WriteString(d.renderHeader(list))
	sb.WriteString(d.renderColumnHeaders())

	maxRows := d.height - 6
	if maxRows < 1 {
		maxRows = 10
	}

	rowCount := 0
	for _, p := range list.Processes {
		if rowCount >= maxRows {
			break
		}
		sb.WriteString(d.renderRow(p))
		rowCount++
	}

	if list.Total == 0 {
		sb.WriteString(d.colorize(colorYellow, "  No containers found\n"))
	}

	sb.WriteString(d.renderFooter(list))

	fmt.Fprint(d.output, sb.String())
}

func (d *Display) renderHeader(list *ContainerProcessList) string {
	var sb strings.Builder

	now := time.Now().Format("15:04:05")

	title := " ctop - Container Top "
	if !d.config.NoColor {
		title = colorBold + colorCyan + title + colorReset
	}

	stats := d.collector.GetStats()

	sb.WriteString(fmt.Sprintf("%s %s\n", title, now))

	hostInfo := fmt.Sprintf("Containers: %d (%d running, %d stopped) | Hosts: %d",
		list.Total, list.Running, list.Stopped, stats.HostCount)

	sb.WriteString(d.colorize(colorWhite, "  "+hostInfo+"\n"))

	sb.WriteString("\n")

	return sb.String()
}

func (d *Display) renderColumnHeaders() string {
	var sb strings.Builder

	headers := []struct {
		name  string
		width int
	}{
		{"ID", 12},
		{"NAME", 20},
		{"HOST", 12},
		{"STATE", 8},
		{"CPU%", 7},
		{"MEM", 10},
		{"MEM%", 7},
		{"NET I/O", 12},
		{"BLOCK", 12},
		{"PIDS", 5},
	}

	sb.WriteString(d.colorize(colorBold, "  "))
	for _, h := range headers {
		sb.WriteString(d.colorize(colorBold, padRight(h.name, h.width)))
	}
	sb.WriteString("\n")

	sb.WriteString(d.colorize(colorBlue, "  "))
	for _, h := range headers {
		sb.WriteString(d.colorize(colorBlue, strings.Repeat("─", h.width)))
	}
	sb.WriteString("\n")

	return sb.String()
}

func (d *Display) renderRow(p ContainerProcess) string {
	var sb strings.Builder

	sb.WriteString("  ")

	stateColor := d.stateColor(p.State)

	sb.WriteString(d.colorize(stateColor, padRight(p.ID, 12)))
	sb.WriteString(d.colorize(stateColor, padRight(truncate(p.Name, 20), 20)))
	sb.WriteString(d.colorize(d.hostColor(p.Host), padRight(truncate(p.Host, 12), 12)))
	sb.WriteString(d.colorize(stateColor, padRight(p.State, 8)))

	if p.StatsUnavailable {
		// CT3-7 (§11.4.108): a failed/empty stats collection must be
		// visually distinct from a confirmed 0% reading — colorPurple/"N/A"
		// never appears in the genuine-reading cpuColor/memColor palette
		// (green/yellow/red), so it cannot be mistaken for a real value.
		sb.WriteString(d.colorize(colorPurple, padRight("N/A", 7)))
		sb.WriteString(d.colorize(colorPurple, padRight("N/A", 10)))
		sb.WriteString(d.colorize(colorPurple, padRight("N/A", 7)))
	} else {
		cpuColor := d.cpuColor(p.CPUPercent)
		sb.WriteString(d.colorize(cpuColor, padRight(fmt.Sprintf("%.1f%%", p.CPUPercent), 7)))
		sb.WriteString(d.colorize(d.memColor(p.MemoryPercent), padRight(formatBytes(p.MemoryUsage), 10)))
		sb.WriteString(d.colorize(d.memColor(p.MemoryPercent), padRight(fmt.Sprintf("%.1f%%", p.MemoryPercent), 7)))
	}
	sb.WriteString(d.colorize(colorWhite, padRight(formatNetIO(p.NetworkRx, p.NetworkTx), 12)))
	sb.WriteString(d.colorize(colorWhite, padRight(formatBlockIO(p.BlockRead, p.BlockWrite), 12)))
	sb.WriteString(d.colorize(colorWhite, padRight(fmt.Sprintf("%d", p.PIDs), 5)))

	sb.WriteString("\n")

	return sb.String()
}

func (d *Display) renderFooter(list *ContainerProcessList) string {
	var sb strings.Builder

	sb.WriteString("\n")

	stats := d.collector.GetStats()

	footer := fmt.Sprintf("  Sort: %s (%s) | Update: %v | Errors: %d",
		d.sortBy, d.sortOrder, stats.UpdateDuration.Round(time.Millisecond), stats.Errors)

	sb.WriteString(d.colorize(colorCyan, footer))

	sb.WriteString(d.colorize(colorWhite, " | "))
	sb.WriteString(d.colorize(colorYellow, "[q] Quit [c] CPU [m] Mem [n] Name [r] Reverse"))

	sb.WriteString("\n")

	return sb.String()
}

func (d *Display) renderError(err error) {
	d.clearScreen()
	d.moveCursor(1, 1)
	fmt.Fprintf(d.output, d.colorize(colorRed, "  Error: %v\n"), err)
}

func (d *Display) stateColor(state string) string {
	switch state {
	case "running":
		return colorGreen
	case "exited", "stopped":
		return colorRed
	case "paused":
		return colorYellow
	case "restarting":
		return colorCyan
	default:
		return colorWhite
	}
}

func (d *Display) hostColor(host string) string {
	switch {
	case host == "local":
		return colorGreen
	case strings.Contains(host, "remote"):
		return colorPurple
	default:
		return colorCyan
	}
}

func (d *Display) cpuColor(percent float64) string {
	switch {
	case percent > 80:
		return colorRed
	case percent > 50:
		return colorYellow
	default:
		return colorGreen
	}
}

func (d *Display) memColor(percent float64) string {
	switch {
	case percent > 80:
		return colorRed
	case percent > 50:
		return colorYellow
	default:
		return colorGreen
	}
}

func (d *Display) colorize(color, text string) string {
	if d.config.NoColor {
		return text
	}
	return color + text + colorReset
}

func (d *Display) clearScreen() {
	fmt.Fprint(d.output, "\033[2J")
}

func (d *Display) moveCursor(row, col int) {
	fmt.Fprintf(d.output, "\033[%d;%dH", row, col)
}

func (d *Display) hideCursor() {
	fmt.Fprint(d.output, "\033[?25l")
}

func (d *Display) showCursor() {
	fmt.Fprint(d.output, "\033[?25h")
}

func (d *Display) updateSize() {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width = 80
		height = 24
	}
	d.width = width
	d.height = height
}

// padRight pads (or byte-unsafely-truncates, pre-fix) s to width. CT3-10:
// slicing the raw string by BYTE offset (the old `s[:width]`) can cut a
// multi-byte UTF-8 rune in half for non-ASCII container/image names,
// corrupting the rendered cell. Operating on []rune instead makes every
// slice/count a rune count, so the cut always lands on a rune boundary and
// the padded width matches the actual number of displayed characters.
func padRight(s string, width int) string {
	runes := []rune(s)
	if len(runes) >= width {
		if width < 0 {
			width = 0
		}
		return string(runes[:width])
	}
	return s + strings.Repeat(" ", width-len(runes))
}

// truncate shortens s to at most maxLen displayed characters, appending "..."
// when it actually cuts content. CT3-10: the old `s[:maxLen-3]` byte slice
// could split a multi-byte UTF-8 rune (accented / CJK characters) at the
// truncation boundary, producing an invalid/corrupted rune in the output.
// Slicing []rune(s) instead guarantees every cut lands on a rune boundary.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		if maxLen < 0 {
			maxLen = 0
		}
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatNetIO(rx, tx uint64) string {
	return fmt.Sprintf("%s / %s", formatBytes(rx), formatBytes(tx))
}

func formatBlockIO(read, write uint64) string {
	return fmt.Sprintf("%s / %s", formatBytes(read), formatBytes(write))
}

func (d *Display) RenderSnapshot(ctx context.Context) (string, error) {
	list, err := d.collector.Collect(ctx)
	if err != nil {
		return "", err
	}

	d.mu.Lock()
	sortBy := d.sortBy
	sortOrder := d.sortOrder
	filterHost := d.filterHost
	filterName := d.filterName
	showStopped := d.config.ShowStopped
	d.mu.Unlock()

	list.Sort(sortBy, sortOrder)
	list.Filter(filterHost, filterName, showStopped)

	var sb strings.Builder

	sb.WriteString(d.renderHeader(list))
	sb.WriteString(d.renderColumnHeaders())

	for _, p := range list.Processes {
		sb.WriteString(d.renderRow(p))
	}

	sb.WriteString(d.renderFooter(list))

	return sb.String(), nil
}

func (d *Display) RenderJSON(ctx context.Context) (string, error) {
	list, err := d.collector.Collect(ctx)
	if err != nil {
		return "", err
	}

	d.mu.Lock()
	sortBy := d.sortBy
	sortOrder := d.sortOrder
	filterHost := d.filterHost
	filterName := d.filterName
	showStopped := d.config.ShowStopped
	d.mu.Unlock()

	list.Sort(sortBy, sortOrder)
	list.Filter(filterHost, filterName, showStopped)

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return "", err
	}

	return string(data), nil
}

type CompactDisplay struct {
	display *Display
}

func NewCompactDisplay(collector *Collector, config DisplayConfig) *CompactDisplay {
	config.Compact = true
	return &CompactDisplay{
		display: NewDisplay(collector, config),
	}
}

func (cd *CompactDisplay) Run(ctx context.Context) error {
	return cd.display.Run(ctx)
}

func (cd *CompactDisplay) Stop() {
	cd.display.Stop()
}

func (cd *CompactDisplay) RenderSnapshot(ctx context.Context) (string, error) {
	return cd.display.RenderSnapshot(ctx)
}
