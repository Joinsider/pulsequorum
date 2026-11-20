// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package uptime

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// ConsensusTotals returns the consensus total of range r for each monitor of
// the tenant. For every monitor the total equals Stats(ctx, m, "", r, now,
// false).Total: the same hour selection and the same calculation, but the
// rollups and the status history of all monitors are loaded with one query
// each (NFA-PERF-03). Only hourly ranges are supported, and every monitor must
// belong to tenantID.
func (s *Service) ConsensusTotals(ctx context.Context, tenantID string, ms []store.Monitor, r Range, now time.Time) (map[string]store.Rollup, error) {
	bucket, n, err := r.layout()
	if err != nil {
		return nil, err
	}
	if bucket != hourMs {
		return nil, fmt.Errorf("consensus totals: range %s is not hourly", r)
	}
	out := make(map[string]store.Rollup, len(ms))
	if len(ms) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.TenantID != tenantID {
			return nil, fmt.Errorf("consensus totals: monitor %s is not of tenant %s", m.ID, tenantID)
		}
		ids = append(ids, m.ID)
	}

	nowMs := now.UnixMilli()
	hourWM, err := s.Metrics.Watermark(ctx, wmHour)
	if err != nil {
		return nil, err
	}
	last := floorTo(nowMs, bucket)
	first := last - int64(n-1)*bucket
	end := last + hourMs

	rows, err := s.Metrics.ListRollupsBatch(ctx, tenantID, store.RollupBatchQuery{
		MonitorIDs: ids, NodeID: "", BucketMs: hourMs, FromMs: first, ToMs: end,
	})
	if err != nil {
		return nil, err
	}
	stored := make(map[string]map[int64]store.Rollup, len(ms))
	for _, row := range rows {
		if stored[row.MonitorID] == nil {
			stored[row.MonitorID] = map[int64]store.Rollup{}
		}
		stored[row.MonitorID][row.StartMs] = row
	}

	queries := make([]query, len(ms))
	for i, m := range ms {
		queries[i] = query{svc: s, m: m, nodeID: "", now: nowMs, hourWM: hourWM}
	}

	// First find the hours that must be computed from the status history, so
	// that the history is read only for monitors that need it and only for the
	// span that they need.
	var liveIDs []string
	liveFrom, liveTo := int64(math.MaxInt64), int64(math.MinInt64)
	for i := range queries {
		q := &queries[i]
		needs := false
		err := q.eachHour(first, end, func(h, e int64) error {
			_, ok := stored[q.m.ID][h]
			if q.kindOf(h, e, ok) != hourLive {
				return nil
			}
			if !needs {
				needs = true
				liveIDs = append(liveIDs, q.m.ID)
			}
			liveFrom = min(liveFrom, h)
			liveTo = max(liveTo, e)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var changes map[string][]store.StatusChange
	if len(liveIDs) > 0 {
		if changes, err = s.State.StatusHistoryBatch(ctx, tenantID, liveIDs, liveFrom, liveTo); err != nil {
			return nil, err
		}
	}

	for i := range queries {
		q := &queries[i]
		m := q.m
		buckets, err := q.assemble(first, end, stored[m.ID], func(start, e int64) (store.Rollup, error) {
			return consensusHour(m, statusWindow(changes[m.ID], start, e), start, e), nil
		})
		if err != nil {
			return nil, err
		}
		out[m.ID] = merge(buckets, store.Rollup{TenantID: m.TenantID, MonitorID: m.ID, NodeID: "", BucketMs: bucket, StartMs: first})
	}
	return out, nil
}

// consensusHour returns the consensus rollup of the hour [start, end) from the
// status changes of the hour, as Calculator.Compute does without latency.
func consensusHour(m store.Monitor, changes []store.StatusChange, start, end int64) store.Rollup {
	r := store.Rollup{TenantID: m.TenantID, MonitorID: m.ID, StartMs: start, BucketMs: hourMs}
	r.UpMs, r.DownMs, r.MaintMs = consensusTimes(changes, start, end)
	return finish(r, end)
}

// statusWindow returns what StatusHistory(from, to) returns, taken from a
// longer, time-ordered history of the monitor: the last change at or before
// from, then the changes before to.
func statusWindow(changes []store.StatusChange, from, to int64) []store.StatusChange {
	lo := 0
	for i, c := range changes {
		if c.SinceMs > from {
			break
		}
		lo = i
	}
	hi := lo
	for hi < len(changes) && changes[hi].SinceMs < to {
		hi++
	}
	return changes[lo:hi]
}
