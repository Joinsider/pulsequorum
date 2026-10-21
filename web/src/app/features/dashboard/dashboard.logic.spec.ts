// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import {
  applyStreamEvent,
  countByStatus,
  filterMonitors,
  formatLatency,
  formatPercent,
  isUnknownMonitorEvent,
  sortMonitors,
  statusRank,
  targetOf,
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
