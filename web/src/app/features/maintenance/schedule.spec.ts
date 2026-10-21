// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import {
  buildCron,
  describeCron,
  formatLocalInput,
  isValidCron,
  isValidTimeZone,
  parseLocalInput,
  parseSimpleCron,
  parseTimeOfDay,
  utcToLocalInput,
  zonedWallToUtcMs,
} from './schedule';

describe('parseLocalInput', () => {
  it.each([
    ['2026-03-29T02:30', { year: 2026, month: 3, day: 29, hour: 2, minute: 30 }],
    ['2026-12-31T23:59:00', { year: 2026, month: 12, day: 31, hour: 23, minute: 59 }],
  ])('parses %s', (value, expected) => {
    expect(parseLocalInput(value)).toEqual(expected);
  });

  it.each([
    '',
    '2026-02-31T10:00',
    '2026-13-01T10:00',
    '2026-01-01T24:00',
    '2026-01-01 10:00',
    'nonsense',
  ])('rejects %s', (value) => {
    expect(parseLocalInput(value)).toBeNull();
  });

  it('round-trips through formatLocalInput', () => {
    const w = parseLocalInput('2026-07-04T09:05');
    expect(w && formatLocalInput(w)).toBe('2026-07-04T09:05');
  });
});

describe('zonedWallToUtcMs', () => {
  const at = (value: string, zone: string) => {
    const w = parseLocalInput(value);
    if (!w) {
      throw new Error(value);
    }
    return new Date(zonedWallToUtcMs(w, zone)).toISOString();
  };

  it.each([
    // Europe/Berlin is UTC+1 in winter and UTC+2 in summer.
    ['2026-01-15T12:00', 'Europe/Berlin', '2026-01-15T11:00:00.000Z'],
    ['2026-07-01T10:00', 'Europe/Berlin', '2026-07-01T08:00:00.000Z'],
    ['2026-07-01T10:00', 'UTC', '2026-07-01T10:00:00.000Z'],
    // America/New_York is UTC-5 in winter, UTC-4 in summer.
    ['2026-01-15T12:00', 'America/New_York', '2026-01-15T17:00:00.000Z'],
    ['2026-07-01T10:00', 'America/New_York', '2026-07-01T14:00:00.000Z'],
    // Half-hour offset.
    ['2026-01-15T12:00', 'Asia/Kolkata', '2026-01-15T06:30:00.000Z'],
  ])('%s in %s is %s', (value, zone, utc) => {
    expect(at(value, zone)).toBe(utc);
  });

  it('switches offset at the spring-forward transition', () => {
    // 2026-03-29 01:59 CET is 00:59 UTC; 03:00 CEST is 01:00 UTC.
    expect(at('2026-03-29T01:59', 'Europe/Berlin')).toBe('2026-03-29T00:59:00.000Z');
    expect(at('2026-03-29T03:00', 'Europe/Berlin')).toBe('2026-03-29T01:00:00.000Z');
  });

  it('moves a time inside the spring-forward gap past the gap', () => {
    // 02:30 does not exist on 2026-03-29 in Berlin; it maps to 03:30 CEST.
    expect(at('2026-03-29T02:30', 'Europe/Berlin')).toBe('2026-03-29T01:30:00.000Z');
  });
});

describe('utcToLocalInput', () => {
  it('shows a UTC instant as wall-clock time in the zone', () => {
    expect(utcToLocalInput(Date.parse('2026-07-01T08:00:00Z'), 'Europe/Berlin')).toBe(
      '2026-07-01T10:00',
    );
    expect(utcToLocalInput(Date.parse('2026-07-01T08:00:00Z'), 'UTC')).toBe('2026-07-01T08:00');
  });

  it('is the inverse of zonedWallToUtcMs outside DST gaps', () => {
    for (const value of ['2026-01-15T12:00', '2026-07-01T23:45', '2026-11-02T00:10']) {
      const w = parseLocalInput(value);
      if (!w) {
        throw new Error(value);
      }
      const utc = zonedWallToUtcMs(w, 'Europe/Berlin');
      expect(utcToLocalInput(utc, 'Europe/Berlin')).toBe(value);
    }
  });
});

describe('isValidTimeZone', () => {
  it('accepts IANA names and rejects unknown ones', () => {
    expect(isValidTimeZone('Europe/Berlin')).toBe(true);
    expect(isValidTimeZone('UTC')).toBe(true);
    expect(isValidTimeZone('Mars/Olympus')).toBe(false);
    expect(isValidTimeZone('')).toBe(false);
  });
});

