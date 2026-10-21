// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { parseEvent } from './live-stream.service';

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
