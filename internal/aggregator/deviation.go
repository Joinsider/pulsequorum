// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"slices"
	"time"

	"github.com/joinsider/uptimecluster/internal/store"
)

// deviationWindow is how far back the deviation statistics reach (ADR 0022).
const deviationWindow = 5 * time.Minute

// DeviationStats counts the check slots in which the leader compared one node
// with its peers (ADR 0022).
type DeviationStats struct {
	// Slots is the number of slots in which the node reported and at least one
	// other node reported UP or DEGRADED for the same monitor.
	Slots int
	// Down is the number of those slots in which the node reported DOWN.
	Down int
}

// deviationKey identifies one node's slot of one monitor. A slot is evaluated
// more than once after a superseded status change, so the key keeps it counted once.
type deviationKey struct {
	tenantID, monitorID, nodeID string
	slot                        int64
}

// deviationEvent is one counted slot of one node.
type deviationEvent struct {
	startMs int64
	nodeID  string
	down    bool
}

// recordDeviation counts the slot k of w for every node that reported in it,
// whether or not the node is responsible any more: an excluded node keeps its
// statistics, so that its return can be seen (ADR 0022). It runs under a.mu.
func (a *Aggregator) recordDeviation(w *watch, k int64, reports []store.Heartbeat, startMs int64) {
	if len(reports) < 2 {
		return
	}
	for _, r := range reports {
		if !slices.ContainsFunc(reports, func(o store.Heartbeat) bool {
			return o.NodeID != r.NodeID && (o.Status == store.CheckUp || o.Status == store.CheckDegraded)
		}) {
			continue
		}
		key := deviationKey{w.m.TenantID, w.m.ID, r.NodeID, k}
		if _, ok := a.deviationSeen[key]; ok {
			continue
		}
		a.deviationSeen[key] = startMs
		a.deviations = append(a.deviations, deviationEvent{startMs: startMs, nodeID: r.NodeID, down: r.Status == store.CheckDown})
	}
	a.pruneDeviation(a.opts.Now().UnixMilli() - deviationWindow.Milliseconds())
}

// pruneDeviation forgets the slots that start before limitMs.
func (a *Aggregator) pruneDeviation(limitMs int64) {
	kept := a.deviations[:0]
	for _, ev := range a.deviations {
		if ev.startMs >= limitMs {
			kept = append(kept, ev)
		}
	}
	a.deviations = kept
	for key, startMs := range a.deviationSeen {
		if startMs < limitMs {
			delete(a.deviationSeen, key)
		}
	}
}

// Deviation returns the deviation statistics of every node for the slots that
// start at or after sinceMs (ADR 0022). Only the leader counts; a node that
// has no counted slot is absent from the map. sinceMs may reach back at most
// deviationWindow.
func (a *Aggregator) Deviation(sinceMs int64) map[string]DeviationStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]DeviationStats{}
	for _, ev := range a.deviations {
		if ev.startMs < sinceMs {
			continue
		}
		s := out[ev.nodeID]
		s.Slots++
		if ev.down {
			s.Down++
		}
		out[ev.nodeID] = s
	}
	return out
}
