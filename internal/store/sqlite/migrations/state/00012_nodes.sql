-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Node registry (ADR 0017). Each node registers its name, role, capabilities
-- and version; the leader sets healthy from its contact with the node. A
-- revoked node loses its entry.

-- +goose Up
CREATE TABLE nodes (
    id                 TEXT    NOT NULL PRIMARY KEY,
    name               TEXT    NOT NULL,
    role               TEXT    NOT NULL,
    capabilities_json  TEXT    NOT NULL DEFAULT '[]',
    labels_json        TEXT    NOT NULL DEFAULT '{}',
    region             TEXT    NOT NULL DEFAULT '',
    version            TEXT    NOT NULL DEFAULT '',
    max_command_version INTEGER NOT NULL DEFAULT 0,
    healthy            INTEGER NOT NULL,
    health_changed_ms  INTEGER NOT NULL,
    registered_ms      INTEGER NOT NULL,
    updated_ms         INTEGER NOT NULL
);

-- +goose Down
DROP TABLE nodes;
