-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- metrics.db: data class B (ADR 0004, 0013). Not part of Raft snapshots.

-- +goose Up
-- Key/value metadata; holds the incarnation of this database (ADR 0012 B4).
CREATE TABLE meta (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);

-- Normal rowid table, not WITHOUT ROWID (ADR 0013 C2).
CREATE TABLE heartbeats (
    node_id      TEXT    NOT NULL,
    incarnation  TEXT    NOT NULL,
    seq          INTEGER NOT NULL,
    tenant_id    TEXT    NOT NULL,
    monitor_id   TEXT    NOT NULL,
    ts_ms        INTEGER NOT NULL,
    status       INTEGER NOT NULL,
    latency_ms   INTEGER NOT NULL,
    error_class  TEXT    NOT NULL DEFAULT '',
    message      TEXT    NOT NULL DEFAULT '',
    details_json TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (node_id, incarnation, seq)
);

CREATE INDEX heartbeats_monitor_ts ON heartbeats (monitor_id, ts_ms);
CREATE INDEX heartbeats_ts ON heartbeats (ts_ms);

-- +goose Down
DROP TABLE heartbeats;
DROP TABLE meta;
