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
-- Monitors and their state are written in the same FSM transaction; the
-- defaults only cover monitors stored without the FSM (store tests).
SELECT sqlc.embed(m),
       CAST(COALESCE(s.status, 'PENDING') AS TEXT)          AS state_status,
       CAST(COALESCE(s.confirmed, 1) AS INTEGER)            AS state_confirmed,
       CAST(COALESCE(s.since_ms, m.created_ms) AS INTEGER)  AS state_since_ms
FROM monitors m
LEFT JOIN monitor_states s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
WHERE m.tenant_id = sqlc.arg(tenant_id) AND m.id = sqlc.arg(id);

-- name: ListMonitors :many
SELECT sqlc.embed(m),
       CAST(COALESCE(s.status, 'PENDING') AS TEXT)          AS state_status,
       CAST(COALESCE(s.confirmed, 1) AS INTEGER)            AS state_confirmed,
       CAST(COALESCE(s.since_ms, m.created_ms) AS INTEGER)  AS state_since_ms
FROM monitors m
LEFT JOIN monitor_states s ON s.tenant_id = m.tenant_id AND s.monitor_id = m.id
WHERE m.tenant_id = sqlc.arg(tenant_id)
  AND m.id > sqlc.arg(after_id)
  AND (CAST(sqlc.arg(status) AS TEXT) = '' OR COALESCE(s.status, 'PENDING') = sqlc.arg(status))
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

-- name: GetMonitorState :one
SELECT * FROM monitor_states WHERE tenant_id = ? AND monitor_id = ?;

-- name: UpsertMonitorState :exec
INSERT INTO monitor_states (tenant_id, monitor_id, status, confirmed, since_ms)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, monitor_id) DO UPDATE SET
    status    = excluded.status,
    confirmed = excluded.confirmed,
    since_ms  = excluded.since_ms;

-- name: InsertIncident :exec
INSERT INTO incidents (tenant_id, id, monitor_id, started_ms, cause, error_class, affected_nodes_json, confirmed)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ResolveIncident :execrows
UPDATE incidents SET resolved_ms = sqlc.arg(resolved_ms)
WHERE tenant_id = sqlc.arg(tenant_id) AND monitor_id = sqlc.arg(monitor_id) AND resolved_ms = 0;

-- name: GetIncident :one
SELECT sqlc.embed(i), m.name AS monitor_name
FROM incidents i
JOIN monitors m ON m.tenant_id = i.tenant_id AND m.id = i.monitor_id
WHERE i.tenant_id = sqlc.arg(tenant_id) AND i.id = sqlc.arg(id);

-- name: ListIncidents :many
-- Newest first with keyset (started_ms, id) descending. An incident matches
-- the period if it overlaps [from_ms, to_ms].
SELECT sqlc.embed(i), m.name AS monitor_name
FROM incidents i
JOIN monitors m ON m.tenant_id = i.tenant_id AND m.id = i.monitor_id
WHERE i.tenant_id = sqlc.arg(tenant_id)
  AND (CAST(sqlc.arg(monitor_id) AS TEXT) = '' OR i.monitor_id = sqlc.arg(monitor_id))
  AND i.started_ms <= sqlc.arg(to_ms)
  AND (i.resolved_ms = 0 OR i.resolved_ms >= sqlc.arg(from_ms))
  AND (CAST(sqlc.arg(open_only) AS INTEGER) = 0 OR i.resolved_ms = 0)
  AND (i.started_ms < sqlc.arg(before_started_ms)
       OR (i.started_ms = sqlc.arg(before_started_ms) AND i.id < sqlc.arg(before_id)))
ORDER BY i.started_ms DESC, i.id DESC
LIMIT sqlc.arg(row_limit);

-- name: UpsertStatusHistory :exec
-- A second change at the same instant (PENDING, then DOWN dated back to the
-- same failure) replaces the first.
INSERT INTO monitor_status_history (tenant_id, monitor_id, since_ms, status)
VALUES (?, ?, ?, ?)
ON CONFLICT (tenant_id, monitor_id, since_ms) DO UPDATE SET status = excluded.status;

-- name: PruneStatusHistory :exec
-- Removes changes before before_ms but keeps the last one at or before it,
-- which still defines the status at before_ms.
DELETE FROM monitor_status_history AS h
WHERE h.tenant_id = sqlc.arg(tenant_id) AND h.monitor_id = sqlc.arg(monitor_id)
  AND h.since_ms < sqlc.arg(before_ms)
  AND h.since_ms < (
        SELECT MAX(n.since_ms) FROM monitor_status_history n
        WHERE n.tenant_id = sqlc.arg(tenant_id) AND n.monitor_id = sqlc.arg(monitor_id)
          AND n.since_ms <= sqlc.arg(before_ms));

