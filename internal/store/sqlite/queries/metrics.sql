/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (C) 2026 The UptimeCluster Authors
 *
 * Queries for metrics.db. Run `make generate` after changes.
 */

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: InsertMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?);

-- name: InsertHeartbeat :execrows
INSERT INTO heartbeats (
    node_id, incarnation, seq, tenant_id, monitor_id, ts_ms, status, latency_ms,
    error_class, message, details_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id, incarnation, seq) DO NOTHING;

-- name: LastSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) AS last_seq
FROM heartbeats WHERE node_id = ? AND incarnation = ?;

-- name: ListHeartbeats :many
-- Keyset pagination ordered by (ts_ms, node_id, incarnation, seq).
SELECT * FROM heartbeats
WHERE tenant_id = sqlc.arg(tenant_id)
  AND monitor_id = sqlc.arg(monitor_id)
  AND (CAST(sqlc.arg(node_id) AS TEXT) = '' OR node_id = sqlc.arg(node_id))
  AND ts_ms <= sqlc.arg(to_ms)
  -- Redundant lower bound: every row matched by the keyset chain below has
  -- ts_ms >= after_ts. Without it SQLite cannot range-scan ts_ms behind the ORs
  -- and reads every retained row of the monitor (see the plan test).
  AND ts_ms >= sqlc.arg(after_ts)
  AND (
        ts_ms > sqlc.arg(after_ts)
     OR (ts_ms = sqlc.arg(after_ts) AND node_id > sqlc.arg(after_node))
     OR (ts_ms = sqlc.arg(after_ts) AND node_id = sqlc.arg(after_node) AND incarnation > sqlc.arg(after_inc))
     OR (ts_ms = sqlc.arg(after_ts) AND node_id = sqlc.arg(after_node) AND incarnation = sqlc.arg(after_inc) AND seq > sqlc.arg(after_seq))
  )
ORDER BY ts_ms, node_id, incarnation, seq
LIMIT sqlc.arg(row_limit);

-- name: LatestHeartbeats :many
-- Newest heartbeats of a monitor, optionally of one node.
SELECT * FROM heartbeats
WHERE tenant_id = sqlc.arg(tenant_id)
  AND monitor_id = sqlc.arg(monitor_id)
  AND (CAST(sqlc.arg(node_id) AS TEXT) = '' OR node_id = sqlc.arg(node_id))
ORDER BY ts_ms DESC, seq DESC
LIMIT sqlc.arg(row_limit);

-- name: DeleteHeartbeatsBefore :execrows
-- One retention batch; short write transactions keep the writer responsive (FA-UPT-06).
DELETE FROM heartbeats
WHERE rowid IN (SELECT h.rowid FROM heartbeats AS h WHERE h.ts_ms < sqlc.arg(before_ms) LIMIT sqlc.arg(batch_size));

-- name: UpsertMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: UpsertRollup :exec
-- Values are written by series ID; the caller resolves it inside the write
-- transaction (ADR 0016). Latencies are microseconds.
INSERT INTO rollups (
    series_id, bucket_ms, start_ms, up_ms, down_ms, maint_ms, unknown_ms,
    latency_count, latency_avg_us, latency_p95_us
) VALUES (
    sqlc.arg(series_id), sqlc.arg(bucket_ms), sqlc.arg(start_ms), sqlc.arg(up_ms), sqlc.arg(down_ms),
    sqlc.arg(maint_ms), sqlc.arg(unknown_ms), sqlc.arg(latency_count), sqlc.arg(latency_avg_us), sqlc.arg(latency_p95_us)
)
ON CONFLICT (series_id, bucket_ms, start_ms) DO UPDATE SET
    up_ms          = excluded.up_ms,
    down_ms        = excluded.down_ms,
    maint_ms       = excluded.maint_ms,
    unknown_ms     = excluded.unknown_ms,
    latency_count  = excluded.latency_count,
    latency_avg_us = excluded.latency_avg_us,
    latency_p95_us = excluded.latency_p95_us;

-- name: ListRollups :many
-- Joins series and filters by tenant in SQL: the tenant boundary never depends
-- on the series cache (ADR 0016, FA-TEN-01).
SELECT s.tenant_id, s.monitor_id, s.node_id, r.bucket_ms, r.start_ms,
       r.up_ms, r.down_ms, r.maint_ms, r.unknown_ms,
       r.latency_count, r.latency_avg_us, r.latency_p95_us
FROM rollups AS r
JOIN series AS s ON s.id = r.series_id
WHERE s.tenant_id = sqlc.arg(tenant_id) AND s.monitor_id = sqlc.arg(monitor_id)
  AND (CAST(sqlc.arg(all_nodes) AS INTEGER) = 1 OR s.node_id = sqlc.arg(node_id))
  AND r.bucket_ms = sqlc.arg(bucket_ms)
  AND r.start_ms >= sqlc.arg(from_ms) AND r.start_ms < sqlc.arg(to_ms)
ORDER BY s.node_id, r.start_ms;

