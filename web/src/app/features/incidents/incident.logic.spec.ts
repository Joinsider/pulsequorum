// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { Incident } from '../../api/gen/model';
import {
  DEFAULT_FILTERS,
  IncidentFilters,
  durationParts,
  formatAbsolute,
  incidentDurationMs,
  matchesFilters,
  mergeIncidents,
  parseFilters,
  sortNewestFirst,
  toApiParams,
  toQueryParams,
  upsertIncident,
} from './incident.logic';

const NOW = Date.parse('2026-10-21T12:00:00Z');
const MON_A = '11111111-1111-4111-8111-111111111111';
const MON_B = '22222222-2222-4222-8222-222222222222';
const HOUR = 3_600_000;

function incident(id: string, overrides: Partial<Incident> = {}): Incident {
  return {
    id,
    monitorId: MON_A,
    monitorName: 'web',
    startedAt: new Date(NOW - HOUR).toISOString(),
    cause: 'connection refused',
    ...overrides,
  };
}

/** Map-like accessor for parseFilters, mirroring Angular's ParamMap.get. */
function params(values: Record<string, string>): { get(name: string): string | null } {
  return { get: (name) => values[name] ?? null };
}

const ALL: IncidentFilters = DEFAULT_FILTERS;

describe('parseFilters', () => {
  it('uses the defaults for an empty URL', () => {
    expect(parseFilters(params({}))).toEqual(DEFAULT_FILTERS);
  });

  it('reads valid values', () => {
    expect(parseFilters(params({ state: 'ongoing', range: '7d', monitor: MON_B }))).toEqual({
      state: 'ongoing',
      range: '7d',
      monitorId: MON_B,
    });
  });

  it.each([
    ['state', { state: 'open' }],
    ['range', { range: '1y' }],
    ['monitor', { monitor: 'not-a-uuid' }],
  ])('falls back to the default for an invalid %s', (_name, values) => {
    expect(parseFilters(params(values))).toEqual(DEFAULT_FILTERS);
  });
});

describe('toQueryParams', () => {
  it('drops defaults so the URL stays clean', () => {
    expect(toQueryParams(DEFAULT_FILTERS)).toEqual({ state: null, range: null, monitor: null });
  });

  it('round-trips through parseFilters', () => {
    const filters: IncidentFilters = { state: 'resolved', range: '30d', monitorId: MON_A };
    expect(parseFilters(params(toQueryParams(filters) as Record<string, string>))).toEqual(filters);
  });
});

describe('toApiParams', () => {
  it('sends no restriction for the default filters', () => {
    const p = toApiParams(ALL, NOW);
    expect(p.open).toBeUndefined();
    expect(p.monitorId).toBeUndefined();
    expect(p.from).toBeUndefined();
  });

  it('maps ongoing to open=true; resolved stays client-side', () => {
    expect(toApiParams({ ...ALL, state: 'ongoing' }, NOW).open).toBe(true);
    expect(toApiParams({ ...ALL, state: 'resolved' }, NOW).open).toBeUndefined();
  });

  it('computes from for a relative time range', () => {
    expect(toApiParams({ ...ALL, range: '24h' }, NOW).from).toBe('2026-10-20T12:00:00.000Z');
  });

  it('passes the monitor id', () => {
    expect(toApiParams({ ...ALL, monitorId: MON_B }, NOW).monitorId).toBe(MON_B);
  });
});

describe('matchesFilters', () => {
  it('matches everything with the default filters', () => {
    expect(matchesFilters(incident('a'), ALL, NOW)).toBe(true);
    expect(matchesFilters(incident('b', { resolvedAt: '2026-10-21T11:00:00Z' }), ALL, NOW)).toBe(
      true,
    );
  });

  it('separates ongoing and resolved incidents', () => {
    const ongoing = incident('a');
    const resolved = incident('b', { resolvedAt: '2026-10-21T11:00:00Z' });
    expect(matchesFilters(ongoing, { ...ALL, state: 'ongoing' }, NOW)).toBe(true);
    expect(matchesFilters(resolved, { ...ALL, state: 'ongoing' }, NOW)).toBe(false);
    expect(matchesFilters(ongoing, { ...ALL, state: 'resolved' }, NOW)).toBe(false);
    expect(matchesFilters(resolved, { ...ALL, state: 'resolved' }, NOW)).toBe(true);
  });

  it('compares monitor ids case-insensitively', () => {
    const inc = incident('a', { monitorId: MON_A.toUpperCase() });
    expect(matchesFilters(inc, { ...ALL, monitorId: MON_A }, NOW)).toBe(true);
    expect(matchesFilters(incident('b'), { ...ALL, monitorId: MON_B }, NOW)).toBe(false);
  });

  it('excludes incidents started before the time range', () => {
    const old = incident('a', { startedAt: new Date(NOW - 2 * 24 * HOUR).toISOString() });
    expect(matchesFilters(old, { ...ALL, range: '24h' }, NOW)).toBe(false);
    expect(matchesFilters(old, { ...ALL, range: '7d' }, NOW)).toBe(true);
  });
});

