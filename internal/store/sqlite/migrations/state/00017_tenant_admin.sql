-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Plans, tenant source and tenant tombstones (ADR 0029). The FSM writes them
-- from command level 8 on.
--
-- tenants.source records how a tenant was created: admin, auto (automatic
-- provisioning, counted against its maximum) or bootstrap (the default and
-- system tenants and tenants created before this migration).
-- The seeded plans have timestamp 0, so every replica holds identical rows.
-- deleted_tenants keeps the IDs of deleted tenants; an ID is never reused.

-- +goose Up
ALTER TABLE tenants ADD COLUMN source TEXT NOT NULL DEFAULT 'bootstrap';

CREATE INDEX tenants_plan ON tenants (plan);
CREATE INDEX tenants_source ON tenants (source);

CREATE TABLE plans (
    id           TEXT    NOT NULL PRIMARY KEY,
    name         TEXT    NOT NULL,
    -- Canonical JSON of the limits; a missing or null field is unlimited.
    limits_json  TEXT    NOT NULL DEFAULT '{}',
    created_ms   INTEGER NOT NULL,
    updated_ms   INTEGER NOT NULL
);

INSERT INTO plans (id, name, limits_json, created_ms, updated_ms)
VALUES ('default', 'Default', '{}', 0, 0), ('system', 'System', '{}', 0, 0);

CREATE TABLE deleted_tenants (
    id         TEXT    NOT NULL PRIMARY KEY,
    deleted_ms INTEGER NOT NULL
);

-- +goose Down
DROP TABLE deleted_tenants;
DROP TABLE plans;
DROP INDEX tenants_source;
DROP INDEX tenants_plan;
ALTER TABLE tenants DROP COLUMN source;