-- name: ListSeriesIDsAfter :many
-- Pages through the series in ID order for the per-series retention batches.
SELECT id FROM series WHERE id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(row_limit);

-- name: DeleteRollupsOfSeriesBefore :execrows
-- One series, one bucket size: the primary key prefix (series_id, bucket_ms)
-- drives both the subquery and the delete, so no statement scans the table.
-- The subquery bounds the batch to row_limit rows.
DELETE FROM rollups
WHERE rollups.series_id = sqlc.arg(series_id) AND rollups.bucket_ms = sqlc.arg(bucket_ms)
  AND rollups.start_ms IN (SELECT r.start_ms FROM rollups AS r
                   WHERE r.series_id = sqlc.arg(series_id) AND r.bucket_ms = sqlc.arg(bucket_ms)
                     AND r.start_ms < sqlc.arg(before_ms)
                   ORDER BY r.start_ms
                   LIMIT sqlc.arg(row_limit));

-- name: InsertLatencyMinute :exec
-- Latencies are microseconds. Existing minutes are kept (FA-UPT-04).
INSERT INTO minutes (
    series_id, minute_ms, min_ms, max_ms, avg_us, p95_us, ok_count, err_count
) VALUES (
    sqlc.arg(series_id), sqlc.arg(minute_ms), sqlc.arg(min_ms), sqlc.arg(max_ms),
    sqlc.arg(avg_us), sqlc.arg(p95_us), sqlc.arg(ok_count), sqlc.arg(err_count)
)
ON CONFLICT (series_id, minute_ms) DO NOTHING;

-- name: DeleteHeartbeat :exec
DELETE FROM heartbeats WHERE node_id = ? AND incarnation = ? AND seq = ?;

-- name: DeleteMinutesOfSeriesBefore :execrows
-- One series: the primary key prefix series_id drives the subquery and the delete.
DELETE FROM minutes
WHERE minutes.series_id = sqlc.arg(series_id)
  AND minutes.minute_ms IN (SELECT m.minute_ms FROM minutes AS m
                    WHERE m.series_id = sqlc.arg(series_id) AND m.minute_ms < sqlc.arg(before_ms)
                    ORDER BY m.minute_ms
                    LIMIT sqlc.arg(row_limit));

-- name: ListRollupsBatch :many
-- Rollups of several monitors of one node (node_id '' is the consensus) and
-- bucket size. Joins series and filters by tenant in SQL (ADR 0016).
SELECT s.tenant_id, s.monitor_id, s.node_id, r.bucket_ms, r.start_ms,
       r.up_ms, r.down_ms, r.maint_ms, r.unknown_ms,
       r.latency_count, r.latency_avg_us, r.latency_p95_us
FROM rollups AS r
JOIN series AS s ON s.id = r.series_id
WHERE s.tenant_id = sqlc.arg(tenant_id)
  AND s.node_id = sqlc.arg(node_id) AND r.bucket_ms = sqlc.arg(bucket_ms)
  AND r.start_ms >= sqlc.arg(from_ms) AND r.start_ms < sqlc.arg(to_ms)
  -- The slice comes last in the WHERE clause: sqlc numbers the named parameters
  -- before the slice's placeholders only if it does.
  AND s.monitor_id IN (sqlc.slice(monitor_ids))
ORDER BY s.monitor_id, r.start_ms;

-- name: GetSeries :one
-- Series are resolved with the tenant: a monitor and node of another tenant
-- never match (ADR 0016).
SELECT id FROM series WHERE tenant_id = ? AND monitor_id = ? AND node_id = ?;

-- name: InsertSeries :one
INSERT INTO series (tenant_id, monitor_id, node_id) VALUES (?, ?, ?) RETURNING id;

-- name: GetStreamPosition :one
SELECT seq FROM stream_positions WHERE node_id = ? AND incarnation = ?;

-- name: UpsertStreamPosition :exec
-- Positions never move backwards: a lower seq leaves the stored one.
INSERT INTO stream_positions (node_id, incarnation, seq) VALUES (?, ?, ?)
ON CONFLICT (node_id, incarnation) DO UPDATE SET seq = MAX(seq, excluded.seq);

-- name: ListStreamPositions :many
SELECT node_id, incarnation, seq FROM stream_positions ORDER BY node_id, incarnation;

-- name: ListStreamSeqsAfter :many
-- Seqs only, to find the contiguous run above a position; the heartbeats primary
-- key serves the range.
SELECT seq FROM heartbeats
WHERE node_id = sqlc.arg(node_id) AND incarnation = sqlc.arg(incarnation) AND seq > sqlc.arg(after_seq)
ORDER BY seq
LIMIT sqlc.arg(row_limit);

-- name: ListStreamHeartbeatsAfter :many
-- One stream in seq order, afterSeq < seq <= untilSeq.
SELECT * FROM heartbeats
WHERE node_id = sqlc.arg(node_id) AND incarnation = sqlc.arg(incarnation)
  AND seq > sqlc.arg(after_seq) AND seq <= sqlc.arg(until_seq)
ORDER BY seq
LIMIT sqlc.arg(row_limit);
