-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- API tokens and the audit log (ADR 0031, FA-AUTH-10, FA-DAT-01). The FSM
-- writes them from command level 9 on.
--
-- api_tokens holds long-lived service tokens of a tenant. Only the SHA-256 of
-- the secret is stored (secret_hash), never the secret itself.
--
-- audit_log holds one entry per applied management command. (raft_index, sub)
-- orders the log: sub is the position within a Batch command, 0 otherwise.
-- audit_log has no foreign key to tenants: entries of the system tenant
-- (_system) and of deleted tenants must be possible.

-- +goose Up
CREATE TABLE api_tokens (
    id              TEXT    NOT NULL PRIMARY KEY,
    tenant_id       TEXT    NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    scope           TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    role            TEXT    NOT NULL,
    -- SHA-256 (hex) of the secret; unique, so a lookup by hash is exact.
    secret_hash     TEXT    NOT NULL UNIQUE,
    created_ms      INTEGER NOT NULL,
    expires_ms      INTEGER NOT NULL,
    created_by_kind TEXT    NOT NULL,
    created_by_id   TEXT    NOT NULL,
    created_by_name TEXT    NOT NULL DEFAULT '',
    UNIQUE (tenant_id, name)
);

CREATE INDEX api_tokens_tenant ON api_tokens (tenant_id);
CREATE INDEX api_tokens_expires ON api_tokens (expires_ms);

CREATE TABLE audit_log (
    raft_index  INTEGER NOT NULL,
    sub         INTEGER NOT NULL,
    at_ms       INTEGER NOT NULL,
    tenant_id   TEXT    NOT NULL,
    actor_kind  TEXT    NOT NULL,
    actor_id    TEXT    NOT NULL DEFAULT '',
    actor_name  TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL,
    object_kind TEXT    NOT NULL DEFAULT '',
    object_id   TEXT    NOT NULL DEFAULT '',
    request_id  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (raft_index, sub)
);

CREATE INDEX audit_log_tenant ON audit_log (tenant_id, raft_index DESC, sub DESC);
CREATE INDEX audit_log_at ON audit_log (at_ms);

-- +goose Down
DROP INDEX audit_log_at;
DROP INDEX audit_log_tenant;
DROP TABLE audit_log;
DROP INDEX api_tokens_expires;
DROP INDEX api_tokens_tenant;
DROP TABLE api_tokens;