describe('isValidCron', () => {
  it.each([
    '0 3 * * 0',
    '*/15 * * * *',
    '0 9-17 * * 1-5',
    '30 2 1,15 * *',
    '0 0 1 jan-mar *',
    '0 12 * * mon',
    '0 0 * * 7',
    '5/15 * * * *',
    '@daily',
    '@weekly',
    '  0 3 * * 0  ',
  ])('accepts %s', (expr) => {
    expect(isValidCron(expr)).toBe(true);
  });

  it.each([
    '',
    '0 3 * *',
    '0 3 * * * *',
    '60 3 * * *',
    '0 24 * * *',
    '0 0 0 * *',
    '0 0 32 * *',
    '0 0 * 13 *',
    '0 0 * * 8',
    '*/0 * * * *',
    '*/61 * * * *',
    '10-5 * * * *',
    '1-2-3 * * * *',
    '0 0 * * mon-xyz',
    '@sometimes',
    'a b c d e',
    '0,,5 * * * *',
  ])('rejects %s', (expr) => {
    expect(isValidCron(expr)).toBe(false);
  });
});

describe('parseSimpleCron', () => {
  it('recognises the presets', () => {
    expect(parseSimpleCron('30 2 * * *')).toEqual({
      kind: 'daily',
      hour: 2,
      minute: 30,
      weekday: 0,
      dayOfMonth: 1,
    });
    expect(parseSimpleCron('0 3 * * 7')).toMatchObject({
      kind: 'weekly',
      hour: 3,
      minute: 0,
      weekday: 0,
    });
    expect(parseSimpleCron('15 22 13 * *')).toMatchObject({
      kind: 'monthly',
      dayOfMonth: 13,
      hour: 22,
    });
    expect(parseSimpleCron('0 1 * * 1-5')).toMatchObject({ kind: 'weekdays', hour: 1 });
    expect(parseSimpleCron('@daily')).toMatchObject({ kind: 'daily', hour: 0, minute: 0 });
  });

  it('returns null for expressions outside the presets', () => {
    expect(parseSimpleCron('*/15 * * * *')).toBeNull();
    expect(parseSimpleCron('0 9-17 * * *')).toBeNull();
    expect(parseSimpleCron('0 0 1 jan *')).toBeNull();
    expect(parseSimpleCron('0 0 1,15 * *')).toBeNull();
    expect(parseSimpleCron('not cron')).toBeNull();
  });
});

describe('buildCron', () => {
  it.each([
    ['daily', '03:00', 0, 1, '0 3 * * *'],
    ['weekly', '23:45', 6, 1, '45 23 * * 6'],
    ['weekly', '03:00', 0, 1, '0 3 * * 0'],
    ['monthly', '00:05', 0, 31, '5 0 31 * *'],
  ] as const)('%s at %s builds %s', (preset, time, weekday, day, expected) => {
    expect(buildCron(preset, time, weekday, day)).toBe(expected);
  });

  it('returns null for invalid input', () => {
    expect(buildCron('daily', '25:00', 0, 1)).toBeNull();
    expect(buildCron('daily', '3:00', 0, 1)).toBeNull();
    expect(buildCron('weekly', '03:00', 7, 1)).toBeNull();
    expect(buildCron('monthly', '03:00', 0, 0)).toBeNull();
    expect(buildCron('monthly', '03:00', 0, 32)).toBeNull();
  });

  it('produces expressions that pass validation and parse back to the preset', () => {
    const cron = buildCron('weekly', '04:30', 2, 1);
    expect(cron).not.toBeNull();
    expect(isValidCron(cron ?? '')).toBe(true);
    expect(parseSimpleCron(cron ?? '')).toMatchObject({
      kind: 'weekly',
      hour: 4,
      minute: 30,
      weekday: 2,
    });
  });
});

describe('parseTimeOfDay', () => {
  it('parses HH:mm only', () => {
    expect(parseTimeOfDay('07:05')).toEqual({ hour: 7, minute: 5 });
    expect(parseTimeOfDay('7:05')).toBeNull();
    expect(parseTimeOfDay('24:00')).toBeNull();
  });
});

describe('describeCron', () => {
  // Fake translator: returns the key and its parameters, so the test checks the chosen key.
  const translate = (key: string, params: Record<string, string | number>) =>
    `${key}|${Object.entries(params)
      .map(([k, v]) => `${k}=${v}`)
      .join(',')}`;
  const weekday = (n: number) => `wd${n}`;

  it.each([
    ['30 2 * * *', 'maintenance.cron.daily|time=02:30'],
    ['0 3 * * 0', 'maintenance.cron.weekly|day=wd0,time=03:00'],
    ['0 3 * * 7', 'maintenance.cron.weekly|day=wd0,time=03:00'],
    ['15 22 13 * *', 'maintenance.cron.monthly|day=13,time=22:15'],
    ['0 1 * * 1-5', 'maintenance.cron.weekdays|time=01:00'],
    ['@weekly', 'maintenance.cron.weekly|day=wd0,time=00:00'],
  ])('describes %s', (expr, expected) => {
    expect(describeCron(expr, translate, weekday)).toBe(expected);
  });

  it('returns null for patterns it does not know, so the raw expression is shown', () => {
    expect(describeCron('*/5 * * * *', translate, weekday)).toBeNull();
    expect(describeCron('garbage', translate, weekday)).toBeNull();
  });
});
