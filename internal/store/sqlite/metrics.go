// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/metricsdb"
)

//go:embed migrations/metrics/*.sql
var metricsMigrationFiles embed.FS

// MetricsFile is the file name of the data class B database inside the data directory.
const MetricsFile = "metrics.db"

const (
	metaIncarnation       = "incarnation"
	defaultHeartbeatLimit = 1000
	maxHeartbeatLimit     = 5000
	// DefaultMetricsCacheKiB is the writer page cache of metrics.db (ADR 0013 C5).
	DefaultMetricsCacheKiB = 64 << 10
)

// MetricsOptions tune metrics.db.
type MetricsOptions struct {
	// CacheSizeKiB of the writer; 0 = DefaultMetricsCacheKiB.
	CacheSizeKiB int
}

// MetricsStore implements store.Metrics on metrics.db.
type MetricsStore struct {
	db          *DB
	q           *metricsdb.Queries // bound to the read pool
	incarnation string
	series      seriesCache
}

var _ store.Metrics = (*MetricsStore)(nil)

// OpenMetrics opens or creates dataDir/metrics.db. A new file gets a new random
// incarnation, even if the node ID is unchanged (ADR 0012 B4, PF-12).
func OpenMetrics(ctx context.Context, dataDir string, opts MetricsOptions) (*MetricsStore, error) {
	migrations, err := fs.Sub(metricsMigrationFiles, "migrations/metrics")
	if err != nil {
		return nil, err
	}
	if opts.CacheSizeKiB == 0 {
		opts.CacheSizeKiB = DefaultMetricsCacheKiB
	}
	db, err := Open(ctx, Options{
		Path:         filepath.Join(dataDir, MetricsFile),
		Synchronous:  SyncNormal,
		ReadConns:    4, // small pool leaves gaps for checkpoints (ADR 0013 C4)
		CacheSizeKiB: opts.CacheSizeKiB,
		Migrations:   migrations,
	})
	if err != nil {
		return nil, err
	}
	m := &MetricsStore{db: db, q: metricsdb.New(db.reader)}
	if m.incarnation, err = m.loadIncarnation(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return m, nil
}

func (m *MetricsStore) loadIncarnation(ctx context.Context) (string, error) {
	w := metricsdb.New(m.db.writer)
	value, err := w.GetMeta(ctx, metaIncarnation)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read incarnation: %w", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate incarnation: %w", err)
	}
	value = hex.EncodeToString(b[:])
	if err := w.InsertMeta(ctx, metricsdb.InsertMetaParams{Key: metaIncarnation, Value: value}); err != nil {
		return "", fmt.Errorf("store incarnation: %w", err)
	}
	return value, nil
}

// DB exposes the underlying database for maintenance (checkpoints, vacuum).
func (m *MetricsStore) DB() *DB { return m.db }

// Close closes the database.
func (m *MetricsStore) Close() error { return m.db.Close() }

// Incarnation returns the incarnation of this metrics database.
func (m *MetricsStore) Incarnation() string { return m.incarnation }

// InsertHeartbeats stores heartbeats in one transaction; duplicates are ignored.
func (m *MetricsStore) InsertHeartbeats(ctx context.Context, hbs []store.Heartbeat) (int, error) {
	if len(hbs) == 0 {
		return 0, nil
	}
	tx, err := m.db.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin heartbeat insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := metricsdb.New(tx)
	inserted := 0
	for _, hb := range hbs {
		seq, err := toInt64(hb.Seq)
		if err != nil {
			return 0, err
		}
		details := string(hb.Details)
		if details == "" {
			details = "{}"
		}
		n, err := q.InsertHeartbeat(ctx, metricsdb.InsertHeartbeatParams{
			NodeID:      hb.NodeID,
			Incarnation: hb.Incarnation,
			Seq:         seq,
			TenantID:    hb.TenantID,
			MonitorID:   hb.MonitorID,
			TsMs:        hb.TsMs,
			Status:      int64(hb.Status),
			LatencyMs:   int64(hb.LatencyMs),
			ErrorClass:  hb.ErrorClass,
			Message:     hb.Message,
			DetailsJson: details,
		})
		if err != nil {
			return 0, fmt.Errorf("insert heartbeat %s/%s/%d: %w", hb.NodeID, hb.Incarnation, hb.Seq, mapErr(err))
		}
		inserted += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit heartbeat insert: %w", err)
	}
	return inserted, nil
}

// LastSeq returns the highest stored seq of the stream, or 0.
func (m *MetricsStore) LastSeq(ctx context.Context, nodeID, incarnation string) (uint64, error) {
	seq, err := m.q.LastSeq(ctx, metricsdb.LastSeqParams{NodeID: nodeID, Incarnation: incarnation})
	if err != nil {
		return 0, mapErr(err)
	}
	return uint64(max(seq, 0)), nil
}

