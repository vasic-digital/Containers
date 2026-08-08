package scheduler

import (
	"fmt"
	"sort"
	"sync/atomic"

	"digital.vasic.containers/pkg/remote"
)

type hostScore struct {
	name  string
	score float64
}

// scheduleResourceAware places the container on the host with the
// highest resource score.
func scheduleResourceAware(
	scorer *ResourceScorer,
	snapshots map[string]*remote.HostResources,
	hosts []remote.RemoteHost,
	req ContainerRequirements,
	localName string,
) PlacementDecision {

	var candidates []hostScore

	// Score local host if a snapshot exists AND it satisfies the required
	// labels. The local host carries no labels, so a request that requires
	// any label must NOT land on it — the remote-host loop below already
	// gates on labelsMatch, and skipping the same gate here let a labelled
	// request (e.g. gpu=true) be placed on an unlabelled local host.
	if snap, ok := snapshots[localName]; ok && labelsMatch(nil, req.Labels) {
		if scorer.CanFit(snap, req) {
			score := scorer.Score(snap, req)

			candidates = append(candidates, hostScore{
				name:  localName,
				score: score,
			})
		}
	}

	// Score remote hosts.
	for _, h := range hosts {
		snap, ok := snapshots[h.Name]
		if !ok {
			continue
		}

		if !labelsMatch(h.Labels, req.Labels) {

			continue
		}
		if !scorer.CanFit(snap, req) {

			continue
		}
		score := scorer.Score(snap, req)

		candidates = append(candidates, hostScore{
			name:  h.Name,
			score: score,
		})
	}

	if len(candidates) == 0 {

		return PlacementDecision{
			Requirement: req,
			Score:       0,
			Reason:      "no host has sufficient resources",
		}
	}

	// Prefer local if requested and available.
	if req.PreferLocal {
		for _, c := range candidates {
			if c.name == localName {
				return PlacementDecision{
					Requirement: req,
					HostName:    c.name,
					Score:       c.score,
					Reason:      "preferred local placement",
				}
			}
		}
	}

	// Sort by score DESC, breaking exact-score ties by host name so the winner is
	// deterministic. candidates derive from HostManager.ListHosts(), which iterates
	// a map (random order per call), and sort.Slice is NOT stable, so without a
	// name tie-break two equally-scored hosts were selected in map-iteration order
	// — non-deterministic placement (§11.4.50, CT-HARDEN-SCHED-HARD SC2-3).
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		// Tie: keep the pre-existing local-first preference (local placement needs
		// no SSH), then order remaining hosts by name so the pick is deterministic.
		liLocal := candidates[i].name == localName
		if liLocal != (candidates[j].name == localName) {
			return liLocal
		}
		return candidates[i].name < candidates[j].name // SC2-3 deterministic tie-break
	})

	best := candidates[0]

	return PlacementDecision{
		Requirement: req,
		HostName:    best.name,
		Score:       best.score,
		Reason: fmt.Sprintf(
			"best resource score %.3f", best.score,
		),
	}
}

// scheduleRoundRobin distributes containers evenly across hosts. The
// round-robin counter is per-scheduler (passed in), not a package global —
// two independent schedulers must not share one rotation, which would
// desynchronise each one's fair distribution.
func scheduleRoundRobin(
	hosts []remote.RemoteHost,
	req ContainerRequirements,
	localName string,
	counter *atomic.Uint64,
) PlacementDecision {
	var allNames []string
	// The local host carries no labels; include it only for label-free
	// requests (labelsMatch(nil, req.Labels) is true iff req.Labels is empty).
	if labelsMatch(nil, req.Labels) {
		allNames = append(allNames, localName)
	}
	for _, h := range hosts {
		if labelsMatch(h.Labels, req.Labels) {
			allNames = append(allNames, h.Name)
		}
	}

	if len(allNames) == 0 {
		return PlacementDecision{
			Requirement: req,
			Score:       0,
			Reason:      "no eligible hosts",
		}
	}

	idx := counter.Add(1) - 1
	selected := allNames[idx%uint64(len(allNames))]

	return PlacementDecision{
		Requirement: req,
		HostName:    selected,
		Score:       1.0 / float64(len(allNames)),
		Reason: fmt.Sprintf(
			"round-robin index %d", idx,
		),
	}
}

