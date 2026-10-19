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

-- name: DeleteHeartbeatsBefore :execrows
-- One retention batch; short write transactions keep the writer responsive (FA-UPT-06).
DELETE FROM heartbeats
WHERE rowid IN (SELECT h.rowid FROM heartbeats AS h WHERE h.ts_ms < sqlc.arg(before_ms) LIMIT sqlc.arg(batch_size));
