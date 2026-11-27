-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Public URL of voters (ADR 0036). The FSM writes it from command level 11 on;
-- below that level it stays empty on every node. Probes never have one.

-- +goose Up
ALTER TABLE nodes ADD COLUMN public_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE nodes DROP COLUMN public_url;
