-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Data encryption keys (DEKs), wrapped with the master key (KEK) (ADR 0011).
-- kek_fingerprint identifies the KEK each DEK is wrapped with, so a voter
-- started with a different KEK refuses to run.

-- +goose Up
CREATE TABLE secret_keys (
    key_id          TEXT    NOT NULL PRIMARY KEY,
    wrapped_dek     BLOB    NOT NULL,
    kek_fingerprint TEXT    NOT NULL,
    created_ms      INTEGER NOT NULL,
    retired_ms      INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE secret_keys;
