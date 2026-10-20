// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package uptime computes uptime and latency from heartbeats and the consensus
// status history, maintains the hourly and daily rollups, compacts old raw
// data and enforces retention (FA-UPT-01..06).
//
// Uptime % = up / (total − maintenance − unknown) × 100. DEGRADED and PARTIAL
// count as up (FA-UPT-01). All computations are pure functions of the stored
// data, so every voter derives the same rollups (FA-UPT-03).
package uptime

import (
	"math"
	"slices"

	"github.com/joinsider/uptimecluster/internal/store"
)

// coverMs is how long a heartbeat counts: until the next heartbeat of the same
// node, at most two intervals. Older heartbeats count as missing (FA-CON-05),
// so the time after them is unknown.
func coverMs(m store.Monitor) int64 {
	return 2 * int64(max(m.IntervalSeconds, 1)) * 1000
}

// overlap returns the length of [a, b) ∩ [start, end).
func overlap(a, b, start, end int64) int64 {
	return max(0, min(b, end)-max(a, start))
}

// span is a half-open interval [from, to) in milliseconds.
type span struct{ from, to int64 }

// maintenanceSpans returns the MAINTENANCE periods of the consensus status
// history, clipped to [start, end). A MAINTENANCE change lasts until the next
// change, the last one until end. changes must be ordered by time.
func maintenanceSpans(changes []store.StatusChange, start, end int64) []span {
	var out []span
	for i, c := range changes {
		if c.Status != store.StatusMaintenance {
			continue
		}
		until := end
		if i+1 < len(changes) {
			until = changes[i+1].SinceMs
		}
		from, to := max(c.SinceMs, start), min(until, end)
		if from < to {
			out = append(out, span{from, to})
		}
	}
	return out
}

// spanOverlap returns the length of [a, b) that lies inside the spans. The
// spans must not overlap each other.
func spanOverlap(spans []span, a, b int64) int64 {
	var sum int64
	for _, s := range spans {
		sum += overlap(s.from, s.to, a, b)
	}
	return sum
}

// nodeUpDown attributes [start, end) from one node's heartbeats, ordered by
// time (FA-UPT-02: one stream, one clock). Heartbeats before start may be
// passed; they count until the next heartbeat or until they expire.
//
// Time inside a maintenance span (FA-UPT-01) is maintenance for the node, not
// up or down, so a node that reports DOWN during maintenance is not penalised.
// The choice for time without heartbeat coverage: a maintenance span counts as
// maintenance for the node from its first heartbeat in the input on, even
// where the node sent nothing, because the whole node was in maintenance. Time
// before the node's first heartbeat stays unknown, since the node was not yet
// part of the monitor then.
func nodeUpDown(hbs []store.Heartbeat, maint []span, start, end, cover int64) (up, down, mt int64) {
	if len(hbs) == 0 {
		return 0, 0, 0
	}
	mt = spanOverlap(maint, max(start, hbs[0].TsMs), end)
	for i, hb := range hbs {
		until := hb.TsMs + cover
		if i+1 < len(hbs) {
			until = min(until, hbs[i+1].TsMs)
		}
		d := overlap(hb.TsMs, until, start, end)
		// Maintenance inside this coverage is already part of mt.
		d -= spanOverlap(maint, max(hb.TsMs, start), min(until, end))
		if hb.Status == store.CheckDown {
			down += d
		} else {
			up += d
		}
	}
	return up, down, mt
}

type kind int

const (
	kindUnknown kind = iota
	kindUp
	kindDown
	kindMaint
)

// classify maps a consensus status to the uptime category. PENDING is a
// retry phase: it counts like the status before it, unknown if that is not up.
func classify(s store.MonitorStatus, prev kind) kind {
	switch s {
	case store.StatusUp, store.StatusDegraded, store.StatusPartial:
		return kindUp
	case store.StatusDown:
		return kindDown
	case store.StatusMaintenance:
		return kindMaint
	case store.StatusPending:
		if prev == kindUp {
			return kindUp
		}
	}
	return kindUnknown
}

// consensusTimes attributes [start, end) from the status history ordered by
// time. A status lasts until the next change; time before the first known
// change is unknown.
func consensusTimes(changes []store.StatusChange, start, end int64) (up, down, maint int64) {
	prev := kindUnknown
	for i, c := range changes {
		k := classify(c.Status, prev)
		until := end
		if i+1 < len(changes) {
			until = changes[i+1].SinceMs
		}
		d := overlap(c.SinceMs, until, start, end)
		switch k {
		case kindUp:
			up += d
		case kindDown:
			down += d
		case kindMaint:
			maint += d
		}
		prev = k
	}
	return up, down, maint
}

// latencyStats are computed over successful checks.
type latencyStats struct {
	count int64
	avg   float64
	p95   float64
}

func latencyOf(values []int) latencyStats {
	if len(values) == 0 {
		return latencyStats{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	var sum int64
	for _, v := range sorted {
		sum += int64(v)
	}
	return latencyStats{
		count: int64(len(sorted)),
		avg:   float64(sum) / float64(len(sorted)),
		p95:   float64(percentile(sorted, 0.95)),
	}
}

// percentile uses the nearest-rank method on sorted values.
func percentile(sorted []int, p float64) int {
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(idx, 0), len(sorted)-1)]
}

// okLatencies returns the latencies of successful heartbeats in [start, end).
func okLatencies(hbs []store.Heartbeat, start, end int64) []int {
	var out []int
	for _, hb := range hbs {
		if hb.TsMs >= start && hb.TsMs < end && hb.Status != store.CheckDown {
			out = append(out, hb.LatencyMs)
		}
	}
	return out
}

// merge sums durations and combines latency; finish may recompute the
// unknown time afterwards. The combined p95 is the
// count-weighted mean of the parts' p95 values, an approximation: exact
// percentiles cannot be combined (documented for daily rollups and ranges).
func merge(parts []store.Rollup, into store.Rollup) store.Rollup {
	var latSum, p95Sum float64
	for _, p := range parts {
		into.UpMs += p.UpMs
		into.DownMs += p.DownMs
		into.MaintMs += p.MaintMs
		into.UnknownMs += p.UnknownMs
		into.LatencyCount += p.LatencyCount
		latSum += p.LatencyAvg * float64(p.LatencyCount)
		p95Sum += p.LatencyP95 * float64(p.LatencyCount)
	}
	if into.LatencyCount > 0 {
		into.LatencyAvg = latSum / float64(into.LatencyCount)
		into.LatencyP95 = p95Sum / float64(into.LatencyCount)
	}
	return into
}
