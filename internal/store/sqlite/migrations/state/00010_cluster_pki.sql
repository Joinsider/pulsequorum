-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Cluster PKI (ADR 0006, FA-PKI-01..04). These tables are platform-level data,
-- not tenant-scoped. The CA private key is stored encrypted (key_enc, ADR 0011),
-- join tokens and certificate revocations are kept until they expire.

-- +goose Up
-- One CA per cluster until rotation exists (P5). The fingerprint is the
-- base64url SHA-256 of the CA public key info, as embedded in join tokens.
CREATE TABLE cluster_ca (
    fingerprint TEXT    NOT NULL PRIMARY KEY,
    cluster_id  TEXT    NOT NULL,
    cert_der    BLOB    NOT NULL,
    -- Ciphertext of the PKCS#8 key (ADR 0011).
    key_enc     TEXT    NOT NULL,
    created_ms  INTEGER NOT NULL
);

-- Join tokens (FA-PKI-02). Only the hash of the secret is stored. A token is
-- used once: used_ms is set by the redemption and never cleared.
CREATE TABLE join_tokens (
    id          TEXT    NOT NULL PRIMARY KEY,
    secret_hash TEXT    NOT NULL,
    role        TEXT    NOT NULL,
    -- Only set for probe tokens of a private tenant.
    tenant_id   TEXT    NOT NULL DEFAULT '',
    expires_ms  INTEGER NOT NULL,
    created_ms  INTEGER NOT NULL,
    used_ms     INTEGER,
    used_by     TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX join_tokens_expires ON join_tokens (expires_ms);

-- Revoked node certificates (FA-PKI-04). Entries are pruned after not_after_ms,
-- because an expired certificate is rejected anyway.
CREATE TABLE revoked_certs (
    serial       TEXT    NOT NULL PRIMARY KEY,
    node_id      TEXT    NOT NULL,
    revoked_ms   INTEGER NOT NULL,
    not_after_ms INTEGER NOT NULL
);

CREATE INDEX revoked_certs_not_after ON revoked_certs (not_after_ms);

-- +goose Down
DROP TABLE revoked_certs;
DROP TABLE join_tokens;
DROP TABLE cluster_ca;
