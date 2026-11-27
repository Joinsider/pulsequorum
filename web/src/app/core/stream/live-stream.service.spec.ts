// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import { parseEvent, streamUrlFor } from './live-stream.service';

describe('parseEvent', () => {
  it('accepts well-formed events', () => {
    const e = parseEvent(
      '{"type":"state","ts":"2026-10-21T10:00:00Z","monitorId":"m1","status":"DOWN"}',
    );
    expect(e?.type).toBe('state');
    expect(e?.status).toBe('DOWN');
  });

  it.each([
    ['not json'],
    ['null'],
    ['"state"'],
    ['{"type":"other","ts":"x"}'],
    ['{"type":"state"}'],
    [new ArrayBuffer(4)],
  ])('drops %s', (data) => {
    expect(parseEvent(data)).toBeNull();
  });
});

describe('streamUrlFor', () => {
  it.each([
    ['https://n1.example', 'wss://n1.example/api/v1/stream?ticket=t1'],
    ['https://n2.example:8443', 'wss://n2.example:8443/api/v1/stream?ticket=t1'],
    ['http://localhost:8080', 'ws://localhost:8080/api/v1/stream?ticket=t1'],
  ])('pins the socket to %s', (origin, expected) => {
    expect(streamUrlFor(origin, 't1')).toBe(expected);
  });

  it('uses the page origin in same-origin mode', () => {
    expect(streamUrlFor('', 't1')).toBe(
      window.location.origin.replace(/^http/, 'ws') + '/api/v1/stream?ticket=t1',
    );
  });
});
