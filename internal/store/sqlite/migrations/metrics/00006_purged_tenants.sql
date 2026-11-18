-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Tenants whose data this node has purged from metrics.db (ADR 0029). It is
-- node-local: every node purges on its own after it sees the tombstone, and a
-- restart continues where the purge stopped. degraded_alerts gets a tenant
-- index; heartbeats deliberately not: it would grow the hottest table by a
-- text column per row (ADR 0016) for a rare purge, which scans instead.

-- +goose Up
CREATE TABLE purged_tenants (
    tenant_id TEXT    NOT NULL PRIMARY KEY,
    purged_ms INTEGER NOT NULL
);
CREATE INDEX degraded_alerts_tenant ON degraded_alerts (tenant_id);

-- +goose Down
DROP INDEX degraded_alerts_tenant;
DROP TABLE purged_tenants;
