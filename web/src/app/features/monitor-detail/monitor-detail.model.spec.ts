// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { Heartbeat, Incident, UptimeBucket } from '../../api/gen/model';
import {
  LIVE_WINDOW_MS,
  MAX_LIVE_HEARTBEATS,
  appendHeartbeat,
  bucketSeries,
  buildNodeSeries,
  collectPages,
  incidentDuration,
  isNotFound,
  latestPerNode,
  monitorTarget,
  nodeIds,
  splitDuration,
  uptimeQuery,
} from './monitor-detail.model';

const NOW = Date.parse('2026-10-21T12:00:00Z');

function hb(
  nodeId: string,
  minutesAgo: number,
  status: Heartbeat['status'],
  latencyMs: number,
): Heartbeat {
  return {
    nodeId,
    status,
    latencyMs,
    ts: new Date(NOW - minutesAgo * 60_000).toISOString(),
  };
}

describe('buildNodeSeries', () => {
  const beats = [
    hb('node-b', 1, 'UP', 40),
    hb('node-a', 3, 'UP', 30),
    hb('node-a', 1, 'DOWN', 5000),
    hb('node-a', 2, 'DEGRADED', 90),
  ];

  it('groups by node in time order and collects failed checks as markers', () => {
    const series = buildNodeSeries(beats, null);

    expect(series.map((s) => s.nodeId)).toEqual(['node-a', 'node-b']);
    expect(series[0]?.points).toEqual([
      [NOW - 3 * 60_000, 30],
      [NOW - 2 * 60_000, 90],
      [NOW - 60_000, 5000],
    ]);
    expect(series[0]?.failures).toEqual([[NOW - 60_000, 5000]]);
    expect(series[1]?.failures).toEqual([]);
  });

  it('restricts to one node when a filter is given', () => {
    const series = buildNodeSeries(beats, 'node-b');

    expect(series).toHaveLength(1);
    expect(series[0]?.nodeId).toBe('node-b');
  });

  it('returns no series for no heartbeats', () => {
    expect(buildNodeSeries([], null)).toEqual([]);
  });
});

describe('bucketSeries', () => {
  it('uses the average latency per bucket and skips buckets without latency', () => {
    const buckets: UptimeBucket[] = [
      {
        start: '2026-10-21T10:00:00Z',
        upSeconds: 3600,
        downSeconds: 0,
        maintenanceSeconds: 0,
        unknownSeconds: 0,
        latencyAvgMs: 42,
      },
      {
        start: '2026-10-21T11:00:00Z',
        upSeconds: 0,
        downSeconds: 0,
        maintenanceSeconds: 0,
        unknownSeconds: 3600,
        latencyAvgMs: null,
      },
      {
        start: '2026-10-21T12:00:00Z',
        upSeconds: 3600,
        downSeconds: 0,
        maintenanceSeconds: 0,
        unknownSeconds: 0,
      },
    ];

    expect(bucketSeries(buckets)).toEqual([[Date.parse('2026-10-21T10:00:00Z'), 42]]);
  });
});

describe('latestPerNode and nodeIds', () => {
  const beats = [hb('node-b', 1, 'UP', 1), hb('node-a', 5, 'UP', 2), hb('node-a', 1, 'DOWN', 3)];

  it('keeps the newest heartbeat of each node, sorted by node', () => {
    const latest = latestPerNode(beats);

    expect(latest.map((h) => [h.nodeId, h.latencyMs])).toEqual([
      ['node-a', 3],
      ['node-b', 1],
    ]);
  });

  it('lists distinct node ids sorted', () => {
    expect(nodeIds(beats)).toEqual(['node-a', 'node-b']);
  });
});

describe('appendHeartbeat', () => {
  it('appends a new heartbeat in time order', () => {
    const result = appendHeartbeat([hb('a', 5, 'UP', 1)], hb('a', 0, 'UP', 2), NOW);

    expect(result.map((h) => h.latencyMs)).toEqual([1, 2]);
  });

  it('ignores a duplicate of the same node and timestamp', () => {
    const existing = [hb('a', 1, 'UP', 1)];

    expect(appendHeartbeat(existing, hb('a', 1, 'UP', 1), NOW)).toHaveLength(1);
  });

  it('drops heartbeats older than the live window', () => {
    const old = hb('a', 25 * 60, 'UP', 1);
    const result = appendHeartbeat([old], hb('a', 0, 'UP', 2), NOW);

    expect(result.map((h) => h.latencyMs)).toEqual([2]);
    expect(LIVE_WINDOW_MS).toBe(24 * 3600_000);
  });

  it('keeps only the newest heartbeats when the cap is exceeded', () => {
    const full = Array.from({ length: MAX_LIVE_HEARTBEATS }, (_, i) => ({
      nodeId: 'a',
      status: 'UP' as const,
      latencyMs: i,
      ts: new Date(NOW - (MAX_LIVE_HEARTBEATS - i) * 1000).toISOString(),
    }));
    const result = appendHeartbeat(full, hb('a', 0, 'UP', -1), NOW);

    expect(result).toHaveLength(MAX_LIVE_HEARTBEATS);
    expect(result.at(-1)?.latencyMs).toBe(-1);
  });
});

