-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The UptimeCluster Authors
--
-- Secrets in monitor configurations (ADR 0015). secrets_json maps JSON pointers
-- into config_json to ciphertexts of the secret fields (for example
-- "/auth/password"). config_json holds the mask "***" at these positions, so the
-- plaintext never reaches state.db.

-- +goose Up
ALTER TABLE monitors ADD COLUMN secrets_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE monitors DROP COLUMN secrets_json;
