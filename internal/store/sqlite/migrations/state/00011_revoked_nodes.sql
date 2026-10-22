-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Revoked nodes (FA-PKI-04). A node revocation rejects every certificate of the
-- node that was issued before revoked_ms. Unlike revoked_certs, the entries are
-- kept: they have no expiry that would make them obsolete.

-- +goose Up
CREATE TABLE revoked_nodes (
    node_id    TEXT    NOT NULL PRIMARY KEY,
    revoked_ms INTEGER NOT NULL
);

-- +goose Down
DROP TABLE revoked_nodes;
