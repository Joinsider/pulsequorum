// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package uptime

import (
	"context"
	"fmt"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// Range is a reporting period of the API.
type Range string

// Supported ranges. 24h and 7d use hourly buckets, 30d and 365d daily buckets;
// the last bucket is the current, partial one.
const (
	Range24h  Range = "24h"
	Range7d   Range = "7d"
	Range30d  Range = "30d"
	Range365d Range = "365d"
)

func (r Range) layout() (bucket int64, n int, err error) {
	switch r {
	case Range24h:
		return hourMs, 24, nil
	case Range7d:
		return hourMs, 7 * 24, nil
	case Range30d:
		return dayMs, 30, nil
	case Range365d:
		return dayMs, 365, nil
	}
	return 0, 0, fmt.Errorf("unknown range %q", r)
}

// Stats is the uptime of a monitor over a range.
type Stats struct {
	Range   Range
	NodeID  string // "" = consensus
	Total   store.Rollup
	Buckets []store.Rollup
}

// Percent returns up / (up + down) × 100, or false if nothing is known.
func Percent(r store.Rollup) (float64, bool) {
	known := r.UpMs + r.DownMs
	if known == 0 {
		return 0, false
	}
	return 100 * float64(r.UpMs) / float64(known), true
}

// Service answers uptime queries from rollups and computes the buckets that
// are not rolled up yet from the raw data.
type Service struct {
	State   store.State
	Metrics store.Metrics
}

// Stats returns the uptime of m over r, for one node or the consensus
// (nodeID ""). Without latency, consensus buckets are computed from the
// status history alone, which keeps monitor lists fast (NFA-PERF-03).
func (s *Service) Stats(ctx context.Context, m store.Monitor, nodeID string, r Range, now time.Time, latency bool) (Stats, error) {
	bucket, n, err := r.layout()
	if err != nil {
		return Stats{}, err
	}
	q := query{svc: s, m: m, nodeID: nodeID, now: now.UnixMilli(), latency: latency}
	if q.hourWM, err = s.Metrics.Watermark(ctx, wmHour); err != nil {
		return Stats{}, err
	}
	last := floorTo(q.now, bucket)
	first := last - int64(n-1)*bucket

	var buckets []store.Rollup
	if bucket == hourMs {
		buckets, err = q.hours(ctx, first, last+hourMs)
	} else {
		buckets, err = q.days(ctx, first, last+dayMs)
	}
	if err != nil {
		return Stats{}, err
	}
	total := merge(buckets, store.Rollup{TenantID: m.TenantID, MonitorID: m.ID, NodeID: nodeID, BucketMs: bucket, StartMs: first})
	return Stats{Range: r, NodeID: nodeID, Total: total, Buckets: buckets}, nil
}

type query struct {
	svc     *Service
	m       store.Monitor
	nodeID  string
	now     int64
	hourWM  int64
	latency bool
}

func (q *query) empty(bucket, start, end int64) store.Rollup {
	return store.Rollup{TenantID: q.m.TenantID, MonitorID: q.m.ID, NodeID: q.nodeID, BucketMs: bucket, StartMs: start, UnknownMs: end - start}
}

func (q *query) stored(ctx context.Context, bucket, from, to int64) (map[int64]store.Rollup, error) {
	rows, err := q.svc.Metrics.ListRollups(ctx, q.m.TenantID, store.RollupQuery{
		MonitorID: q.m.ID, NodeID: q.nodeID, BucketMs: bucket, FromMs: from, ToMs: to,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.Rollup, len(rows))
	for _, r := range rows {
		out[r.StartMs] = r
	}
	return out, nil
}

// hours returns the hourly buckets of [from, to) up to now. Rolled-up hours
// come from rollups; later hours are computed while their raw data exists.
func (q *query) hours(ctx context.Context, from, to int64) ([]store.Rollup, error) {
	stored, err := q.stored(ctx, hourMs, from, to)
	if err != nil {
		return nil, err
	}
	return q.assemble(from, to, stored, func(start, end int64) (store.Rollup, error) {
		return q.live(ctx, start, end)
	})
}

// hourKind says how an hour of a monitor is filled.
type hourKind int

const (
	hourStored hourKind = iota // rolled up: the stored rollup
	hourEmpty                  // unknown: rolled up without data, or before the monitor or the raw data
	hourLive                   // rolled up later: computed from the raw data
)

// kindOf decides how the hour [h, end) is filled; stored tells whether a
// rollup of the hour exists.
func (q *query) kindOf(h, end int64, stored bool) hourKind {
	rawFrom := q.now - CompactAfter.Milliseconds()
	switch {
	case stored && h+hourMs <= q.hourWM:
		return hourStored
	case h+hourMs <= q.hourWM || h < rawFrom || q.m.CreatedMs >= end:
		return hourEmpty
	}
	return hourLive
}

// eachHour calls fn for every hour of [from, to) that starts before now, with
// the end of the hour cut at now.
func (q *query) eachHour(from, to int64, fn func(h, end int64) error) error {
	for h := from; h < to && h < q.now; h += hourMs {
		if err := fn(h, min(h+hourMs, q.now)); err != nil {
			return err
		}
	}
	return nil
}

// assemble returns the hourly buckets of [from, to) up to now: the stored
// rollups where they are final, empty buckets where there is no data, and
// the buckets that live computes from the raw data.
func (q *query) assemble(from, to int64, stored map[int64]store.Rollup, live func(start, end int64) (store.Rollup, error)) ([]store.Rollup, error) {
	var out []store.Rollup
	err := q.eachHour(from, to, func(h, end int64) error {
		r, ok := stored[h]
		switch q.kindOf(h, end, ok) {
		case hourStored:
			out = append(out, r)
		case hourEmpty:
			out = append(out, q.empty(hourMs, h, end))
		default:
			computed, err := live(h, end)
			if err != nil {
				return err
			}
			out = append(out, computed)
		}
		return nil
	})
	return out, err
}

func (q *query) live(ctx context.Context, start, end int64) (store.Rollup, error) {
	calc := Calculator{State: q.svc.State, Metrics: q.svc.Metrics}
	rs, err := calc.Compute(ctx, q.m, start, end, ComputeOptions{
		BucketMs: hourMs, NodeID: q.nodeID, ConsensusOnly: q.nodeID == "", Latency: q.latency,
	})
	if err != nil {
		return store.Rollup{}, err
	}
	for _, r := range rs {
		if r.NodeID == q.nodeID {
			return r, nil
		}
	}
	return q.empty(hourMs, start, end), nil
}

// days returns the daily buckets of [from, to) up to now. Days that are not
// rolled up yet are summed from their hours.
func (q *query) days(ctx context.Context, from, to int64) ([]store.Rollup, error) {
	stored, err := q.stored(ctx, dayMs, from, to)
	if err != nil {
		return nil, err
	}
	dayWM, err := q.svc.Metrics.Watermark(ctx, wmDay)
	if err != nil {
		return nil, err
	}
	var out []store.Rollup
	for d := from; d < to && d < q.now; d += dayMs {
		end := min(d+dayMs, q.now)
		if r, ok := stored[d]; ok {
			out = append(out, r)
			continue
		}
		if d+dayMs <= dayWM || q.m.CreatedMs >= end {
			out = append(out, q.empty(dayMs, d, end))
			continue
		}
		hours, err := q.hours(ctx, d, d+dayMs)
		if err != nil {
			return nil, err
		}
		day := store.Rollup{TenantID: q.m.TenantID, MonitorID: q.m.ID, NodeID: q.nodeID, BucketMs: dayMs, StartMs: d}
		out = append(out, finish(merge(hours, day), end))
	}
	return out, nil
}
