-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Rollups and latency minutes (FA-UPT-03/04, Pflichtenheft 7.2). Computed
-- locally and deterministically on every voter; never replicated.

-- +goose Up
-- node_id '' is the consensus view. Durations in milliseconds.
CREATE TABLE uptime_rollups (
    tenant_id     TEXT    NOT NULL,
    monitor_id    TEXT    NOT NULL,
    node_id       TEXT    NOT NULL,
    bucket_ms     INTEGER NOT NULL,
    start_ms      INTEGER NOT NULL,
    up_ms         INTEGER NOT NULL,
    down_ms       INTEGER NOT NULL,
    maint_ms      INTEGER NOT NULL,
    unknown_ms    INTEGER NOT NULL,
    latency_count INTEGER NOT NULL,
    latency_avg   REAL    NOT NULL,
    latency_p95   REAL    NOT NULL,
    PRIMARY KEY (monitor_id, node_id, bucket_ms, start_ms)
);

CREATE INDEX uptime_rollups_start ON uptime_rollups (bucket_ms, start_ms);

CREATE TABLE latency_minutes (
    tenant_id  TEXT    NOT NULL,
    monitor_id TEXT    NOT NULL,
    node_id    TEXT    NOT NULL,
    minute_ms  INTEGER NOT NULL,
    min_ms     INTEGER NOT NULL,
    max_ms     INTEGER NOT NULL,
    avg_ms     REAL    NOT NULL,
    p95_ms     REAL    NOT NULL,
    ok_count   INTEGER NOT NULL,
    err_count  INTEGER NOT NULL,
    PRIMARY KEY (monitor_id, node_id, minute_ms)
);

CREATE INDEX latency_minutes_minute ON latency_minutes (minute_ms);

-- +goose Down
DROP TABLE latency_minutes;
DROP TABLE uptime_rollups;
