package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"digital.vasic.containers/pkg/runtime"
)

// ResourceMonitor collects periodic resource snapshots and
// evaluates threshold rules.
type ResourceMonitor interface {
	// Start begins periodic snapshot collection at the given
	// interval. It blocks until ctx is cancelled or Stop is
	// called.
	Start(ctx context.Context, interval time.Duration) error

	// Stop halts periodic collection.
	Stop() error

	// Snapshot returns the most recently collected resource
	// snapshot.
	Snapshot() (*ResourceSnapshot, error)

	// SetThreshold registers a threshold rule for evaluation on
	// each snapshot.
	SetThreshold(rule ThresholdRule)
}

// DefaultMonitor is the standard ResourceMonitor implementation.
type DefaultMonitor struct {
	mu        sync.RWMutex
	rt        runtime.ContainerRuntime
	sys       SystemCollector
	threshold *ThresholdEvaluator
	latest    *ResourceSnapshot
	stopCh    chan struct{}
	stopped   bool
}

// NewDefaultMonitor creates a DefaultMonitor that uses the given
// container runtime for stats and the system collector for host
// metrics.
func NewDefaultMonitor(
	rt runtime.ContainerRuntime,
	sys SystemCollector,
) *DefaultMonitor {
	return &DefaultMonitor{
		rt:        rt,
		sys:       sys,
		threshold: NewThresholdEvaluator(),
		stopCh:    make(chan struct{}),
	}
}

// Start begins periodic snapshot collection. It returns when ctx
// is cancelled or Stop is called.
func (m *DefaultMonitor) Start(
	ctx context.Context,
	interval time.Duration,
) error {
	if interval <= 0 {
		return fmt.Errorf("monitor: interval must be positive")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Take an initial snapshot immediately.
	m.collect(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.stopCh:
			return nil
		case <-ticker.C:
			m.collect(ctx)
		}
	}
}

// Stop halts periodic collection.
func (m *DefaultMonitor) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopped {
		m.stopped = true
		close(m.stopCh)
	}
	return nil
}

// Snapshot returns the most recently collected snapshot.
func (m *DefaultMonitor) Snapshot() (*ResourceSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.latest == nil {
		return nil, fmt.Errorf("monitor: no snapshot available")
	}
	return m.latest, nil
}

// SetThreshold registers a threshold rule.
func (m *DefaultMonitor) SetThreshold(rule ThresholdRule) {
	m.threshold.AddRule(rule)
}

// collect gathers system and container metrics, stores the
// snapshot, and evaluates threshold rules.
func (m *DefaultMonitor) collect(ctx context.Context) {
	snap := &ResourceSnapshot{
		Timestamp:  time.Now(),
		Containers: make(map[string]ContainerResources),
	}

	// Collect system metrics.
	if m.sys != nil {
		snap.System = m.sys.Collect()
	}

	// Collect container stats. A List error or a per-container Stats error
	// must NOT silently read as "0 containers, all healthy" (§11.4.68/.69
	// error-as-healthy). The Stats-errored container is still dropped (existing
	// intended behavior, codified by TestDefaultMonitor_StatsError); the
	// hardening only SURFACES the failure on the snapshot so a consumer can
	// tell a genuinely empty host from a failed probe (CT-HARDEN-MON-HARD
	// MON-3).
	if m.rt != nil {
		containers, err := m.rt.List(ctx, runtime.ListFilter{
			All:    false,
			Status: []runtime.ContainerState{runtime.StateRunning},
		})
		if err != nil {
			snap.ListError = true
		} else {
			for _, c := range containers {
				stats, sErr := m.rt.Stats(ctx, c.ID)
				if sErr != nil {
					snap.StatsFailures++
					continue
				}
				snap.Containers[c.Name] = ContainerResources{
					Name:          c.Name,
					CPUPercent:    stats.CPUPercent,
					MemoryPercent: stats.MemoryPercent,
					MemoryUsage:   stats.MemoryUsage,
					MemoryLimit:   stats.MemoryLimit,
				}
			}
		}
	}

	m.mu.Lock()
	m.latest = snap
	m.mu.Unlock()

	// Evaluate thresholds.
	m.threshold.Evaluate(snap)
}
