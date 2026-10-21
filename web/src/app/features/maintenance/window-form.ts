// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { AbstractControl, ValidationErrors, ValidatorFn } from '@angular/forms';
import {
  MaintenanceSchedule,
  MaintenanceWindow,
  MaintenanceWindowInput,
  Problem,
} from '../../api/gen/model';
import {
  CronPreset,
  buildCron,
  isValidCron,
  isValidTimeZone,
  parseLocalInput,
  parseSimpleCron,
  parseTimeOfDay,
  formatTimeOfDay,
  utcToLocalInput,
  zonedWallToUtcMs,
} from './schedule';

/** Maximum window length in minutes (FA-MNT-01, same as the server). */
export const MAX_DURATION_MINUTES = 10080;

export type WindowKind = 'once' | 'recurring';
export type PresetChoice = 'custom' | CronPreset;

/** Flat form model of the editor. Times are datetime-local strings in `timezone`. */
export interface WindowFormValue {
  name: string;
  description: string;
  active: boolean;
  allMonitors: boolean;
  monitorIds: string[];
  kind: WindowKind;
  startsAt: string;
  endsAt: string;
  timezone: string;
  cron: string;
  durationMinutes: number | null;
  preset: PresetChoice;
  time: string;
  weekday: number;
  dayOfMonth: number;
}

/** Initial values of a new window in the given time zone. */
export function emptyWindowValue(timezone: string): WindowFormValue {
  return {
    name: '',
    description: '',
    active: true,
    allMonitors: true,
    monitorIds: [],
    kind: 'once',
    startsAt: '',
    endsAt: '',
    timezone,
    cron: '0 3 * * 0',
    durationMinutes: 60,
    preset: 'custom',
    time: '03:00',
    weekday: 0,
    dayOfMonth: 1,
  };
}

/** Converts a datetime-local value in a zone to a UTC RFC 3339 timestamp. */
export function localToIso(local: string, timezone: string): string {
  const wall = parseLocalInput(local);
  if (!wall || !isValidTimeZone(timezone)) {
    throw new Error(`invalid local time ${local} in ${timezone}`);
  }
  return new Date(zonedWallToUtcMs(wall, timezone)).toISOString();
}

/**
 * Builds the API body from the form. `original` is the schedule as loaded:
 * recurring bounds (startsAt/endsAt) are not editable in the UI, so they are
 * kept, not dropped by an update.
 */
export function toMaintenanceInput(
  v: WindowFormValue,
  original?: MaintenanceSchedule | null,
): MaintenanceWindowInput {
  const timezone = v.timezone.trim();
  const monitorIds = v.allMonitors ? [] : [...v.monitorIds];
  const base = {
    name: v.name.trim(),
    description: v.description.trim(),
    monitorIds,
    active: v.active,
  };
  if (v.kind === 'once') {
    return {
      ...base,
      schedule: {
        kind: 'once',
        startsAt: localToIso(v.startsAt, timezone),
        endsAt: localToIso(v.endsAt, timezone),
        timezone,
      },
    };
  }
  if (v.durationMinutes === null) {
    throw new Error('durationMinutes is required for a recurring window');
  }
  const schedule: MaintenanceSchedule = {
    kind: 'recurring',
    cron: v.cron.trim(),
    durationMinutes: v.durationMinutes,
    timezone,
  };
  if (original?.kind === 'recurring') {
    if (original.startsAt) {
      schedule.startsAt = original.startsAt;
    }
    if (original.endsAt) {
      schedule.endsAt = original.endsAt;
    }
  }
  return { ...base, schedule };
}

/** Converts a stored window to form values, shown in its own time zone. */
export function fromMaintenanceWindow(w: MaintenanceWindow, browserZone: string): WindowFormValue {
  const s = w.schedule;
  const recurring = s.kind === 'recurring';
  const timezone = s.timezone || (recurring ? 'UTC' : browserZone);
  const base = emptyWindowValue(timezone);
  const simple = recurring && s.cron ? parseSimpleCron(s.cron) : null;
  const preset: PresetChoice = simple && simple.kind !== 'weekdays' ? simple.kind : 'custom';
  return {
    ...base,
    name: w.name,
    description: w.description ?? '',
    active: w.active ?? true,
    allMonitors: w.monitorIds.length === 0,
    monitorIds: [...w.monitorIds],
    kind: recurring ? 'recurring' : 'once',
    startsAt: !recurring && s.startsAt ? utcToLocalInput(Date.parse(s.startsAt), timezone) : '',
    endsAt: !recurring && s.endsAt ? utcToLocalInput(Date.parse(s.endsAt), timezone) : '',
    cron: recurring ? (s.cron ?? '') : base.cron,
    durationMinutes: recurring ? (s.durationMinutes ?? null) : base.durationMinutes,
    preset,
    time: simple ? formatTimeOfDay(simple.hour, simple.minute) : base.time,
    weekday: simple?.weekday ?? base.weekday,
    dayOfMonth: simple?.dayOfMonth ?? base.dayOfMonth,
  };
}