-- name: StatusHistory :many
-- Changes in [from_ms, to_ms) plus the last change before from_ms.
SELECT h.since_ms, h.status FROM monitor_status_history h
WHERE h.tenant_id = sqlc.arg(tenant_id) AND h.monitor_id = sqlc.arg(monitor_id)
  AND h.since_ms < sqlc.arg(to_ms)
  AND h.since_ms >= COALESCE((
        SELECT MAX(p.since_ms) FROM monitor_status_history p
        WHERE p.tenant_id = sqlc.arg(tenant_id) AND p.monitor_id = sqlc.arg(monitor_id)
          AND p.since_ms <= sqlc.arg(from_ms)), sqlc.arg(from_ms))
ORDER BY h.since_ms;

-- name: GetChannel :one
SELECT * FROM notification_channels WHERE tenant_id = ? AND id = ?;

-- name: ListChannels :many
SELECT * FROM notification_channels WHERE tenant_id = ? ORDER BY name, id;

-- name: InsertChannel :exec
INSERT INTO notification_channels (
    tenant_id, id, name, kind, url_enc, webhook_method, webhook_headers_json, webhook_template,
    events_json, is_default, state, last_error, revision, created_ms, updated_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok', '', ?, ?, ?);

-- name: UpdateChannel :execrows
-- An update by the user resets the delivery state (the channel may be fixed).
UPDATE notification_channels SET
    name                 = sqlc.arg(name),
    kind                 = sqlc.arg(kind),
    url_enc              = sqlc.arg(url_enc),
    webhook_method       = sqlc.arg(webhook_method),
    webhook_headers_json = sqlc.arg(webhook_headers_json),
    webhook_template     = sqlc.arg(webhook_template),
    events_json          = sqlc.arg(events_json),
    is_default           = sqlc.arg(is_default),
    state                = 'ok',
    last_error           = '',
    revision             = sqlc.arg(revision),
    updated_ms           = sqlc.arg(updated_ms)
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision);

-- name: ChannelRevision :one
SELECT revision FROM notification_channels WHERE tenant_id = ? AND id = ?;

-- name: SetChannelState :exec
UPDATE notification_channels SET state = ?, last_error = ?
WHERE tenant_id = ? AND id = ?;

-- name: DeleteChannel :execrows
DELETE FROM notification_channels WHERE tenant_id = ? AND id = ?;

-- name: ListMonitorChannelIDs :many
SELECT channel_id FROM monitor_channels WHERE tenant_id = ? AND monitor_id = ? ORDER BY channel_id;

-- name: DeleteMonitorChannels :exec
DELETE FROM monitor_channels WHERE tenant_id = ? AND monitor_id = ?;

-- name: InsertMonitorChannel :exec
INSERT INTO monitor_channels (tenant_id, monitor_id, channel_id) VALUES (?, ?, ?);

-- name: InsertAlert :exec
-- The same alert for the same channel is enqueued once (FA-ALR-03).
INSERT INTO alert_outbox (
    tenant_id, alert_key, channel_id, monitor_id, payload_json, next_attempt_ms, created_ms, updated_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (alert_key, channel_id) DO NOTHING;

-- name: GetAlert :one
SELECT * FROM alert_outbox WHERE alert_key = ? AND channel_id = ?;

-- name: UpdateAlert :exec
UPDATE alert_outbox SET attempts = ?, next_attempt_ms = ?, state = ?, last_error = ?, updated_ms = ?
WHERE alert_key = ? AND channel_id = ?;

-- name: DueAlerts :many
SELECT * FROM alert_outbox
WHERE state = 'pending' AND next_attempt_ms <= sqlc.arg(now_ms)
ORDER BY next_attempt_ms, alert_key, channel_id
LIMIT sqlc.arg(row_limit);

-- name: PruneAlerts :exec
DELETE FROM alert_outbox WHERE state <> 'pending' AND updated_ms < ?;

-- name: GetMaintenanceWindow :one
SELECT * FROM maintenance_windows WHERE tenant_id = ? AND id = ?;

-- name: ListMaintenanceWindows :many
SELECT * FROM maintenance_windows WHERE tenant_id = ? ORDER BY name, id;

-- name: ListAllMaintenanceWindows :many
SELECT * FROM maintenance_windows ORDER BY tenant_id, id;

-- name: InsertMaintenanceWindow :exec
INSERT INTO maintenance_windows (
    tenant_id, id, name, description, monitor_ids_json, schedule_json, active, revision, created_ms, updated_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateMaintenanceWindow :execrows
-- created_ms is kept: it is set once by the create.
UPDATE maintenance_windows SET
    name             = sqlc.arg(name),
    description      = sqlc.arg(description),
    monitor_ids_json = sqlc.arg(monitor_ids_json),
    schedule_json    = sqlc.arg(schedule_json),
    active           = sqlc.arg(active),
    revision         = sqlc.arg(revision),
    updated_ms       = sqlc.arg(updated_ms)
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision);

-- name: MaintenanceWindowRevision :one
SELECT revision FROM maintenance_windows WHERE tenant_id = ? AND id = ?;

-- name: DeleteMaintenanceWindow :execrows
DELETE FROM maintenance_windows WHERE tenant_id = ? AND id = ?;