// ListHeartbeats returns one page of the tenant's heartbeats of a monitor.
func (m *MetricsStore) ListHeartbeats(ctx context.Context, tenantID string, q store.HeartbeatQuery) (store.HeartbeatPage, error) {
	limit := clampLimit(q.Limit, defaultHeartbeatLimit, maxHeartbeatLimit)
	toMs := q.ToMs
	if toMs == 0 {
		toMs = math.MaxInt64
	}
	// Without a cursor start just before FromMs; the keyset condition ts > after_ts
	// then means ts >= FromMs.
	after := store.HeartbeatCursor{TsMs: q.FromMs - 1}
	if q.After != nil {
		after = *q.After
	}
	afterSeq, err := toInt64(after.Seq)
	if err != nil {
		return store.HeartbeatPage{}, err
	}

	rows, err := m.q.ListHeartbeats(ctx, metricsdb.ListHeartbeatsParams{
		TenantID:  tenantID,
		MonitorID: q.MonitorID,
		NodeID:    q.NodeID,
		ToMs:      toMs,
		AfterTs:   after.TsMs,
		AfterNode: after.NodeID,
		AfterInc:  after.Incarnation,
		AfterSeq:  afterSeq,
		RowLimit:  int64(limit + 1),
	})
	if err != nil {
		return store.HeartbeatPage{}, mapErr(err)
	}

	page := store.HeartbeatPage{Items: make([]store.Heartbeat, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			last := page.Items[limit-1]
			page.Next = &store.HeartbeatCursor{TsMs: last.TsMs, NodeID: last.NodeID, Incarnation: last.Incarnation, Seq: last.Seq}
			break
		}
		page.Items = append(page.Items, heartbeatFromRow(r))
	}
	return page, nil
}

func heartbeatFromRow(r metricsdb.Heartbeat) store.Heartbeat {
	return store.Heartbeat{
		NodeID:      r.NodeID,
		Incarnation: r.Incarnation,
		Seq:         uint64(max(r.Seq, 0)),
		TenantID:    r.TenantID,
		MonitorID:   r.MonitorID,
		TsMs:        r.TsMs,
		Status:      store.CheckStatus(r.Status),
		LatencyMs:   int(r.LatencyMs),
		ErrorClass:  r.ErrorClass,
		Message:     r.Message,
		Details:     []byte(r.DetailsJson),
	}
}

// LatestHeartbeats returns the newest heartbeats of a monitor, newest first.
func (m *MetricsStore) LatestHeartbeats(ctx context.Context, tenantID, monitorID, nodeID string, limit int) ([]store.Heartbeat, error) {
	rows, err := m.q.LatestHeartbeats(ctx, metricsdb.LatestHeartbeatsParams{
		TenantID:  tenantID,
		MonitorID: monitorID,
		NodeID:    nodeID,
		RowLimit:  int64(clampLimit(limit, 1, maxHeartbeatLimit)),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Heartbeat, 0, len(rows))
	for _, r := range rows {
		out = append(out, heartbeatFromRow(r))
	}
	return out, nil
}

// latestHeartbeatsByMonitorSQL is LatestHeartbeatsByMonitor's query. It is
// written by hand because sqlc cannot type json_each or recursive CTEs. The ids
// drive one seek each on heartbeats_monitor_ts: the correlated MAX reads a
// single index entry, and the join then reads only the rows at that instant.
// TestLatestHeartbeatsByMonitorPlan guards this plan.
const latestHeartbeatsByMonitorSQL = `
WITH ids AS (SELECT value AS monitor_id FROM json_each(?)),
newest AS (
    SELECT ids.monitor_id AS monitor_id,
           (SELECT MAX(l.ts_ms) FROM heartbeats AS l
            WHERE l.monitor_id = ids.monitor_id AND l.tenant_id = ?) AS ts_ms
    FROM ids
)
SELECT h.node_id, h.incarnation, h.seq, h.tenant_id, h.monitor_id, h.ts_ms, h.status,
       h.latency_ms, h.error_class, h.message, h.details_json
FROM newest AS n
JOIN heartbeats AS h ON h.monitor_id = n.monitor_id AND h.ts_ms = n.ts_ms
WHERE h.tenant_id = ?`

// LatestHeartbeatsByMonitor returns the newest heartbeat of each listed monitor
// over all nodes, in one query. Rows that share the newest instant are resolved
// by the highest seq, as LatestHeartbeats orders them.
func (m *MetricsStore) LatestHeartbeatsByMonitor(ctx context.Context, tenantID string, monitorIDs []string) (map[string]store.Heartbeat, error) {
	out := make(map[string]store.Heartbeat, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("latest heartbeats: monitor IDs: %w", err)
	}
	rows, err := m.db.reader.QueryContext(ctx, latestHeartbeatsByMonitorSQL, string(ids), tenantID, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r metricsdb.Heartbeat
		if err := rows.Scan(&r.NodeID, &r.Incarnation, &r.Seq, &r.TenantID, &r.MonitorID, &r.TsMs, &r.Status,
			&r.LatencyMs, &r.ErrorClass, &r.Message, &r.DetailsJson); err != nil {
			return nil, mapErr(err)
		}
		hb := heartbeatFromRow(r)
		if cur, ok := out[hb.MonitorID]; !ok || hb.TsMs > cur.TsMs || (hb.TsMs == cur.TsMs && hb.Seq > cur.Seq) {
			out[hb.MonitorID] = hb
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// DeleteHeartbeatsBefore deletes one batch of heartbeats older than beforeMs.
func (m *MetricsStore) DeleteHeartbeatsBefore(ctx context.Context, beforeMs int64, batchSize int) (int64, error) {
	if batchSize <= 0 {
		return 0, errors.New("delete heartbeats: batch size must be positive")
	}
	n, err := metricsdb.New(m.db.writer).DeleteHeartbeatsBefore(ctx, metricsdb.DeleteHeartbeatsBeforeParams{
		BeforeMs:  beforeMs,
		BatchSize: int64(batchSize),
	})
	if err != nil {
		return 0, fmt.Errorf("delete heartbeats: %w", mapErr(err))
	}
	return n, nil
}