// scheduleAffinity places containers only on hosts with matching
// labels.
func scheduleAffinity(
	scorer *ResourceScorer,
	snapshots map[string]*remote.HostResources,
	hosts []remote.RemoteHost,
	req ContainerRequirements,
) PlacementDecision {
	var best *hostScore
	for _, h := range hosts {
		if !labelsMatch(h.Labels, req.Labels) {
			continue
		}
		snap, ok := snapshots[h.Name]
		if !ok || !scorer.CanFit(snap, req) {
			continue
		}
		s := scorer.Score(snap, req)
		// Break exact-score ties by host name for deterministic selection: `hosts`
		// arrives in HostManager.ListHosts() map order (random per call), so a pure
		// `s > best.score` first-seen tie-break chose non-deterministically among
		// equally-scored affinity matches (§11.4.50, CT-HARDEN-SCHED-HARD SC2-4).
		if best == nil || s > best.score || (s == best.score && h.Name < best.name) {
			best = &hostScore{name: h.Name, score: s}
		}
	}

	if best == nil {
		return PlacementDecision{
			Requirement: req,
			Score:       0,
			Reason:      "no host matches affinity labels",
		}
	}

	return PlacementDecision{
		Requirement: req,
		HostName:    best.name,
		Score:       best.score,
		Reason:      "affinity label match",
	}
}

// scheduleSpread distributes to minimize per-host container count.
func scheduleSpread(
	snapshots map[string]*remote.HostResources,
	hosts []remote.RemoteHost,
	req ContainerRequirements,
	localName string,
	existing map[string]int,
) PlacementDecision {
	var allNames []string
	// Local host carries no labels: include only for label-free requests.
	if labelsMatch(nil, req.Labels) {
		allNames = append(allNames, localName)
	}
	for _, h := range hosts {
		if labelsMatch(h.Labels, req.Labels) {
			allNames = append(allNames, h.Name)
		}
	}

	if len(allNames) == 0 {
		return PlacementDecision{
			Requirement: req,
			Score:       0,
			Reason:      "no eligible hosts",
		}
	}

	// Pick host with fewest existing containers, breaking exact-count ties by host
	// name so the winner is deterministic. allNames arrives in
	// HostManager.ListHosts() map order (random per call) and sort.Slice is not
	// stable, so without the name tie-break two hosts with equal container counts
	// were chosen in map-iteration order (§11.4.50, CT-HARDEN-SCHED-HARD SC2-5).
	sort.Slice(allNames, func(i, j int) bool {
		if existing[allNames[i]] != existing[allNames[j]] {
			return existing[allNames[i]] < existing[allNames[j]]
		}
		// Tie: keep the pre-existing local-first preference, then order remaining
		// hosts by name so the pick is deterministic.
		liLocal := allNames[i] == localName
		if liLocal != (allNames[j] == localName) {
			return liLocal
		}
		return allNames[i] < allNames[j] // SC2-5 deterministic tie-break
	})

	selected := allNames[0]
	return PlacementDecision{
		Requirement: req,
		HostName:    selected,
		Score:       0.5,
		Reason: fmt.Sprintf(
			"spread: fewest containers (%d)",
			existing[selected],
		),
	}
}

