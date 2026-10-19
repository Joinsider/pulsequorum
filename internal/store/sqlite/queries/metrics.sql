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
INSERT INTO uptime_rollups (
    tenant_id, monitor_id, node_id, bucket_ms, start_ms, up_ms, down_ms, maint_ms, unknown_ms,
    latency_count, latency_avg, latency_p95
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (monitor_id, node_id, bucket_ms, start_ms) DO UPDATE SET
    up_ms         = excluded.up_ms,
    down_ms       = excluded.down_ms,
    maint_ms      = excluded.maint_ms,
    unknown_ms    = excluded.unknown_ms,
    latency_count = excluded.latency_count,
    latency_avg   = excluded.latency_avg,
    latency_p95   = excluded.latency_p95;

-- name: ListRollups :many
SELECT * FROM uptime_rollups
WHERE tenant_id = sqlc.arg(tenant_id) AND monitor_id = sqlc.arg(monitor_id)
  AND (CAST(sqlc.arg(all_nodes) AS INTEGER) = 1 OR node_id = sqlc.arg(node_id))
  AND bucket_ms = sqlc.arg(bucket_ms)
  AND start_ms >= sqlc.arg(from_ms) AND start_ms < sqlc.arg(to_ms)
ORDER BY node_id, start_ms;

-- name: DeleteRollupsBefore :execrows
DELETE FROM uptime_rollups
WHERE rowid IN (SELECT r.rowid FROM uptime_rollups AS r
                WHERE r.bucket_ms = sqlc.arg(bucket_ms) AND r.start_ms < sqlc.arg(before_ms)
                LIMIT sqlc.arg(batch_size));

-- name: InsertLatencyMinute :exec
INSERT INTO latency_minutes (
    tenant_id, monitor_id, node_id, minute_ms, min_ms, max_ms, avg_ms, p95_ms, ok_count, err_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (monitor_id, node_id, minute_ms) DO NOTHING;

-- name: DeleteHeartbeat :exec
DELETE FROM heartbeats WHERE node_id = ? AND incarnation = ? AND seq = ?;

-- name: DeleteLatencyMinutesBefore :execrows
DELETE FROM latency_minutes
WHERE rowid IN (SELECT l.rowid FROM latency_minutes AS l WHERE l.minute_ms < sqlc.arg(before_ms) LIMIT sqlc.arg(batch_size));
