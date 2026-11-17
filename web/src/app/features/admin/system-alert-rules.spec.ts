// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { detailRows } from './system-alert-rules';

describe('detailRows', () => {
  it('returns no rows without details', () => {
    expect(detailRows(undefined)).toEqual([]);
    expect(detailRows(null)).toEqual([]);
    expect(detailRows({})).toEqual([]);
  });

  it('sorts the keys and formats the values as text', () => {
    expect(
      detailRows({ offsetMs: 2500, node: 'n2', healthy: false, missing: null, nested: { a: 1 } }),
    ).toEqual([
      { key: 'healthy', value: 'false' },
      { key: 'missing', value: '-' },
      { key: 'nested', value: '{"a":1}' },
      { key: 'node', value: 'n2' },
      { key: 'offsetMs', value: '2500' },
    ]);
  });
});
