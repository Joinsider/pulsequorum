-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Notification channels, their monitor assignment and the replicated alert
-- outbox (ADR 0014, FA-ALR-01/03). Secrets are stored only as ciphertext
-- (ADR 0011): url_enc and the header values in webhook_headers_json.

-- +goose Up
CREATE TABLE notification_channels (
    tenant_id            TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id                   TEXT    NOT NULL,
    name                 TEXT    NOT NULL,
    kind                 TEXT    NOT NULL,
    url_enc              TEXT    NOT NULL,
    webhook_method       TEXT    NOT NULL DEFAULT '',
    webhook_headers_json TEXT    NOT NULL DEFAULT '{}',
    webhook_template     TEXT    NOT NULL DEFAULT '',
    events_json          TEXT    NOT NULL,
    is_default           INTEGER NOT NULL DEFAULT 0,
    -- ok | failing | disabled (FA-ALR-05)
    state                TEXT    NOT NULL DEFAULT 'ok',
    last_error           TEXT    NOT NULL DEFAULT '',
    revision             INTEGER NOT NULL,
    created_ms           INTEGER NOT NULL,
    updated_ms           INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, id)
);

CREATE UNIQUE INDEX notification_channels_id ON notification_channels (id);

CREATE TABLE monitor_channels (
    tenant_id  TEXT NOT NULL,
    monitor_id TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, monitor_id, channel_id),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, channel_id) REFERENCES notification_channels (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX monitor_channels_channel ON monitor_channels (tenant_id, channel_id);

-- One row per alert and channel; alert_key = <monitor>:<since_ms>:<status>[:reminder:<n>].
CREATE TABLE alert_outbox (
    tenant_id       TEXT    NOT NULL,
    alert_key       TEXT    NOT NULL,
    channel_id      TEXT    NOT NULL,
    monitor_id      TEXT    NOT NULL,
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

CREATE INDEX alert_outbox_due ON alert_outbox (state, next_attempt_ms);
CREATE INDEX alert_outbox_updated ON alert_outbox (updated_ms);

-- +goose Down
DROP TABLE alert_outbox;
DROP TABLE monitor_channels;
DROP TABLE notification_channels;
