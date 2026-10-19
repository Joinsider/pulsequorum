/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (C) 2026 The UptimeCluster Authors
 *
 * Queries for state.db. Run `make generate` after changes.
 */

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = ?;

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY id;

-- name: UpsertTenant :exec
INSERT INTO tenants (id, name, zitadel_org_id, plan, limits_json, suspended, created_ms, updated_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    name           = excluded.name,
    zitadel_org_id = excluded.zitadel_org_id,
    plan           = excluded.plan,
    limits_json    = excluded.limits_json,
    suspended      = excluded.suspended,
    updated_ms     = excluded.updated_ms;

-- name: GetMonitor :one
SELECT * FROM monitors WHERE tenant_id = ? AND id = ?;

-- name: ListMonitors :many
SELECT m.* FROM monitors m
WHERE m.tenant_id = sqlc.arg(tenant_id)
  AND m.id > sqlc.arg(after_id)
  AND (CAST(sqlc.arg(search) AS TEXT) = '' OR m.name LIKE '%' || sqlc.arg(search) || '%')
  AND (CAST(sqlc.arg(tag) AS TEXT) = '' OR EXISTS (
        SELECT 1 FROM monitor_tags t
        WHERE t.tenant_id = m.tenant_id AND t.monitor_id = m.id AND t.tag = sqlc.arg(tag)))
ORDER BY m.id
LIMIT sqlc.arg(row_limit);

-- name: InsertMonitor :exec
INSERT INTO monitors (
    tenant_id, id, name, description, type, config_json, interval_s, timeout_s, retries,
    recovery_threshold, resend_interval_min, assignment_json, consensus_policy, paused,
    revision, created_ms, updated_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateMonitor :execrows
UPDATE monitors SET
    name                = sqlc.arg(name),
    description         = sqlc.arg(description),
    type                = sqlc.arg(type),
    config_json         = sqlc.arg(config_json),
    interval_s          = sqlc.arg(interval_s),
    timeout_s           = sqlc.arg(timeout_s),
    retries             = sqlc.arg(retries),
    recovery_threshold  = sqlc.arg(recovery_threshold),
    resend_interval_min = sqlc.arg(resend_interval_min),
    assignment_json     = sqlc.arg(assignment_json),
    consensus_policy    = sqlc.arg(consensus_policy),
    paused              = sqlc.arg(paused),
    revision            = sqlc.arg(revision),
    updated_ms          = sqlc.arg(updated_ms)
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision);

-- name: MonitorRevision :one
SELECT revision FROM monitors WHERE tenant_id = ? AND id = ?;

-- name: DeleteMonitor :execrows
DELETE FROM monitors WHERE tenant_id = ? AND id = ?;

-- name: ListMonitorTags :many
SELECT tag FROM monitor_tags WHERE tenant_id = ? AND monitor_id = ? ORDER BY tag;

-- name: DeleteMonitorTags :exec
DELETE FROM monitor_tags WHERE tenant_id = ? AND monitor_id = ?;

-- name: InsertMonitorTag :exec
INSERT INTO monitor_tags (tenant_id, monitor_id, tag) VALUES (?, ?, ?);

-- name: GetFSMMeta :one
SELECT value FROM fsm_meta WHERE key = ?;

-- name: SetFSMMeta :exec
INSERT INTO fsm_meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: GetDedup :one
SELECT * FROM request_dedup WHERE request_id = ?;

-- name: InsertDedup :exec
INSERT INTO request_dedup (request_id, applied_index, issued_at_ms, result_json, error_code, error_message)
VALUES (?, ?, ?, ?, ?, ?);

-- name: PruneDedup :execrows
DELETE FROM request_dedup WHERE issued_at_ms < ?;

-- name: ListSecretKeys :many
SELECT * FROM secret_keys ORDER BY created_ms, key_id;

-- name: UpsertSecretKey :exec
-- created_ms is kept on update; rewrapping (KEK rotation) and retiring update the row.
INSERT INTO secret_keys (key_id, wrapped_dek, kek_fingerprint, created_ms, retired_ms)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (key_id) DO UPDATE SET
    wrapped_dek     = excluded.wrapped_dek,
    kek_fingerprint = excluded.kek_fingerprint,
    retired_ms      = excluded.retired_ms;
