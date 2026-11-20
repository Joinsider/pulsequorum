// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package aggregator

import (
	"slices"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
)

// hbKey identifies a heartbeat (FA-REP-01).
type hbKey struct {
	nodeID      string
	incarnation string
	seq         uint64
}

// watch is the leader's evaluation state of one monitor (ADR 0021). All times
// are Unix milliseconds. Slot k is the check round that starts at start(k) =
// phase + k*interval, the same grid as scheduler.NextRun.
type watch struct {
	m          store.Monitor
	known      bool  // configured at least once
	intervalMs int64 // 0 if the monitor is not scheduled
	phaseMs    int64
	deadlineMs int64 // offset of the evaluation deadline from start(k)

	// ready is set when the tracker reflects the replicated status and the
	// heartbeat buffer holds the heartbeats of the open slots. It is false on
	// followers, after a leader change, after a revision change and after a
	// superseded status change; the next evaluation then rebuilds it.
	ready bool
	t     *tracker
	// next is the first slot not yet evaluated.
	next int64
	// hbs holds the heartbeats of this monitor needed for the open slots,
	// all nodes; the reporters of a slot are selected per evaluation.
	hbs map[hbKey]store.Heartbeat
}

// configure applies the monitor configuration. A changed revision or interval
// drops the evaluation state, which is rebuilt from the stored heartbeats.
func (w *watch) configure(m store.Monitor, unit, reportDelay time.Duration) {
	if !w.known || w.m.Revision != m.Revision || w.m.IntervalSeconds != m.IntervalSeconds {
		w.drop()
	}
	w.known = true
	w.m = m

	interval := time.Duration(m.IntervalSeconds) * unit
	timeout := time.Duration(m.TimeoutSeconds) * unit
	if m.TimeoutSeconds <= 0 {
		timeout = interval * 8 / 10 // as the scheduler does
	}
	w.intervalMs = interval.Milliseconds()
	w.phaseMs = scheduler.Phase(m.ID, interval).Milliseconds()
	// FA-CON-05 deadline: timeout plus the report delay, at most one interval.
	w.deadlineMs = min((timeout + reportDelay).Milliseconds(), w.intervalMs)
}

// enabled reports whether the monitor is evaluated at all.
func (w *watch) enabled() bool {
	return w.known && !w.m.Paused && w.intervalMs > 0
}

// drop forgets the evaluation state.
func (w *watch) drop() {
	w.ready = false
	w.t = nil
	w.hbs = nil
}

// lookback is the number of slots that rebuilding looks back (ADR 0021): what
// the retry and recovery rules can need, plus a margin.
func (w *watch) lookback() int64 {
	return int64(max(w.m.Retries+1, w.m.RecoveryThreshold) + 2)
}

// start returns the start of slot k in Unix milliseconds.
func (w *watch) start(k int64) int64 {
	return w.phaseMs + k*w.intervalMs
}

// slotOf returns the slot in which ms lies.
func (w *watch) slotOf(ms int64) int64 {
	return floorDiv(ms-w.phaseMs, w.intervalMs)
}

// due reports whether slot k is evaluated at nowMs: when every responsible
// node has delivered a heartbeat for k, or at the deadline (ADR 0021).
func (w *watch) due(k, nowMs int64, responsible []string) bool {
	if nowMs >= w.start(k)+w.deadlineMs {
		return true
	}
	return w.allReported(k, responsible)
}

// allReported reports whether every responsible node has a heartbeat within
// slot k. Without responsible nodes the deadline alone decides.
func (w *watch) allReported(k int64, responsible []string) bool {
	if len(responsible) == 0 {
		return false
	}
	lo, hi := w.start(k), w.start(k+1)
	seen := make(map[string]bool, len(responsible))
	for _, hb := range w.hbs {
		if hb.TsMs >= lo && hb.TsMs < hi && countable(hb.Status) {
			seen[hb.NodeID] = true
		}
	}
	for _, id := range responsible {
		if !seen[id] {
			return false
		}
	}
	return true
}

// reporters returns the reports of slot k (FA-CON-05): per responsible node
// its newest heartbeat of slot k or k-1, start(k-1) <= ts < start(k+1). At the
// deadline of slot k a heartbeat of slot k-2 is older than two intervals and
// counts as missing (FA-CON-05, ADR 0021). The result is sorted by node ID.
func (w *watch) reporters(k int64, responsible []string) []store.Heartbeat {
	lo, hi := w.start(k-1), w.start(k+1)
	newest := make(map[string]store.Heartbeat, len(responsible))
	for _, hb := range w.hbs {
		if hb.TsMs < lo || hb.TsMs >= hi || !countable(hb.Status) || !slices.Contains(responsible, hb.NodeID) {
			continue
		}
		if cur, ok := newest[hb.NodeID]; !ok || newerThan(hb, cur) {
			newest[hb.NodeID] = hb
		}
	}
	out := make([]store.Heartbeat, 0, len(newest))
	for _, id := range responsible {
		if hb, ok := newest[id]; ok {
			out = append(out, hb)
		}
	}
	return out
}

// allReporters returns the newest heartbeat of slot k (k-1 as in reporters)
// of every node that reported, responsible or not, sorted by node ID. It feeds
// the deviation statistics (ADR 0022).
func (w *watch) allReporters(k int64) []store.Heartbeat {
	lo, hi := w.start(k-1), w.start(k+1)
	newest := map[string]store.Heartbeat{}
	for _, hb := range w.hbs {
		if hb.TsMs < lo || hb.TsMs >= hi || !countable(hb.Status) {
			continue
		}
		if cur, ok := newest[hb.NodeID]; !ok || newerThan(hb, cur) {
			newest[hb.NodeID] = hb
		}
	}
	out := make([]store.Heartbeat, 0, len(newest))
	for _, hb := range newest {
		out = append(out, hb)
	}
	slices.SortFunc(out, func(a, b store.Heartbeat) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out
}

// add buffers a heartbeat unless it is too old for any open slot.
func (w *watch) add(hb store.Heartbeat) {
	if hb.TsMs <= w.start(w.next)-2*w.intervalMs {
		return
	}
	w.hbs[hbKey{hb.NodeID, hb.Incarnation, hb.Seq}] = hb
}

// prune drops heartbeats that no open slot can use any more.
func (w *watch) prune() {
	limit := w.start(w.next) - 2*w.intervalMs
	for key, hb := range w.hbs {
		if hb.TsMs <= limit {
			delete(w.hbs, key)
		}
	}
}

// countable reports whether a heartbeat carries a check result.
func countable(s store.CheckStatus) bool {
	return s == store.CheckUp || s == store.CheckDown || s == store.CheckDegraded
}

// newerThan orders heartbeats of one node by time, then sequence.
func newerThan(a, b store.Heartbeat) bool {
	if a.TsMs != b.TsMs {
		return a.TsMs > b.TsMs
	}
	return a.Seq > b.Seq
}

// floorDiv divides rounding towards negative infinity; b must be positive.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
