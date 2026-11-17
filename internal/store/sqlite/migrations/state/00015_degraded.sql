-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Degraded mode and correction after the quorum returned (ADR 0024). A node
-- that sent alerts without quorum reports them to the leader afterwards;
-- unconfirmed_alerts holds them until the leader has judged each one. The
-- table has no foreign keys: the monitor or channel may be deleted meanwhile.
--
-- reevaluated marks incidents that were added to a gap in the history after
-- the quorum returned (FA-UPT-07). They never caused a notification.

-- +goose Up
CREATE TABLE unconfirmed_alerts (
    node_id     TEXT    NOT NULL,
    -- ID in the local degraded log of node_id; unique per node.
    local_id    INTEGER NOT NULL,
    tenant_id   TEXT    NOT NULL,
    monitor_id  TEXT    NOT NULL,
    channel_id  TEXT    NOT NULL,
    status      TEXT    NOT NULL,
    since_ms    INTEGER NOT NULL,
    sent_ms     INTEGER NOT NULL,
    outcome     TEXT    NOT NULL,
    reachable   INTEGER NOT NULL,
    total       INTEGER NOT NULL,
    -- JSON array of the node IDs that reported the change.
    reporters   TEXT    NOT NULL DEFAULT '[]',
    -- Command time at which the report was stored.
    reported_ms INTEGER NOT NULL,
    -- Empty while open; "confirmed", "false_alarm" or "unverifiable" afterwards.
    verdict     TEXT    NOT NULL DEFAULT '',
    resolved_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, local_id)
);

CREATE INDEX unconfirmed_alerts_verdict ON unconfirmed_alerts (verdict, since_ms);

ALTER TABLE incidents ADD COLUMN reevaluated INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE incidents DROP COLUMN reevaluated;

DROP INDEX unconfirmed_alerts_verdict;
DROP TABLE unconfirmed_alerts;
