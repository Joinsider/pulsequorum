-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Consensus status and incidents (FA-CON-03/06, FA-INC-01, Konzept 6.5). Both
-- are data class A: only status changes are written, by the leader via Raft.

-- +goose Up
-- One row per monitor, created and kept in sync with pause/resume by the FSM.
CREATE TABLE monitor_states (
    tenant_id  TEXT    NOT NULL,
    monitor_id TEXT    NOT NULL,
    status     TEXT    NOT NULL,
    -- 0 if set without quorum (degraded mode, phase 3).
    confirmed  INTEGER NOT NULL DEFAULT 1,
    since_ms   INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, monitor_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

-- Monitors created before this migration start as PENDING (or PAUSED). The rows
-- derive only from replicated data, so every voter computes the same values.
INSERT INTO monitor_states (tenant_id, monitor_id, status, confirmed, since_ms)
SELECT tenant_id, id, CASE paused WHEN 1 THEN 'PAUSED' ELSE 'PENDING' END, 1, updated_ms
FROM monitors;

-- An incident starts with the change to DOWN and ends when the monitor leaves
-- DOWN; resolved_ms = 0 means open. Incidents are deleted with their monitor.
CREATE TABLE incidents (
    tenant_id           TEXT    NOT NULL,
    id                  TEXT    NOT NULL,
    monitor_id          TEXT    NOT NULL,
    started_ms          INTEGER NOT NULL,
    resolved_ms         INTEGER NOT NULL DEFAULT 0,
    cause               TEXT    NOT NULL DEFAULT '',
    error_class         TEXT    NOT NULL DEFAULT '',
    affected_nodes_json TEXT    NOT NULL DEFAULT '[]',
    confirmed           INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

-- Incident IDs are UUIDs and therefore unique across tenants.
CREATE UNIQUE INDEX incidents_id ON incidents (id);
-- At most one open incident per monitor.
CREATE UNIQUE INDEX incidents_open ON incidents (monitor_id) WHERE resolved_ms = 0;
CREATE INDEX incidents_started ON incidents (tenant_id, started_ms, id);
CREATE INDEX incidents_monitor ON incidents (tenant_id, monitor_id, started_ms);

-- +goose Down
DROP TABLE incidents;
DROP TABLE monitor_states;
