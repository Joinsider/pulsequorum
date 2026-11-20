-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Replication watermark per heartbeat stream (ADR 0012 B2, FA-REP-03). A stream
-- is one (node_id, incarnation) of heartbeats; seq is strictly increasing within
-- it. The position is the highest seq P such that every heartbeat of the stream
-- with seq <= P that still exists anywhere is stored here. Gaps below P are
-- compacted or expired heartbeats, never missing ones.

-- +goose Up
CREATE TABLE stream_positions (
    node_id     TEXT    NOT NULL,
    incarnation TEXT    NOT NULL,
    seq         INTEGER NOT NULL,
    PRIMARY KEY (node_id, incarnation)
) WITHOUT ROWID;

-- Before replication a node only held the streams it produced itself. Their
-- heartbeats are complete in its own view, so the highest stored seq is a valid
-- position. Streams that arrive later are advanced by InsertHeartbeats.
INSERT INTO stream_positions (node_id, incarnation, seq)
SELECT node_id, incarnation, MAX(seq) FROM heartbeats GROUP BY node_id, incarnation;

-- +goose Down
DROP TABLE stream_positions;