/**
 * The cron expression a preset writes into the field, or null if the preset
 * inputs are invalid or the preset is "custom".
 */
export function presetCron(
  v: Pick<WindowFormValue, 'preset' | 'time' | 'weekday' | 'dayOfMonth'>,
): string | null {
  if (v.preset === 'custom') {
    return null;
  }
  return buildCron(v.preset, v.time, v.weekday, v.dayOfMonth);
}

/** Error of a once window's range, or null. */
export function onceRangeError(
  startsAt: string,
  endsAt: string,
  timezone: string,
): 'required' | 'invalid' | 'order' | null {
  if (!startsAt || !endsAt) {
    return 'required';
  }
  const start = parseLocalInput(startsAt);
  const end = parseLocalInput(endsAt);
  if (!start || !end || !isValidTimeZone(timezone)) {
    return 'invalid';
  }
  const startMs = zonedWallToUtcMs(start, timezone);
  const endMs = zonedWallToUtcMs(end, timezone);
  return endMs > startMs ? null : 'order';
}

function kindOf(c: AbstractControl): WindowKind | undefined {
  return c.parent?.get('kind')?.value as WindowKind | undefined;
}

/** Start is required for one-time windows. */
export const onceStartValidator: ValidatorFn = (c) => {
  if (kindOf(c) !== 'once') {
    return null;
  }
  if (!c.value) {
    return { required: true };
  }
  return parseLocalInput(c.value) ? null : { datetime: true };
};

/** End is required and after the start for one-time windows. */
export const onceEndValidator: ValidatorFn = (c) => {
  if (kindOf(c) !== 'once') {
    return null;
  }
  const startsAt = c.parent?.get('startsAt')?.value ?? '';
  const timezone = c.parent?.get('timezone')?.value ?? '';
  if (!c.value) {
    return { required: true };
  }
  if (!parseLocalInput(c.value)) {
    return { datetime: true };
  }
  if (!startsAt || !parseLocalInput(startsAt) || !isValidTimeZone(timezone)) {
    return null;
  }
  return onceRangeError(startsAt, c.value, timezone) === 'order' ? { order: true } : null;
};

/** The cron expression is required and valid for recurring windows. */
export const cronValidator: ValidatorFn = (c) => {
  if (kindOf(c) !== 'recurring') {
    return null;
  }
  return isValidCron(c.value ?? '') ? null : { cron: true };
};

/** Duration is a whole number of minutes in 1..10080 for recurring windows. */
export const durationValidator: ValidatorFn = (c): ValidationErrors | null => {
  if (kindOf(c) !== 'recurring') {
    return null;
  }
  const v = c.value;
  if (v === null || v === undefined || v === '') {
    return { required: true };
  }
  return Number.isInteger(v) && v >= 1 && v <= MAX_DURATION_MINUTES ? null : { range: true };
};

/** Time zone must be one Intl knows. */
export const timeZoneValidator: ValidatorFn = (c) => {
  const v = String(c.value ?? '').trim();
  if (!v) {
    return { required: true };
  }
  return isValidTimeZone(v) ? null : { timezone: true };
};

/** Time of day is required when a preset is used. */
export const presetTimeValidator: ValidatorFn = (c) => {
  if (c.parent?.get('preset')?.value === 'custom') {
    return null;
  }
  return parseTimeOfDay(c.value ?? '') ? null : { time: true };
};

/** Day of month is required and in 1..31 for the monthly preset. */
export const presetDayValidator: ValidatorFn = (c) => {
  if (c.parent?.get('preset')?.value !== 'monthly') {
    return null;
  }
  const v = c.value;
  return Number.isInteger(v) && v >= 1 && v <= 31 ? null : { range: true };
};

/**
 * Server field pointers address the nested schedule ("/schedule/cron"); the
 * flat editor form uses the bare name. Strips the prefix so that
 * applyFieldErrors finds the control.
 */
export function flattenFieldPointers(problem: Problem): Problem {
  return {
    ...problem,
    errors: problem.errors?.map((e) => ({
      ...e,
      field: e.field.startsWith('/schedule/') ? '/' + e.field.slice('/schedule/'.length) : e.field,
    })),
  };
}
