// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/metricsdb"
)

const watermarkPrefix = "watermark:"

// PutRollups stores rollups and watermarks in one transaction.
func (m *MetricsStore) PutRollups(ctx context.Context, rollups []store.Rollup, watermarks map[string]int64) error {
	return m.write(ctx, "store rollups", func(q *metricsdb.Queries) error {
		for _, r := range rollups {
			err := q.UpsertRollup(ctx, metricsdb.UpsertRollupParams{
				TenantID: r.TenantID, MonitorID: r.MonitorID, NodeID: r.NodeID, BucketMs: r.BucketMs, StartMs: r.StartMs,
				UpMs: r.UpMs, DownMs: r.DownMs, MaintMs: r.MaintMs, UnknownMs: r.UnknownMs,
				LatencyCount: r.LatencyCount, LatencyAvg: r.LatencyAvg, LatencyP95: r.LatencyP95,
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
			LatencyCount: r.LatencyCount, LatencyAvg: r.LatencyAvg, LatencyP95: r.LatencyP95,
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
			LatencyCount: r.LatencyCount, LatencyAvg: r.LatencyAvg, LatencyP95: r.LatencyP95,
		})
	}
	return out, nil
}

// DeleteRollupsBefore deletes one batch of rollups of a bucket size.
func (m *MetricsStore) DeleteRollupsBefore(ctx context.Context, bucketMs, beforeMs int64, batchSize int) (int64, error) {
	if batchSize <= 0 {
		return 0, errors.New("delete rollups: batch size must be positive")
	}
	n, err := metricsdb.New(m.db.writer).DeleteRollupsBefore(ctx, metricsdb.DeleteRollupsBeforeParams{
		BucketMs: bucketMs, BeforeMs: beforeMs, BatchSize: int64(batchSize),
	})
	if err != nil {
		return 0, fmt.Errorf("delete rollups: %w", mapErr(err))
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
	return m.write(ctx, "compact heartbeats", func(q *metricsdb.Queries) error {
		for _, l := range minutes {
			err := q.InsertLatencyMinute(ctx, metricsdb.InsertLatencyMinuteParams{
				TenantID: l.TenantID, MonitorID: l.MonitorID, NodeID: l.NodeID, MinuteMs: l.MinuteMs,
				MinMs: int64(l.Min), MaxMs: int64(l.Max), AvgMs: l.Avg, P95Ms: l.P95,
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

// DeleteLatencyMinutesBefore deletes one batch of latency minutes.
func (m *MetricsStore) DeleteLatencyMinutesBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error) {
	if batchSize <= 0 {
		return 0, errors.New("delete latency minutes: batch size must be positive")
	}
	n, err := metricsdb.New(m.db.writer).DeleteLatencyMinutesBefore(ctx, metricsdb.DeleteLatencyMinutesBeforeParams{
		BeforeMs: beforeMs, BatchSize: int64(batchSize),
	})
	if err != nil {
		return 0, fmt.Errorf("delete latency minutes: %w", mapErr(err))
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