// scheduleBinPack packs containers onto as few hosts as possible.
func scheduleBinPack(
	scorer *ResourceScorer,
	snapshots map[string]*remote.HostResources,
	hosts []remote.RemoteHost,
	req ContainerRequirements,
	localName string,
) PlacementDecision {
	type candidate struct {
		name string
		used float64
	}

	var candidates []candidate

	// Local host carries no labels: include only for label-free requests.
	if snap, ok := snapshots[localName]; ok && labelsMatch(nil, req.Labels) {
		if scorer.CanFit(snap, req) {
			candidates = append(candidates, candidate{
				name: localName,
				used: snap.CPUPercent + snap.MemoryPercent,
			})
		}
	}

	for _, h := range hosts {
		if !labelsMatch(h.Labels, req.Labels) {
			continue
		}
		snap, ok := snapshots[h.Name]
		if !ok || !scorer.CanFit(snap, req) {
			continue
		}
		candidates = append(candidates, candidate{
			name: h.Name,
			used: snap.CPUPercent + snap.MemoryPercent,
		})
	}

	if len(candidates) == 0 {
		return PlacementDecision{
			Requirement: req,
			Score:       0,
			Reason:      "no host can fit container",
		}
	}

	// Pick the most-used host that can still fit, breaking exact-utilization ties
	// by host name so the winner is deterministic. candidates arrive in
	// HostManager.ListHosts() map order (random per call) and sort.Slice is not
	// stable, so without the name tie-break two equally-utilized hosts were chosen
	// in map-iteration order (§11.4.50, CT-HARDEN-SCHED-HARD SC2-6).
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].used != candidates[j].used {
			return candidates[i].used > candidates[j].used
		}
		// Tie: keep the pre-existing local-first preference, then order remaining
		// hosts by name so the pick is deterministic.
		liLocal := candidates[i].name == localName
		if liLocal != (candidates[j].name == localName) {
			return liLocal
		}
		return candidates[i].name < candidates[j].name // SC2-6 deterministic tie-break
	})

	selected := candidates[0]
	return PlacementDecision{
		Requirement: req,
		HostName:    selected.name,
		Score:       0.5,
		Reason: fmt.Sprintf(
			"bin-pack: most utilized host (%.1f%%)",
			selected.used/2,
		),
	}
}

// labelsMatch returns true if the host has all required labels.
func labelsMatch(
	hostLabels, requiredLabels map[string]string,
) bool {
	for k, v := range requiredLabels {
		if hostLabels[k] != v {
			return false
		}
	}
	return true
}

// selectByStrategy dispatches to the appropriate strategy function
// given a flat map of candidate host snapshots. Returns (hostName, reason).
// Used by tests and by callers that already hold a snapshot map.
func selectByStrategy(
	strategy PlacementStrategy,
	candidates map[string]*remote.HostResources,
	req ContainerRequirements,
	scorer *ResourceScorer,
) (string, string) {
	switch strategy {
	case StrategyGPUAffinity:
		return pickGPUAffinity(candidates, req, scorer)
	default:
		return "", "selectByStrategy: unsupported strategy " + string(strategy)
	}
}

// pickGPUAffinity selects the highest-scoring GPU-bearing host.
func pickGPUAffinity(
	candidates map[string]*remote.HostResources,
	req ContainerRequirements,
	scorer *ResourceScorer,
) (string, string) {
	bestHost := ""
	// bestScore starts BELOW the valid [0,1] score range so the FIRST fitting GPU
	// host is selected even when its composite score clamps to exactly 0 — a
	// resource-saturated host with a free, matching GPU still FITS (CanFit already
	// proved it), so pre-fix `bestScore := 0.0` with a strict `>` silently dropped
	// it and reported "no host fits GPU requirement", a §11.4.108 false negative
	// that also bypassed scheduleOne's minSelectedScore fixup (CT-HARDEN-SCHED-HARD
	// SC2-1).
	bestScore := -1.0
	for name, res := range candidates {
		if !scorer.CanFit(res, req) || !res.HasGPU() {
			continue
		}
		sc := scorer.Score(res, req)
		// Break exact-score ties by host name so selection is deterministic:
		// `candidates` is a map, whose iteration order Go randomizes per range, so
		// a pure `sc > bestScore` picked whichever equally-scored host the map
		// yielded first — non-deterministic placement (§11.4.50, CT-HARDEN-SCHED-HARD
		// SC2-2).
		if sc > bestScore || (sc == bestScore && name < bestHost) {
			bestScore = sc
			bestHost = name
		}
	}
	if bestHost == "" {
		return "", "gpu_affinity: no host fits GPU requirement"
	}
	return bestHost, fmt.Sprintf(
		"gpu_affinity: selected %s with score %.3f", bestHost, bestScore)
}
