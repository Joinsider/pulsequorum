// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite/metricsdb"
)

const watermarkPrefix = "watermark:"

// microsOf converts a latency in milliseconds to the integer microseconds
// stored in metrics.db (ADR 0016). The rounding error is at most 0.0005 ms.
func microsOf(ms float64) int64 { return int64(math.Round(ms * 1000)) }

// millisOf converts stored microseconds back to milliseconds.
func millisOf(us int64) float64 { return float64(us) / 1000 }

// PutRollups stores rollups and watermarks in one transaction.
func (m *MetricsStore) PutRollups(ctx context.Context, rollups []store.Rollup, watermarks map[string]int64) error {
	return m.writeSeries(ctx, "store rollups", func(q *metricsdb.Queries, series *seriesResolver) error {
		for _, r := range rollups {
			seriesID, err := series.id(ctx, r.TenantID, r.MonitorID, r.NodeID)
			if err != nil {
				return err
			}
			err = q.UpsertRollup(ctx, metricsdb.UpsertRollupParams{
				SeriesID: seriesID, BucketMs: r.BucketMs, StartMs: r.StartMs,
				UpMs: r.UpMs, DownMs: r.DownMs, MaintMs: r.MaintMs, UnknownMs: r.UnknownMs,
				LatencyCount: r.LatencyCount, LatencyAvgUs: microsOf(r.LatencyAvg), LatencyP95Us: microsOf(r.LatencyP95),
			})
			if err != nil {
				return mapErr(err)
			}
		}
		for name, v := range watermarks {
			if err := q.UpsertMeta(ctx, metricsdb.UpsertMetaParams{Key: watermarkPrefix + name, Value: strconv.FormatInt(v, 10)}); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// ListRollups returns rollups ordered by node and start.
func (m *MetricsStore) ListRollups(ctx context.Context, tenantID string, q store.RollupQuery) ([]store.Rollup, error) {
	rows, err := m.q.ListRollups(ctx, metricsdb.ListRollupsParams{
		TenantID: tenantID, MonitorID: q.MonitorID, AllNodes: boolToInt(q.AllNodes), NodeID: q.NodeID,
		BucketMs: q.BucketMs, FromMs: q.FromMs, ToMs: q.ToMs,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Rollup, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.Rollup{
			TenantID: r.TenantID, MonitorID: r.MonitorID, NodeID: r.NodeID, BucketMs: r.BucketMs, StartMs: r.StartMs,
			UpMs: r.UpMs, DownMs: r.DownMs, MaintMs: r.MaintMs, UnknownMs: r.UnknownMs,
			LatencyCount: r.LatencyCount, LatencyAvg: millisOf(r.LatencyAvgUs), LatencyP95: millisOf(r.LatencyP95Us),
		})
	}
	return out, nil
}

// ListRollupsBatch returns the rollups of several monitors for one node, ordered
// by monitor and time.
func (m *MetricsStore) ListRollupsBatch(ctx context.Context, tenantID string, q store.RollupBatchQuery) ([]store.Rollup, error) {
	if len(q.MonitorIDs) == 0 {
		return nil, nil
	}
	rows, err := m.q.ListRollupsBatch(ctx, metricsdb.ListRollupsBatchParams{
		TenantID: tenantID, MonitorIds: q.MonitorIDs, NodeID: q.NodeID,
		BucketMs: q.BucketMs, FromMs: q.FromMs, ToMs: q.ToMs,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Rollup, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.Rollup{
			TenantID: r.TenantID, MonitorID: r.MonitorID, NodeID: r.NodeID, BucketMs: r.BucketMs, StartMs: r.StartMs,
			UpMs: r.UpMs, DownMs: r.DownMs, MaintMs: r.MaintMs, UnknownMs: r.UnknownMs,
			LatencyCount: r.LatencyCount, LatencyAvg: millisOf(r.LatencyAvgUs), LatencyP95: millisOf(r.LatencyP95Us),
		})
	}
	return out, nil
}

// DeleteRollupsBefore deletes one batch of rollups of a bucket size. The batch
// holds at most batchSize rows in one transaction (FA-UPT-06).
func (m *MetricsStore) DeleteRollupsBefore(ctx context.Context, bucketMs, beforeMs int64, batchSize int) (int64, error) {
	n, err := m.retainPerSeries(ctx, "delete rollups", batchSize,
		func(ctx context.Context, q *metricsdb.Queries, seriesID, limit int64) (int64, error) {
			return q.DeleteRollupsOfSeriesBefore(ctx, metricsdb.DeleteRollupsOfSeriesBeforeParams{
				SeriesID: seriesID, BucketMs: bucketMs, BeforeMs: beforeMs, RowLimit: limit,
			})
		})
	if err != nil {
		return 0, fmt.Errorf("delete rollups: %w", err)
	}
	return n, nil
}

// Watermark returns a stored watermark, 0 if unset.
func (m *MetricsStore) Watermark(ctx context.Context, name string) (int64, error) {
	v, err := m.q.GetMeta(ctx, watermarkPrefix+name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read watermark %s: %w", name, err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("watermark %s: %w", name, err)
	}
	return n, nil
}

// SetWatermark stores a watermark.
func (m *MetricsStore) SetWatermark(ctx context.Context, name string, value int64) error {
	return mapErr(metricsdb.New(m.db.writer).UpsertMeta(ctx, metricsdb.UpsertMetaParams{
		Key: watermarkPrefix + name, Value: strconv.FormatInt(value, 10),
	}))
}

// CompactHeartbeats stores latency minutes and deletes heartbeats atomically.
func (m *MetricsStore) CompactHeartbeats(ctx context.Context, minutes []store.LatencyMinute, deleted []store.HeartbeatKey) error {
	return m.writeSeries(ctx, "compact heartbeats", func(q *metricsdb.Queries, series *seriesResolver) error {
		for _, l := range minutes {
			seriesID, err := series.id(ctx, l.TenantID, l.MonitorID, l.NodeID)
			if err != nil {
				return err
			}
			err = q.InsertLatencyMinute(ctx, metricsdb.InsertLatencyMinuteParams{
				SeriesID: seriesID, MinuteMs: l.MinuteMs,
				MinMs: int64(l.Min), MaxMs: int64(l.Max), AvgUs: microsOf(l.Avg), P95Us: microsOf(l.P95),
				OkCount: int64(l.OKCount), ErrCount: int64(l.ErrCount),
			})
			if err != nil {
				return mapErr(err)
			}
		}
		for _, k := range deleted {
			seq, err := toInt64(k.Seq)
			if err != nil {
				return err
			}
			if err := q.DeleteHeartbeat(ctx, metricsdb.DeleteHeartbeatParams{NodeID: k.NodeID, Incarnation: k.Incarnation, Seq: seq}); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// DeleteLatencyMinutesBefore deletes one batch of latency minutes, at most
// batchSize rows in one transaction (FA-UPT-06).
func (m *MetricsStore) DeleteLatencyMinutesBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error) {
	n, err := m.retainPerSeries(ctx, "delete latency minutes", batchSize,
		func(ctx context.Context, q *metricsdb.Queries, seriesID, limit int64) (int64, error) {
			return q.DeleteMinutesOfSeriesBefore(ctx, metricsdb.DeleteMinutesOfSeriesBeforeParams{
				SeriesID: seriesID, BeforeMs: beforeMs, RowLimit: limit,
			})
		})
	if err != nil {
		return 0, fmt.Errorf("delete latency minutes: %w", err)
	}
	return n, nil
}

func (m *MetricsStore) write(ctx context.Context, what string, fn func(q *metricsdb.Queries) error) error {
	tx, err := m.db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(metricsdb.New(tx)); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", what, err)
	}
	return nil
}
