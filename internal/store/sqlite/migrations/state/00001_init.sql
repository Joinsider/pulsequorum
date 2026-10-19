-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- state.db: data class A, written only by the Raft FSM (ADR 0004, 0012 A1).
-- Times are UTC milliseconds, booleans 0/1 (portable to PostgreSQL).
-- auto_vacuum=INCREMENTAL is set via the DSN before this migration runs (ADR 0013 C3).

-- +goose Up
CREATE TABLE tenants (
    id             TEXT    NOT NULL PRIMARY KEY,
    name           TEXT    NOT NULL,
    zitadel_org_id TEXT    UNIQUE,
    plan           TEXT    NOT NULL DEFAULT 'default',
    limits_json    TEXT    NOT NULL DEFAULT '{}',
    suspended      INTEGER NOT NULL DEFAULT 0,
    created_ms     INTEGER NOT NULL,
    updated_ms     INTEGER NOT NULL
);

CREATE TABLE monitors (
    tenant_id           TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id                  TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    description         TEXT    NOT NULL DEFAULT '',
    type                TEXT    NOT NULL,
    config_json         TEXT    NOT NULL,
    interval_s          INTEGER NOT NULL,
    timeout_s           INTEGER NOT NULL,
    retries             INTEGER NOT NULL,
    recovery_threshold  INTEGER NOT NULL,
    resend_interval_min INTEGER NOT NULL,
    assignment_json     TEXT    NOT NULL,
    consensus_policy    TEXT    NOT NULL,
    paused              INTEGER NOT NULL DEFAULT 0,
    revision            INTEGER NOT NULL,
    created_ms          INTEGER NOT NULL,
    updated_ms          INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

-- Monitor IDs are UUIDs and therefore unique across tenants.
CREATE UNIQUE INDEX monitors_id ON monitors (id);

CREATE TABLE monitor_tags (
    tenant_id  TEXT NOT NULL,
    monitor_id TEXT NOT NULL,
    tag        TEXT NOT NULL,
    PRIMARY KEY (tenant_id, monitor_id, tag),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX monitor_tags_tag ON monitor_tags (tenant_id, tag);

-- +goose Down
DROP TABLE monitor_tags;
DROP TABLE monitors;
DROP TABLE tenants;
