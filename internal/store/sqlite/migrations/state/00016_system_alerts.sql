-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- System alerts and the last known incarnation of each node (FA-OBS-01,
-- FA-REP-06, ADR 0025). The FSM writes both from command level 6 on.
--
-- system_alerts holds the current state of each alert; a change enqueues the
-- notification in alert_outbox. node_incarnations is the reference the leader
-- compares a reported incarnation with, so that a leader change is no change.
--
-- alert_outbox.monitor_id becomes nullable: a system alert belongs to no
-- monitor. NULL skips the monitor foreign key (SQLite does not check a foreign
-- key with a NULL column), and every other row keeps its monitor.
-- The table is rebuilt because SQLite cannot drop NOT NULL in place.
-- Its only referrers are its own indexes, so nothing else changes.

-- +goose Up
CREATE TABLE system_alerts (
    kind         TEXT    NOT NULL,
    subject      TEXT    NOT NULL,
    active       INTEGER NOT NULL,
    -- Expiry stage of a certificate or a counter; 0 if unused.
    level        INTEGER NOT NULL,
    -- Start of the current active period; kept while the alert clears.
    since_ms     INTEGER NOT NULL,
    updated_ms   INTEGER NOT NULL,
    details_json TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (kind, subject)
);

CREATE TABLE node_incarnations (
    node_id     TEXT    PRIMARY KEY,
    incarnation TEXT    NOT NULL,
    seen_ms     INTEGER NOT NULL
);

CREATE TABLE alert_outbox_new (
    tenant_id       TEXT    NOT NULL,
    alert_key       TEXT    NOT NULL,
    channel_id      TEXT    NOT NULL,
    -- NULL for system alerts (ADR 0025); the monitor foreign key is not checked then.
    monitor_id      TEXT,
    payload_json    TEXT    NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_ms INTEGER NOT NULL,
    -- pending | delivered | failed
    state           TEXT    NOT NULL DEFAULT 'pending',
    last_error      TEXT    NOT NULL DEFAULT '',
    created_ms      INTEGER NOT NULL,
    updated_ms      INTEGER NOT NULL,
    PRIMARY KEY (alert_key, channel_id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

INSERT INTO alert_outbox_new (tenant_id, alert_key, channel_id, monitor_id, payload_json, attempts,
                              next_attempt_ms, state, last_error, created_ms, updated_ms)
SELECT tenant_id, alert_key, channel_id, monitor_id, payload_json, attempts,
       next_attempt_ms, state, last_error, created_ms, updated_ms
FROM alert_outbox;

DROP TABLE alert_outbox;
ALTER TABLE alert_outbox_new RENAME TO alert_outbox;

CREATE INDEX alert_outbox_due ON alert_outbox (state, next_attempt_ms);
CREATE INDEX alert_outbox_updated ON alert_outbox (updated_ms);

-- +goose Down
-- System alerts exist only in the system tenant; the monitor column is NOT NULL again.
DELETE FROM alert_outbox WHERE monitor_id IS NULL;

CREATE TABLE alert_outbox_old (
    tenant_id       TEXT    NOT NULL,
    alert_key       TEXT    NOT NULL,
    channel_id      TEXT    NOT NULL,
    monitor_id      TEXT    NOT NULL,
    payload_json    TEXT    NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_ms INTEGER NOT NULL,
    state           TEXT    NOT NULL DEFAULT 'pending',
    last_error      TEXT    NOT NULL DEFAULT '',
    created_ms      INTEGER NOT NULL,
    updated_ms      INTEGER NOT NULL,
    PRIMARY KEY (alert_key, channel_id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

INSERT INTO alert_outbox_old (tenant_id, alert_key, channel_id, monitor_id, payload_json, attempts,
                              next_attempt_ms, state, last_error, created_ms, updated_ms)
SELECT tenant_id, alert_key, channel_id, monitor_id, payload_json, attempts,
       next_attempt_ms, state, last_error, created_ms, updated_ms
FROM alert_outbox;

DROP TABLE alert_outbox;
ALTER TABLE alert_outbox_old RENAME TO alert_outbox;

CREATE INDEX alert_outbox_due ON alert_outbox (state, next_attempt_ms);
CREATE INDEX alert_outbox_updated ON alert_outbox (updated_ms);

DROP TABLE node_incarnations;
DROP TABLE system_alerts;