describe('collectPages', () => {
  it('follows cursors until the last page', async () => {
    const pages: Record<string, { items: number[]; nextCursor?: string | null }> = {
      start: { items: [1, 2], nextCursor: 'c1' },
      c1: { items: [3], nextCursor: 'c2' },
      c2: { items: [4], nextCursor: null },
    };
    const seen: Array<string | undefined> = [];

    const items = await collectPages(async (cursor) => {
      seen.push(cursor);
      return pages[cursor ?? 'start'] ?? { items: [] };
    }, 10);

    expect(items).toEqual([1, 2, 3, 4]);
    expect(seen).toEqual([undefined, 'c1', 'c2']);
  });

  it('stops after maxPages even if more pages exist', async () => {
    let calls = 0;
    const items = await collectPages(async () => {
      calls++;
      return { items: [calls], nextCursor: 'more' };
    }, 3);

    expect(calls).toBe(3);
    expect(items).toEqual([1, 2, 3]);
  });
});

describe('splitDuration', () => {
  it.each<[string, number, { unit: string; count: number }]>([
    ['zero', 0, { unit: 'seconds', count: 0 }],
    ['seconds', 45_000, { unit: 'seconds', count: 45 }],
    ['minutes', 10 * 60_000, { unit: 'minutes', count: 10 }],
    ['hours', 5 * 3600_000, { unit: 'hours', count: 5 }],
    ['days', 3 * 86_400_000, { unit: 'days', count: 3 }],
    ['negative clamps to zero', -5_000, { unit: 'seconds', count: 0 }],
  ])('%s', (_name, ms, expected) => {
    expect(splitDuration(ms)).toEqual(expected);
  });
});

describe('incidentDuration', () => {
  const base: Incident = {
    id: 'i1',
    monitorId: 'm1',
    monitorName: 'Web',
    startedAt: '2026-10-21T11:00:00Z',
    cause: 'timeout',
  };

  it('measures to now while the incident is ongoing', () => {
    expect(incidentDuration(base, NOW)).toBe(60 * 60_000);
  });

  it('measures to the resolution when resolved', () => {
    expect(incidentDuration({ ...base, resolvedAt: '2026-10-21T11:30:00Z' }, NOW)).toBe(
      30 * 60_000,
    );
  });

  it('never returns a negative duration', () => {
    expect(incidentDuration({ ...base, resolvedAt: '2026-10-21T10:00:00Z' }, NOW)).toBe(0);
  });
});

describe('monitorTarget', () => {
  it.each<[string, Record<string, unknown> | undefined, string]>([
    ['url', { url: 'https://example.com' }, 'https://example.com'],
    ['host with port', { host: 'db.internal', port: 5432 }, 'db.internal:5432'],
    ['host without port', { host: 'db.internal' }, 'db.internal'],
    ['empty values are skipped', { url: '', hostname: 'x.test' }, 'x.test'],
    ['no config', undefined, ''],
    ['no address field', { method: 'GET' }, ''],
  ])('%s', (_name, config, expected) => {
    expect(monitorTarget(config)).toBe(expected);
  });
});

describe('uptimeQuery and isNotFound', () => {
  it('omits the node for the consensus view', () => {
    expect(uptimeQuery('7d', null)).toEqual({ range: '7d' });
    expect(uptimeQuery('24h', 'node-a')).toEqual({ range: '24h', node: 'node-a' });
  });

  it('recognises 404 responses only', () => {
    expect(isNotFound(new HttpErrorResponse({ status: 404 }))).toBe(true);
    expect(isNotFound(new HttpErrorResponse({ status: 500 }))).toBe(false);
    expect(isNotFound(undefined)).toBe(false);
  });
});
