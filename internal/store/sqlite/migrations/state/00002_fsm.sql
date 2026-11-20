-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Raft FSM bookkeeping (ADR 0005 points 2 and 3, ADR 0012 A1/A2). Lives in
-- state.db so that it is written in the same transaction as the data and is
-- part of every snapshot.

-- +goose Up
CREATE TABLE fsm_meta (
    key   TEXT    NOT NULL PRIMARY KEY,
    value INTEGER NOT NULL
);

-- Results of applied commands by request ID, kept 24 h (FA-CLU-06).
CREATE TABLE request_dedup (
    request_id    TEXT    NOT NULL PRIMARY KEY,
    applied_index INTEGER NOT NULL,
    issued_at_ms  INTEGER NOT NULL,
    result_json   TEXT    NOT NULL DEFAULT '',
    error_code    TEXT    NOT NULL DEFAULT '',
    error_message TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX request_dedup_issued ON request_dedup (issued_at_ms);

-- +goose Down
DROP TABLE request_dedup;
DROP TABLE fsm_meta;
