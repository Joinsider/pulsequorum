-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Maintenance windows (FA-MNT-01). Monitor IDs are kept as JSON without a
-- foreign key: IDs of deleted monitors are ignored when windows are evaluated.
-- schedule_json holds the MaintenanceSchedule document.

-- +goose Up
CREATE TABLE maintenance_windows (
    tenant_id        TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id               TEXT    NOT NULL,
    name             TEXT    NOT NULL,
    description      TEXT    NOT NULL DEFAULT '',
    monitor_ids_json TEXT    NOT NULL DEFAULT '[]',
    schedule_json    TEXT    NOT NULL,
    active           INTEGER NOT NULL DEFAULT 1,
    revision         INTEGER NOT NULL,
    created_ms       INTEGER NOT NULL,
    updated_ms       INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE UNIQUE INDEX maintenance_windows_id ON maintenance_windows (id);

-- +goose Down
DROP TABLE maintenance_windows;
