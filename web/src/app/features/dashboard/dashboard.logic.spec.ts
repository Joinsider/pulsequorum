// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import {
  applyStreamEvent,
  bulkErrorKey,
  countByStatus,
  filterMonitors,
  formatLatency,
  formatPercent,
  isUnknownMonitorEvent,
  parseTags,
  pruneSelection,
  selectionState,
  sortMonitors,
  statusRank,
  targetOf,
  toggleAll,
} from './dashboard.logic';

function monitor(
  id: string,
  name: string,
  status: MonitorStatus,
  extra: Partial<Monitor> = {},
): Monitor {
  return {
    id,
    name,
    status,
    type: 'http',
    config: { url: `https://${name.toLowerCase()}.example` },
    intervalSeconds: 60,
    statusSince: '2026-10-01T00:00:00Z',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    lastCheckAt: '2026-10-21T10:00:00Z',
    lastLatencyMs: 120,
    uptime24h: 99.5,
    ...extra,
  };
}

describe('sortMonitors', () => {
  it('puts DOWN first, then DEGRADED and PARTIAL, then the rest, each group by name', () => {
    const sorted = sortMonitors([
      monitor('1', 'Zeta', MonitorStatus.UP),
      monitor('2', 'beta', MonitorStatus.DEGRADED),
      monitor('3', 'Alpha', MonitorStatus.DOWN),
      monitor('4', 'gamma', MonitorStatus.PARTIAL),
      monitor('5', 'Delta', MonitorStatus.PAUSED),
      monitor('6', 'Bravo', MonitorStatus.DOWN),
    ]);
    expect(sorted.map((m) => m.name)).toEqual(['Alpha', 'Bravo', 'beta', 'gamma', 'Delta', 'Zeta']);
  });

  it('does not mutate its input', () => {
    const input = [monitor('1', 'B', MonitorStatus.UP), monitor('2', 'A', MonitorStatus.DOWN)];
    const copy = [...input];
    sortMonitors(input);
    expect(input).toEqual(copy);
  });

  it('ranks statuses', () => {
    expect(statusRank(MonitorStatus.DOWN)).toBe(0);
    expect(statusRank(MonitorStatus.PARTIAL)).toBe(1);
    expect(statusRank(MonitorStatus.DEGRADED)).toBe(1);
    expect(statusRank(MonitorStatus.UNKNOWN)).toBe(2);
  });
});

describe('countByStatus', () => {
  it('counts per status and reports zero for absent statuses', () => {
    const counts = countByStatus([
      monitor('1', 'a', MonitorStatus.UP),
      monitor('2', 'b', MonitorStatus.UP),
      monitor('3', 'c', MonitorStatus.DOWN),
    ]);
    expect(counts.UP).toBe(2);
    expect(counts.DOWN).toBe(1);
    expect(counts.PAUSED).toBe(0);
  });
});

describe('filterMonitors', () => {
  const list = [
    monitor('1', 'Shop API', MonitorStatus.UP),
    monitor('2', 'shop web', MonitorStatus.DOWN),
    monitor('3', 'Mail', MonitorStatus.DOWN),
  ];

  it('keeps everything without filters', () => {
    expect(filterMonitors(list, null, '')).toHaveLength(3);
  });

  it('filters by status', () => {
    expect(filterMonitors(list, MonitorStatus.DOWN, '').map((m) => m.id)).toEqual(['2', '3']);
  });

  it('searches names case-insensitively and ignores surrounding spaces', () => {
    expect(filterMonitors(list, null, '  SHOP ').map((m) => m.id)).toEqual(['1', '2']);
  });

  it('combines status and search', () => {
    expect(filterMonitors(list, MonitorStatus.DOWN, 'shop').map((m) => m.id)).toEqual(['2']);
  });
});

describe('targetOf', () => {
  it.each([
    [{ url: 'https://example.org' }, 'https://example.org'],
    [{ host: 'db.internal', port: 5432 }, 'db.internal:5432'],
    [{ hostname: 'ns.example', port: '53' }, 'ns.example:53'],
    [{ host: 'icmp.example' }, 'icmp.example'],
    [{ url: '  ' }, null],
    [{ url: 42 }, null],
    [{ other: 'x' }, null],
    [null, null],
    ['https://example.org', null],
    [undefined, null],
  ])('derives the target of %j', (config, expected) => {
    expect(targetOf(config)).toBe(expected);
  });
});

