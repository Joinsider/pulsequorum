-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Local log of the alerts a voter sent while it was degraded (FA-ALR-10,
-- ADR 0024). It is node-local: it is neither replicated nor part of the
-- fingerprint. reported is set once the entry reached the leader; entries are
-- kept 30 days.

-- +goose Up
CREATE TABLE degraded_alerts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id  TEXT    NOT NULL,
    monitor_id TEXT    NOT NULL,
    channel_id TEXT    NOT NULL,
    status     TEXT    NOT NULL,
    since_ms   INTEGER NOT NULL,
    sent_ms    INTEGER NOT NULL,
    -- 'sent', 'failed' or 'suppressed'.
    outcome    TEXT    NOT NULL,
    reachable  INTEGER NOT NULL,
    total      INTEGER NOT NULL,
    -- JSON array of the node IDs that reported the change.
    reporters  TEXT    NOT NULL DEFAULT '[]',
    reported   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX degraded_alerts_reported ON degraded_alerts (reported, id);
CREATE INDEX degraded_alerts_sent ON degraded_alerts (sent_ms);

-- +goose Down
DROP INDEX degraded_alerts_sent;
DROP INDEX degraded_alerts_reported;
DROP TABLE degraded_alerts;
