-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Probe registry columns (ADR 0034, ADR 0035). The FSM writes them from command
-- level 10 on. owner_tenant_id is empty for voters and public probes, and set
-- to the owning tenant ID for private probes. incarnation holds the latest
-- metrics.db incarnation reported by the probe.

-- +goose Up
ALTER TABLE nodes ADD COLUMN owner_tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN incarnation     TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE nodes DROP COLUMN incarnation;
ALTER TABLE nodes DROP COLUMN owner_tenant_id;