describe('formatting', () => {
  it('formats percentages with two decimals or a dash', () => {
    expect(formatPercent(99.5)).toBe('99.50 %');
    expect(formatPercent(100)).toBe('100.00 %');
    expect(formatPercent(null)).toBe('–');
    expect(formatPercent(Number.NaN)).toBe('–');
  });

  it('formats latency as whole milliseconds or a dash', () => {
    expect(formatLatency(120.6)).toBe('121');
    expect(formatLatency(null)).toBe('–');
  });
});

describe('applyStreamEvent', () => {
  const list = [monitor('m1', 'A', MonitorStatus.UP), monitor('m2', 'B', MonitorStatus.UP)];

  it('updates status and statusSince on state events', () => {
    const event: StreamEvent = {
      type: 'state',
      ts: '2026-10-21T11:00:00Z',
      monitorId: 'm2',
      status: MonitorStatus.DOWN,
    };
    const next = applyStreamEvent(list, event);
    expect(next[1]).toMatchObject({ status: 'DOWN', statusSince: '2026-10-21T11:00:00Z' });
    expect(next[0]).toBe(list[0]);
    expect(list[1].status).toBe('UP');
  });

  it('updates lastCheckAt and lastLatencyMs on heartbeat events', () => {
    const next = applyStreamEvent(list, {
      type: 'heartbeat',
      ts: '2026-10-21T11:00:00Z',
      monitorId: 'm1',
      heartbeat: {
        nodeId: 'n1',
        ts: '2026-10-21T11:00:00Z',
        status: 'UP',
        latencyMs: 42,
      },
    });
    expect(next[0]).toMatchObject({ lastCheckAt: '2026-10-21T11:00:00Z', lastLatencyMs: 42 });
  });

  it('ignores older heartbeats than the one already shown', () => {
    const next = applyStreamEvent(list, {
      type: 'heartbeat',
      ts: '2026-10-21T09:00:00Z',
      monitorId: 'm1',
      heartbeat: { nodeId: 'n1', ts: '2026-10-21T09:00:00Z', status: 'UP', latencyMs: 1 },
    });
    expect(next).toBe(list);
  });

  it('returns the same array for unknown monitors, incidents and no-op states', () => {
    expect(
      applyStreamEvent(list, {
        type: 'state',
        ts: '2026-10-21T11:00:00Z',
        monitorId: 'zz',
        status: MonitorStatus.DOWN,
      }),
    ).toBe(list);
    expect(
      applyStreamEvent(list, { type: 'incident', ts: '2026-10-21T11:00:00Z', monitorId: 'm1' }),
    ).toBe(list);
    expect(
      applyStreamEvent(list, {
        type: 'state',
        ts: '2026-10-21T11:00:00Z',
        monitorId: 'm1',
        status: MonitorStatus.UP,
      }),
    ).toBe(list);
  });

  it('detects events for unknown monitors', () => {
    expect(isUnknownMonitorEvent(list, { type: 'state', ts: 'x', monitorId: 'zz' })).toBe(true);
    expect(isUnknownMonitorEvent(list, { type: 'state', ts: 'x', monitorId: 'm1' })).toBe(false);
    expect(isUnknownMonitorEvent(list, { type: 'node', ts: 'x' })).toBe(false);
  });
});

describe('selectionState', () => {
  const visible = ['a', 'b', 'c'];
  it.each([
    { name: 'nothing visible', selected: ['a'], ids: [], expected: 'none' },
    { name: 'nothing selected', selected: [], ids: visible, expected: 'none' },
    { name: 'selection outside the visible list', selected: ['z'], ids: visible, expected: 'none' },
    { name: 'some visible selected', selected: ['a'], ids: visible, expected: 'some' },
    { name: 'all visible selected', selected: ['a', 'b', 'c'], ids: visible, expected: 'all' },
    {
      name: 'all visible plus hidden',
      selected: ['a', 'b', 'c', 'z'],
      ids: visible,
      expected: 'all',
    },
  ])('$name -> $expected', ({ selected, ids, expected }) => {
    expect(selectionState(new Set(selected), ids)).toBe(expected);
  });
});

