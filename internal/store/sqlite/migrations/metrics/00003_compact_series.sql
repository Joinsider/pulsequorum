-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Compact storage for latency minutes and rollups (ADR 0016). A series is one
-- (tenant, monitor, node) of this node's metrics.db; its integer ID replaces
-- three UUID texts per row. Series IDs are local to this file and never
-- replicated. The tenant is part of the unique key, so a monitor and node of
-- another tenant can never resolve to the same series: every read joins
-- series and filters by tenant_id (FA-TEN-01).
--
-- Rows live once in a WITHOUT ROWID primary key. Latencies are integers:
-- min/max in milliseconds, avg/p95 in microseconds (no loss for weighted
-- means at the precision the API reports). Retention deletes per series over
-- the primary key, so there is no separate time index.

-- +goose Up
CREATE TABLE series (
    id         INTEGER PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    monitor_id TEXT NOT NULL,
    node_id    TEXT NOT NULL,
    UNIQUE (tenant_id, monitor_id, node_id)
);

INSERT INTO series (tenant_id, monitor_id, node_id)
SELECT tenant_id, monitor_id, node_id FROM uptime_rollups
UNION
SELECT tenant_id, monitor_id, node_id FROM latency_minutes
ORDER BY 1, 2, 3;

CREATE TABLE rollups (
    series_id      INTEGER NOT NULL,
    bucket_ms      INTEGER NOT NULL,
    start_ms       INTEGER NOT NULL,
    up_ms          INTEGER NOT NULL,
    down_ms        INTEGER NOT NULL,
    maint_ms       INTEGER NOT NULL,
    unknown_ms     INTEGER NOT NULL,
    latency_count  INTEGER NOT NULL,
    latency_avg_us INTEGER NOT NULL,
    latency_p95_us INTEGER NOT NULL,
    PRIMARY KEY (series_id, bucket_ms, start_ms)
) WITHOUT ROWID;

INSERT INTO rollups
SELECT s.id, r.bucket_ms, r.start_ms, r.up_ms, r.down_ms, r.maint_ms, r.unknown_ms, r.latency_count,
       CAST(ROUND(r.latency_avg * 1000) AS INTEGER), CAST(ROUND(r.latency_p95 * 1000) AS INTEGER)
FROM uptime_rollups r
JOIN series s ON s.tenant_id = r.tenant_id AND s.monitor_id = r.monitor_id AND s.node_id = r.node_id;

CREATE TABLE minutes (
    series_id INTEGER NOT NULL,
    minute_ms INTEGER NOT NULL,
    min_ms    INTEGER NOT NULL,
    max_ms    INTEGER NOT NULL,
    avg_us    INTEGER NOT NULL,
    p95_us    INTEGER NOT NULL,
    ok_count  INTEGER NOT NULL,
    err_count INTEGER NOT NULL,
    PRIMARY KEY (series_id, minute_ms)
) WITHOUT ROWID;

INSERT INTO minutes
SELECT s.id, l.minute_ms, l.min_ms, l.max_ms,
       CAST(ROUND(l.avg_ms * 1000) AS INTEGER), CAST(ROUND(l.p95_ms * 1000) AS INTEGER), l.ok_count, l.err_count
FROM latency_minutes l
JOIN series s ON s.tenant_id = l.tenant_id AND s.monitor_id = l.monitor_id AND s.node_id = l.node_id;

DROP TABLE latency_minutes;
DROP TABLE uptime_rollups;

-- +goose Down
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
INSERT INTO uptime_rollups
SELECT s.tenant_id, s.monitor_id, s.node_id, r.bucket_ms, r.start_ms, r.up_ms, r.down_ms, r.maint_ms, r.unknown_ms,
       r.latency_count, r.latency_avg_us / 1000.0, r.latency_p95_us / 1000.0
FROM rollups r JOIN series s ON s.id = r.series_id;

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
INSERT INTO latency_minutes
SELECT s.tenant_id, s.monitor_id, s.node_id, m.minute_ms, m.min_ms, m.max_ms, m.avg_us / 1000.0, m.p95_us / 1000.0,
       m.ok_count, m.err_count
FROM minutes m JOIN series s ON s.id = m.series_id;

DROP TABLE minutes;
DROP TABLE rollups;
DROP TABLE series;
