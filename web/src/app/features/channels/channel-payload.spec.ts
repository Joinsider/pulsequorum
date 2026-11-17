// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { NotificationEvent } from '../../api/gen/model';
import { HeaderRow, buildHeaders, eventFlags, selectedEvents } from './channel-payload';

const added = (key: string, value: string): HeaderRow => ({
  key,
  value,
  serverKey: '',
  serverValue: '',
});

const stored = (key: string, masked: string, value = masked): HeaderRow => ({
  key,
  value,
  serverKey: key,
  serverValue: masked,
});

describe('buildHeaders', () => {
  it('sends an unedited masked value back so the server keeps the secret', () => {
    expect(buildHeaders([stored('Authorization', '***')])).toEqual({
      ok: true,
      headers: { Authorization: '***' },
    });
  });

  it('sends a value the user replaced', () => {
    expect(buildHeaders([stored('Authorization', '***', 'Bearer new')])).toEqual({
      ok: true,
      headers: { Authorization: 'Bearer new' },
    });
  });

  it('does not keep a stored value when the key was renamed', () => {
    const row: HeaderRow = {
      key: 'X-Token',
      value: '***',
      serverKey: 'Authorization',
      serverValue: '***',
    };
    expect(buildHeaders([row])).toEqual({ ok: false, error: 'emptyValue', index: 0 });
  });

  it('trims keys and values and skips fully empty rows', () => {
    expect(buildHeaders([added(' X-A ', ' 1 '), added('', ''), added('X-B', 'two')])).toEqual({
      ok: true,
      headers: { 'X-A': '1', 'X-B': 'two' },
    });
  });

  it('returns undefined when there are no headers', () => {
    expect(buildHeaders([])).toEqual({ ok: true, headers: undefined });
  });

  it('rejects a value without a key', () => {
    expect(buildHeaders([added('', 'orphan')])).toEqual({ ok: false, error: 'emptyKey', index: 0 });
  });

  it('rejects a new key without a value', () => {
    expect(buildHeaders([added('X-A', '1'), added('X-B', ' ')])).toEqual({
      ok: false,
      error: 'emptyValue',
      index: 1,
    });
  });

  it('rejects duplicate keys case-insensitively', () => {
    expect(buildHeaders([added('X-A', '1'), added('x-a', '2')])).toEqual({
      ok: false,
      error: 'duplicateKey',
      index: 1,
    });
  });
});

describe('event flags', () => {
  it('maps events to flags and back in API order', () => {
    const flags = eventFlags([NotificationEvent.up, NotificationEvent.down]);
    expect(flags).toEqual({
      down: true,
      up: true,
      degraded: false,
      partial: false,
      flapping: false,
      channel_failed: false,
    });
    expect(selectedEvents(flags)).toEqual([NotificationEvent.down, NotificationEvent.up]);
  });

  it('selects the flapping and channel-failed events in API order', () => {
    const flags = eventFlags([NotificationEvent.channel_failed, NotificationEvent.flapping]);
    expect(selectedEvents(flags)).toEqual([
      NotificationEvent.flapping,
      NotificationEvent.channel_failed,
    ]);
  });

  it('returns an empty selection when nothing is checked', () => {
    expect(selectedEvents(eventFlags([]))).toEqual([]);
  });
});
