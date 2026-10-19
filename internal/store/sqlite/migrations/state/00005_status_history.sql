-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Consensus status changes, kept 30 days (Pflichtenheft 7.3). They are the
-- source of the consensus uptime (FA-UPT-01); every voter computes the same
-- rollups from them because they are replicated (FA-UPT-03).

-- +goose Up
CREATE TABLE monitor_status_history (
    tenant_id  TEXT    NOT NULL,
    monitor_id TEXT    NOT NULL,
    since_ms   INTEGER NOT NULL,
    status     TEXT    NOT NULL,
    PRIMARY KEY (tenant_id, monitor_id, since_ms),
    FOREIGN KEY (tenant_id, monitor_id) REFERENCES monitors (tenant_id, id) ON DELETE CASCADE
);

INSERT INTO monitor_status_history (tenant_id, monitor_id, since_ms, status)
SELECT tenant_id, monitor_id, since_ms, status FROM monitor_states;

-- +goose Down
DROP TABLE monitor_status_history;
