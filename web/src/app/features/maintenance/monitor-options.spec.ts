// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { Monitor, MonitorPage } from '../../api/gen/model';
import { loadAllMonitors } from './monitor-options';

const monitor = (id: string, name: string): Monitor => ({ id, name }) as unknown as Monitor;

describe('loadAllMonitors', () => {
  it('follows the cursor across pages and sorts by name', async () => {
    const pages: Record<string, MonitorPage> = {
      start: { items: [monitor('2', 'zeta')], nextCursor: 'c1' },
      c1: { items: [monitor('1', 'Alpha')], nextCursor: 'c2' },
      c2: { items: [monitor('3', 'beta')], nextCursor: null },
    };
    const requested: Array<string | undefined> = [];
    const result = await loadAllMonitors(async (cursor) => {
      requested.push(cursor);
      return pages[cursor ?? 'start'];
    });
    expect(requested).toEqual([undefined, 'c1', 'c2']);
    expect(result.map((m) => m.id)).toEqual(['1', '3', '2']);
  });

  it('returns an empty list for a tenant without monitors', async () => {
    expect(await loadAllMonitors(async () => ({ items: [], nextCursor: null }))).toEqual([]);
  });

  it('throws when the cursor repeats instead of looping', async () => {
    await expect(loadAllMonitors(async () => ({ items: [], nextCursor: 'same' }))).rejects.toThrow(
      'cursor repeated',
    );
  });
});
