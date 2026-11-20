-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 The Pulsequorum Authors
--
-- Flapping state of a monitor (FA-ALR-06, ADR 0023). The FSM sets it from
-- command level 4 on; flapping_since_ms is the time the period started.
--
-- Existing channels receive the two new events, so that they get the flapping
-- and channel-failed alerts without being edited (ADR 0023, 3d).

-- +goose Up
ALTER TABLE monitor_states ADD COLUMN flapping          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE monitor_states ADD COLUMN flapping_since_ms INTEGER NOT NULL DEFAULT 0;

UPDATE notification_channels
SET events_json = json_insert(events_json, '$[#]', 'flapping')
WHERE NOT EXISTS (SELECT 1 FROM json_each(notification_channels.events_json) WHERE value = 'flapping');

UPDATE notification_channels
SET events_json = json_insert(events_json, '$[#]', 'channel_failed')
WHERE NOT EXISTS (SELECT 1 FROM json_each(notification_channels.events_json) WHERE value = 'channel_failed');

-- +goose Down
UPDATE notification_channels
SET events_json = (
    SELECT json_group_array(e.value) FROM (
        SELECT value FROM json_each(notification_channels.events_json)
        WHERE value NOT IN ('flapping', 'channel_failed')
        ORDER BY key) AS e);

ALTER TABLE monitor_states DROP COLUMN flapping_since_ms;
ALTER TABLE monitor_states DROP COLUMN flapping;