describe('toggleAll', () => {
  it('selects all visible monitors and keeps hidden selections', () => {
    const next = toggleAll(new Set(['z']), ['a', 'b']);
    expect([...next].sort()).toEqual(['a', 'b', 'z']);
  });

  it('selects all visible monitors when only some are selected', () => {
    const next = toggleAll(new Set(['a']), ['a', 'b', 'c']);
    expect([...next].sort()).toEqual(['a', 'b', 'c']);
  });

  it('clears the visible monitors when all are selected, keeping hidden ones', () => {
    const next = toggleAll(new Set(['a', 'b', 'z']), ['a', 'b']);
    expect([...next]).toEqual(['z']);
  });

  it('returns a new set and does not mutate its input', () => {
    const input = new Set(['a']);
    const next = toggleAll(input, ['a', 'b']);
    expect(next).not.toBe(input);
    expect([...input]).toEqual(['a']);
  });

  it('does nothing when there are no visible monitors', () => {
    expect([...toggleAll(new Set(['a']), [])]).toEqual(['a']);
  });
});

describe('pruneSelection', () => {
  it('drops IDs that are no longer in the list', () => {
    const selected = new Set(['a', 'b', 'c']);
    expect([...pruneSelection(selected, ['a', 'c', 'x'])]).toEqual(['a', 'c']);
  });

  it('returns the same set when nothing was dropped', () => {
    const selected = new Set(['a']);
    expect(pruneSelection(selected, ['a', 'b'])).toBe(selected);
  });

  it('empties the selection when no monitor is left', () => {
    expect(pruneSelection(new Set(['a']), []).size).toBe(0);
  });
});

describe('parseTags', () => {
  it.each([
    { name: 'single tag', input: 'prod', tags: ['prod'] },
    { name: 'comma separated with spaces', input: ' prod , eu-west ', tags: ['prod', 'eu-west'] },
    { name: 'empty entries are skipped', input: 'a,,b,', tags: ['a', 'b'] },
    { name: 'duplicates are removed', input: 'a, b, a', tags: ['a', 'b'] },
    { name: 'exactly 64 characters', input: 'x'.repeat(64), tags: ['x'.repeat(64)] },
    {
      name: 'exactly 20 tags',
      input: Array.from({ length: 20 }, (_, i) => `t${i}`).join(','),
      tags: Array.from({ length: 20 }, (_, i) => `t${i}`),
    },
    {
      name: 'duplicates do not count towards the limit',
      input: 'a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a,a',
      tags: ['a'],
    },
  ])('accepts $name', ({ input, tags }) => {
    expect(parseTags(input)).toEqual({ tags });
  });

  it.each([
    { name: 'empty input', input: '', error: 'dashboard.bulk.tags.empty' },
    { name: 'only separators', input: ' , , ', error: 'dashboard.bulk.tags.empty' },
    { name: 'tag of 65 characters', input: 'x'.repeat(65), error: 'dashboard.bulk.tags.tooLong' },
    {
      name: 'more than 20 tags',
      input: Array.from({ length: 21 }, (_, i) => `t${i}`).join(','),
      error: 'dashboard.bulk.tags.tooMany',
    },
  ])('rejects $name', ({ input, error }) => {
    expect(parseTags(input)).toEqual({ error });
  });

  it('counts characters, not UTF-16 units', () => {
    expect(parseTags('ä'.repeat(64))).toEqual({ tags: ['ä'.repeat(64)] });
    expect(parseTags('😀'.repeat(65))).toEqual({ error: 'dashboard.bulk.tags.tooLong' });
  });
});

describe('bulkErrorKey', () => {
  function httpError(status: number, body: unknown = null): HttpErrorResponse {
    return new HttpErrorResponse({ error: body, status, url: '/api/v1/monitors/bulk' });
  }

  it.each([
    { name: '403 forbidden', err: httpError(403), key: 'dashboard.bulk.errors.forbidden' },
    { name: '409 conflict', err: httpError(409), key: 'dashboard.bulk.errors.conflict' },
    { name: '503 unavailable', err: httpError(503), key: 'dashboard.bulk.errors.unavailable' },
    {
      name: '422 with problem body',
      err: httpError(422, {
        type: 'about:blank',
        title: 'Validation failed',
        status: 422,
        errors: [{ field: '/ids/3', message: 'unknown monitor' }],
      }),
      key: 'dashboard.bulk.errors.invalid',
    },
    { name: '500 generic', err: httpError(500), key: 'dashboard.bulk.errors.generic' },
    { name: 'network error', err: httpError(0), key: 'dashboard.bulk.errors.generic' },
    { name: 'non-HTTP error', err: new Error('boom'), key: 'dashboard.bulk.errors.generic' },
  ])('maps $name', ({ err, key }) => {
    expect(bulkErrorKey(err)).toBe(key);
  });
});
