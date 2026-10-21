-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Local authentication (ADR 0008, FA-AUTH-01..03). Secrets are never stored in
-- plaintext: password_hash is an Argon2id PHC string, refresh tokens are stored
-- only as hashes (token_hash) and the private part of a signing key is
-- encrypted (private_key_enc, ADR 0011).

-- +goose Up
CREATE TABLE users (
    tenant_id     TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id            TEXT    NOT NULL,
    -- Unique across all tenants, compared case-insensitively.
    username      TEXT    NOT NULL COLLATE NOCASE,
    display_name  TEXT    NOT NULL DEFAULT '',
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0,
    revision      INTEGER NOT NULL,
    created_ms    INTEGER NOT NULL,
    updated_ms    INTEGER NOT NULL,
    last_login_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, id)
);

CREATE UNIQUE INDEX users_id ON users (id);
CREATE UNIQUE INDEX users_username ON users (username);

-- Signing keys of access tokens (ADR 0008). Rotated keys stay until retired_ms
-- plus the verification grace period.
CREATE TABLE signing_keys (
    kid             TEXT    NOT NULL PRIMARY KEY,
    alg             TEXT    NOT NULL,
    public_key      BLOB    NOT NULL,
    -- Ciphertext of the private key (ADR 0011).
    private_key_enc TEXT    NOT NULL,
    created_ms      INTEGER NOT NULL,
    retired_ms      INTEGER NOT NULL DEFAULT 0
);

-- One row per refresh token; a token family groups the tokens of one login.
CREATE TABLE refresh_sessions (
    token_hash TEXT    NOT NULL PRIMARY KEY,
    family_id  TEXT    NOT NULL,
    tenant_id  TEXT    NOT NULL,
    user_id    TEXT    NOT NULL,
    created_ms INTEGER NOT NULL,
    expires_ms INTEGER NOT NULL,
    used_ms    INTEGER NOT NULL DEFAULT 0,
    revoked_ms INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX refresh_sessions_family ON refresh_sessions (family_id);
CREATE INDEX refresh_sessions_user ON refresh_sessions (tenant_id, user_id);
CREATE INDEX refresh_sessions_expires ON refresh_sessions (expires_ms);

-- +goose Down
DROP TABLE refresh_sessions;
DROP TABLE signing_keys;
DROP TABLE users;
