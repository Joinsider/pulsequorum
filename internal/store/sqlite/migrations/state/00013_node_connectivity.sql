-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Node connectivity (ADR 0022). The leader marks a node impaired when its
-- reference targets fail or its heartbeats deviate from the peers; the reason
-- is "reference" or "deviation". Existing nodes start unimpaired.

-- +goose Up
ALTER TABLE nodes ADD COLUMN connectivity_impaired   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN connectivity_reason     TEXT    NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN connectivity_changed_ms INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE nodes DROP COLUMN connectivity_changed_ms;
ALTER TABLE nodes DROP COLUMN connectivity_reason;
ALTER TABLE nodes DROP COLUMN connectivity_impaired;