describe('upsertIncident', () => {
  it('inserts a matching incident at its sorted position', () => {
    const older = incident('old', { startedAt: new Date(NOW - 5 * HOUR).toISOString() });
    const list = [incident('new', { startedAt: new Date(NOW - 1000).toISOString() }), older];
    const result = upsertIncident(list, incident('mid'), ALL, NOW);
    expect(result.map((i) => i.id)).toEqual(['new', 'mid', 'old']);
  });

  it('replaces an incident with the same id', () => {
    const list = [incident('a')];
    const resolved = incident('a', { resolvedAt: '2026-10-21T11:30:00Z' });
    const result = upsertIncident(list, resolved, ALL, NOW);
    expect(result).toHaveLength(1);
    expect(result[0].resolvedAt).toBe('2026-10-21T11:30:00Z');
  });

  it('removes an incident that no longer matches the filters', () => {
    const list = [incident('a')];
    const resolved = incident('a', { resolvedAt: '2026-10-21T11:30:00Z' });
    expect(upsertIncident(list, resolved, { ...ALL, state: 'ongoing' }, NOW)).toEqual([]);
  });

  it('ignores a non-matching incident that is not in the list', () => {
    const list = [incident('a')];
    const other = incident('b', { monitorId: MON_B });
    expect(upsertIncident(list, other, { ...ALL, monitorId: MON_A }, NOW)).toEqual(list);
  });
});

describe('mergeIncidents and sortNewestFirst', () => {
  it('removes duplicates and sorts newest first', () => {
    const a = incident('a', { startedAt: new Date(NOW - 3 * HOUR).toISOString() });
    const b = incident('b', { startedAt: new Date(NOW - HOUR).toISOString() });
    const merged = mergeIncidents([a], [b, a]);
    expect(merged.map((i) => i.id)).toEqual(['b', 'a']);
  });

  it('orders equal start times by id', () => {
    const same = new Date(NOW).toISOString();
    expect(
      sortNewestFirst([incident('b', { startedAt: same }), incident('a', { startedAt: same })]).map(
        (i) => i.id,
      ),
    ).toEqual(['a', 'b']);
  });
});

describe('incidentDurationMs', () => {
  it('uses the resolution time when resolved', () => {
    const inc = incident('a', { resolvedAt: new Date(NOW - 30 * 60_000).toISOString() });
    expect(incidentDurationMs(inc, NOW)).toBe(30 * 60_000);
  });

  it('runs until now while ongoing', () => {
    expect(incidentDurationMs(incident('a'), NOW)).toBe(HOUR);
  });

  it('never returns a negative duration', () => {
    expect(
      incidentDurationMs(incident('a', { startedAt: new Date(NOW + 5000).toISOString() }), NOW),
    ).toBe(0);
  });
});

describe('durationParts', () => {
  const MIN = 60_000;
  it.each<[string, number, ReturnType<typeof durationParts>]>([
    ['zero', 0, [{ unit: 's', value: 0 }]],
    ['seconds', 45_000, [{ unit: 's', value: 45 }]],
    [
      'minutes and seconds',
      5 * MIN + 3_000,
      [
        { unit: 'min', value: 5 },
        { unit: 's', value: 3 },
      ],
    ],
    ['exact minutes drops seconds', 7 * MIN, [{ unit: 'min', value: 7 }]],
    [
      'hours and minutes',
      2 * HOUR + 5 * MIN,
      [
        { unit: 'h', value: 2 },
        { unit: 'min', value: 5 },
      ],
    ],
    ['exact hours drops minutes', 2 * HOUR, [{ unit: 'h', value: 2 }]],
    [
      'days and hours',
      3 * 24 * HOUR + 4 * HOUR + 30 * MIN,
      [
        { unit: 'd', value: 3 },
        { unit: 'h', value: 4 },
      ],
    ],
    ['exact days', 24 * HOUR, [{ unit: 'd', value: 1 }]],
    ['negative is zero', -5_000, [{ unit: 's', value: 0 }]],
  ])('formats %s', (_name, ms, expected) => {
    expect(durationParts(ms)).toEqual(expected);
  });
});

describe('formatAbsolute', () => {
  it('returns an empty string for an invalid timestamp', () => {
    expect(formatAbsolute('not a date', 'en')).toBe('');
  });

  it('returns a non-empty date for a valid timestamp', () => {
    expect(formatAbsolute('2026-10-21T12:00:00Z', 'en').length).toBeGreaterThan(0);
  });
});
