// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { describe, expect, it } from 'vitest';
import { MaintenanceWindow, Problem } from '../../api/gen/model';
import {
  emptyWindowValue,
  flattenFieldPointers,
  fromMaintenanceWindow,
  onceRangeError,
  presetCron,
  toMaintenanceInput,
  WindowFormValue,
} from './window-form';

const base: WindowFormValue = {
  ...emptyWindowValue('Europe/Berlin'),
  name: '  Database upgrade  ',
  description: ' weekly patch ',
  monitorIds: ['b2d1', 'a1c3'],
  allMonitors: false,
  kind: 'once',
  startsAt: '2026-07-01T10:00',
  endsAt: '2026-07-01T12:30',
};

describe('toMaintenanceInput', () => {
  it('converts a one-time window from its zone to UTC instants', () => {
    expect(toMaintenanceInput(base)).toEqual({
      name: 'Database upgrade',
      description: 'weekly patch',
      monitorIds: ['b2d1', 'a1c3'],
      active: true,
      schedule: {
        kind: 'once',
        startsAt: '2026-07-01T08:00:00.000Z',
        endsAt: '2026-07-01T10:30:00.000Z',
        timezone: 'Europe/Berlin',
      },
    });
  });

  it('sends an empty monitor list when all monitors are selected', () => {
    expect(toMaintenanceInput({ ...base, allMonitors: true }).monitorIds).toEqual([]);
  });

  it('sends cron, duration and zone for recurring windows', () => {
    const input = toMaintenanceInput({
      ...base,
      kind: 'recurring',
      cron: ' 0 3 * * 0 ',
      durationMinutes: 120,
      timezone: 'UTC',
    });
    expect(input.schedule).toEqual({
      kind: 'recurring',
      cron: '0 3 * * 0',
      durationMinutes: 120,
      timezone: 'UTC',
    });
  });

  it('keeps recurring bounds from the loaded window, which the editor does not edit', () => {
    const input = toMaintenanceInput(
      { ...base, kind: 'recurring', cron: '0 3 * * 0', durationMinutes: 60, timezone: 'UTC' },
      { kind: 'recurring', startsAt: '2026-01-01T00:00:00Z', endsAt: '2027-01-01T00:00:00Z' },
    );
    expect(input.schedule).toMatchObject({
      startsAt: '2026-01-01T00:00:00Z',
      endsAt: '2027-01-01T00:00:00Z',
    });
  });

  it('does not carry once bounds into a recurring schedule', () => {
    const input = toMaintenanceInput(
      { ...base, kind: 'recurring', cron: '0 3 * * 0', durationMinutes: 60 },
      { kind: 'once', startsAt: '2026-01-01T00:00:00Z', endsAt: '2026-01-02T00:00:00Z' },
    );
    expect(input.schedule).not.toHaveProperty('startsAt');
    expect(input.schedule).not.toHaveProperty('endsAt');
  });

  it('throws for a recurring window without duration', () => {
    expect(() =>
      toMaintenanceInput({ ...base, kind: 'recurring', cron: '0 3 * * 0', durationMinutes: null }),
    ).toThrow();
  });
});

describe('fromMaintenanceWindow', () => {
  const window = (
    schedule: MaintenanceWindow['schedule'],
    monitorIds: string[] = [],
  ): MaintenanceWindow => ({
    id: '0190-x',
    name: 'Patch',
    description: 'd',
    active: false,
    monitorIds,
    schedule,
    createdAt: '2026-01-01T00:00:00Z',
  });

  it('shows one-time windows in their own zone', () => {
    const v = fromMaintenanceWindow(
      window({
        kind: 'once',
        startsAt: '2026-07-01T08:00:00Z',
        endsAt: '2026-07-01T10:30:00Z',
        timezone: 'Europe/Berlin',
      }),
      'UTC',
    );
    expect(v).toMatchObject({
      kind: 'once',
      startsAt: '2026-07-01T10:00',
      endsAt: '2026-07-01T12:30',
      timezone: 'Europe/Berlin',
      active: false,
      allMonitors: true,
      description: 'd',
    });
  });

  it('recognises presets in recurring cron expressions', () => {
    const v = fromMaintenanceWindow(
      window(
        { kind: 'recurring', cron: '45 23 * * 6', durationMinutes: 90, timezone: 'Asia/Tokyo' },
        ['a'],
      ),
      'UTC',
    );
    expect(v).toMatchObject({
      kind: 'recurring',
      preset: 'weekly',
      time: '23:45',
      weekday: 6,
      cron: '45 23 * * 6',
      durationMinutes: 90,
      timezone: 'Asia/Tokyo',
      allMonitors: false,
      monitorIds: ['a'],
    });
  });

  it('uses custom for other cron expressions and UTC when no zone is stored', () => {
    const v = fromMaintenanceWindow(
      window({ kind: 'recurring', cron: '*/10 * * * *', durationMinutes: 5 }),
      'UTC',
    );
    expect(v).toMatchObject({ preset: 'custom', cron: '*/10 * * * *', timezone: 'UTC' });
  });

  it('round-trips through toMaintenanceInput', () => {
    const schedule = {
      kind: 'once' as const,
      startsAt: '2026-10-25T01:30:00.000Z',
      endsAt: '2026-10-25T03:00:00.000Z',
      timezone: 'Europe/Berlin',
    };
    const input = toMaintenanceInput(fromMaintenanceWindow(window(schedule), 'UTC'));
    expect(input.schedule).toEqual(schedule);
  });
});

describe('onceRangeError', () => {
  it('requires both values', () => {
    expect(onceRangeError('', '2026-07-01T10:00', 'UTC')).toBe('required');
  });

  it('rejects an end that is not after the start, in the chosen zone', () => {
    expect(onceRangeError('2026-07-01T10:00', '2026-07-01T10:00', 'UTC')).toBe('order');
    expect(onceRangeError('2026-07-01T10:00', '2026-07-01T09:00', 'UTC')).toBe('order');
    expect(onceRangeError('2026-07-01T10:00', '2026-07-01T11:00', 'UTC')).toBeNull();
  });

  it('flags invalid input', () => {
    expect(onceRangeError('2026-02-30T10:00', '2026-03-01T10:00', 'UTC')).toBe('invalid');
    expect(onceRangeError('2026-07-01T10:00', '2026-07-01T11:00', 'Mars/Base')).toBe('invalid');
  });
});

describe('presetCron', () => {
  it('builds the cron of a preset and nothing for custom', () => {
    expect(presetCron({ preset: 'daily', time: '02:15', weekday: 0, dayOfMonth: 1 })).toBe(
      '15 2 * * *',
    );
    expect(presetCron({ preset: 'monthly', time: '02:15', weekday: 0, dayOfMonth: 9 })).toBe(
      '15 2 9 * *',
    );
    expect(presetCron({ preset: 'custom', time: '02:15', weekday: 0, dayOfMonth: 9 })).toBeNull();
  });
});

describe('flattenFieldPointers', () => {
  it('strips the schedule prefix so that field errors address the flat form', () => {
    const problem: Problem = {
      type: 'about:blank',
      title: 'Validation Failed',
      status: 422,
      errors: [
        { field: '/schedule/cron', message: 'bad' },
        { field: '/name', message: 'too long' },
        { field: '/monitorIds', message: 'unknown monitor' },
      ],
    };
    expect(flattenFieldPointers(problem).errors).toEqual([
      { field: '/cron', message: 'bad' },
      { field: '/name', message: 'too long' },
      { field: '/monitorIds', message: 'unknown monitor' },
    ]);
  });
});
