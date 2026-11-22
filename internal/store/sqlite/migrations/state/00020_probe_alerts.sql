-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Offline alert state for private probes (FA-PRB-06, ADR 0035). The FSM writes
-- this table from command level 10 on; rows are cleared when a probe is
-- revoked or its owning tenant is deleted.

-- +goose Up
CREATE TABLE probe_alerts (
    tenant_id  TEXT    NOT NULL,
    probe_id   TEXT    NOT NULL,
    active     INTEGER NOT NULL DEFAULT 0,
    since_ms   INTEGER NOT NULL,
    updated_ms INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, probe_id)
);

-- +goose Down
DROP TABLE probe_alerts;
