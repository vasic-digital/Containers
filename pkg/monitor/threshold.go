package monitor

import (
	"strings"
	"sync"
)

// ThresholdEvaluator checks resource snapshots against registered
// threshold rules and fires actions when conditions are met.
type ThresholdEvaluator struct {
	mu    sync.RWMutex
	rules []ThresholdRule
}

// NewThresholdEvaluator creates an empty ThresholdEvaluator.
func NewThresholdEvaluator() *ThresholdEvaluator {
	return &ThresholdEvaluator{}
}

// AddRule registers a threshold rule.
func (e *ThresholdEvaluator) AddRule(rule ThresholdRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, rule)
}

// Rules returns a copy of the registered rules.
func (e *ThresholdEvaluator) Rules() []ThresholdRule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]ThresholdRule, len(e.rules))
	copy(out, e.rules)
	return out
}

// Evaluate checks each rule against the snapshot and fires the
// rule's action when the condition is satisfied.
func (e *ThresholdEvaluator) Evaluate(snap *ResourceSnapshot) {
	if snap == nil {
		return
	}

	e.mu.RLock()
	rules := make([]ThresholdRule, len(e.rules))
	copy(rules, e.rules)
	e.mu.RUnlock()

	for _, rule := range rules {
		value, ok := resolveMetric(snap, rule.Metric)
		if !ok {
			continue
		}
		if compare(value, rule.Operator, rule.Threshold) {
			if rule.Action != nil {
				rule.Action(snap)
			}
		}
	}
}

// resolveMetric extracts the numeric value for a named metric from
// the snapshot. Supported metrics:
//
//	"system.cpu", "system.memory", "system.disk"
//	"container.<name>.cpu", "container.<name>.memory"
func resolveMetric(
	snap *ResourceSnapshot,
	metric string,
) (float64, bool) {
	switch {
	case metric == "system.cpu":
		// SW2-1: a broken CPU probe (CPUError) cannot be RESOLVED — return
		// ok=false so Evaluate's `if !ok { continue }` skips the rule instead of
		// comparing a genuine-looking 0 that silently never fires.
		if snap.System.CPUError {
			return 0, false
		}
		return snap.System.CPUPercent, true
	case metric == "system.memory":
		// SW2-1: broken memory probe — unresolvable, see system.cpu above.
		if snap.System.MemoryError {
			return 0, false
		}
		return snap.System.MemoryPercent, true
	case metric == "system.disk":
		// SW2-1: broken disk probe — unresolvable. This is the load-bearing
		// case: `system.disk > 90 → page` must NOT silently pass on a 0 when
		// statfs failed while the disk is actually full.
		if snap.System.DiskError {
			return 0, false
		}
		return snap.System.DiskPercent, true
	case strings.HasPrefix(metric, "container."):
		return resolveContainerMetric(snap, metric)
	default:
		return 0, false
	}
}

// resolveContainerMetric parses "container.<name>.<field>" and
// returns the value.
func resolveContainerMetric(
	snap *ResourceSnapshot,
	metric string,
) (float64, bool) {
	// Expected format: "container.<name>.<field>"
	parts := strings.SplitN(metric, ".", 3)
	if len(parts) < 3 {
		return 0, false
	}
	name := parts[1]
	field := parts[2]

	c, ok := snap.Containers[name]
	if !ok {
		return 0, false
	}

	switch field {
	case "cpu":
		return c.CPUPercent, true
	case "memory":
		return c.MemoryPercent, true
	default:
		return 0, false
	}
}

// compare evaluates "value op threshold".
func compare(value float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	default:
		return false
	}
}
