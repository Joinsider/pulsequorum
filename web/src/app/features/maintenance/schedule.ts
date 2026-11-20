// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

/**
 * Pure helpers for maintenance schedules: time zone conversion with Intl,
 * cron validation, presets and human-readable descriptions. No I/O, no
 * clock reads except where a caller passes the instant.
 */

/** A wall-clock time without zone, as typed in a datetime-local input. */
export interface WallTime {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
}

const LOCAL_INPUT = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::\d{2})?$/;
const TIME_OF_DAY = /^(\d{2}):(\d{2})$/;

/** Parses a datetime-local value ("2026-03-29T02:30"). Returns null for anything invalid. */
export function parseLocalInput(value: string): WallTime | null {
  const m = LOCAL_INPUT.exec(value.trim());
  if (!m) {
    return null;
  }
  const w: WallTime = {
    year: Number(m[1]),
    month: Number(m[2]),
    day: Number(m[3]),
    hour: Number(m[4]),
    minute: Number(m[5]),
  };
  // Reject impossible dates such as 2026-02-31.
  const probe = new Date(Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute));
  if (
    probe.getUTCMonth() !== w.month - 1 ||
    probe.getUTCDate() !== w.day ||
    w.hour > 23 ||
    w.minute > 59
  ) {
    return null;
  }
  return w;
}

/** Formats a wall-clock time as a datetime-local value. */
export function formatLocalInput(w: WallTime): string {
  return `${pad(w.year, 4)}-${pad(w.month)}-${pad(w.day)}T${pad(w.hour)}:${pad(w.minute)}`;
}

const partFormatters = new Map<string, Intl.DateTimeFormat>();

function partsOf(timeZone: string, utcMs: number): Record<string, number> {
  let fmt = partFormatters.get(timeZone);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat('en-US', {
      timeZone,
      hourCycle: 'h23',
      year: 'numeric',
      month: 'numeric',
      day: 'numeric',
      hour: 'numeric',
      minute: 'numeric',
      second: 'numeric',
    });
    partFormatters.set(timeZone, fmt);
  }
  const out: Record<string, number> = {};
  for (const p of fmt.formatToParts(new Date(utcMs))) {
    if (p.type !== 'literal') {
      out[p.type] = Number(p.value);
    }
  }
  return out;
}

/**
 * UTC offset of a time zone at an instant, in milliseconds (wall clock minus
 * UTC). Seconds of the instant are truncated, as in the wall clock.
 */
export function zoneOffsetMs(utcMs: number, timeZone: string): number {
  const p = partsOf(timeZone, utcMs);
  const asUtc = Date.UTC(
    p['year'],
    p['month'] - 1,
    p['day'],
    p['hour'] % 24,
    p['minute'],
    p['second'],
  );
  return asUtc - Math.floor(utcMs / 1000) * 1000;
}

/**
 * Converts a wall-clock time in a time zone to a UTC instant (ms since epoch).
 * A time inside a spring-forward gap is moved past the gap; an ambiguous
 * time in the autumn overlap resolves to its later occurrence.
 */
export function zonedWallToUtcMs(w: WallTime, timeZone: string): number {
  const wall = Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute);
  const first = zoneOffsetMs(wall, timeZone);
  let utc = wall - first;
  const second = zoneOffsetMs(utc, timeZone);
  if (second !== first) {
    utc = wall - second;
  }
  return utc;
}

/** Shows a UTC instant as a datetime-local value in a time zone. */
export function utcToLocalInput(utcMs: number, timeZone: string): string {
  const shifted = new Date(utcMs + zoneOffsetMs(utcMs, timeZone));
  return formatLocalInput({
    year: shifted.getUTCFullYear(),
    month: shifted.getUTCMonth() + 1,
    day: shifted.getUTCDate(),
    hour: shifted.getUTCHours(),
    minute: shifted.getUTCMinutes(),
  });
}

/** True if Intl knows the IANA time zone. */
export function isValidTimeZone(timeZone: string): boolean {
  if (!timeZone) {
    return false;
  }
  try {
    new Intl.DateTimeFormat('en-US', { timeZone });
    return true;
  } catch {
    return false;
  }
}

/** The browser's own time zone, or UTC if it cannot be determined. */
export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
}

/** All time zones the browser supports, UTC first if present; falls back to the browser zone. */
export function listTimeZones(): string[] {
  const zones = Intl.supportedValuesOf('timeZone');
  const list = zones.length > 0 ? zones : [browserTimeZone()];
  return list.includes('UTC') ? list : ['UTC', ...list];
}

/** Formats an instant in a time zone (or the browser zone when omitted). */
export function formatInstant(utcMs: number, lang: string, timeZone?: string): string {
  return new Intl.DateTimeFormat(lang, {
    dateStyle: 'medium',
    timeStyle: 'short',
    ...(timeZone ? { timeZone } : {}),
  }).format(utcMs);
}

/** Formats an interval, e.g. "3 Mar, 02:00 – 04:00". */
export function formatInterval(
  fromMs: number,
  toMs: number,
  lang: string,
  timeZone?: string,
): string {
  return new Intl.DateTimeFormat(lang, {
    dateStyle: 'medium',
    timeStyle: 'short',
    ...(timeZone ? { timeZone } : {}),
  }).formatRange(fromMs, toMs);
}

/** Localised weekday name for cron weekday numbers (0 = Sunday). */
export function weekdayName(lang: string, weekday: number): string {
  // 2024-01-07 was a Sunday.
  const sunday = Date.UTC(2024, 0, 7 + (weekday % 7));
  return new Intl.DateTimeFormat(lang, { weekday: 'long', timeZone: 'UTC' }).format(sunday);
}

/** Expands the supported @-macros; anything else is only trimmed and lower-cased. */
export function expandCron(expr: string): string {
  const text = expr.trim().toLowerCase();
  return CRON_MACROS.get(text) ?? text;
}

const CRON_MACROS = new Map<string, string>([
  ['@yearly', '0 0 1 1 *'],
  ['@annually', '0 0 1 1 *'],
  ['@monthly', '0 0 1 * *'],
  ['@weekly', '0 0 * * 0'],
  ['@daily', '0 0 * * *'],
  ['@midnight', '0 0 * * *'],
  ['@hourly', '0 * * * *'],
]);

interface FieldSpec {
  lo: number;
  hi: number;
  names?: ReadonlyMap<string, number>;
}

const MONTH_NAMES = new Map([
  ['jan', 1],
  ['feb', 2],
  ['mar', 3],
  ['apr', 4],
  ['may', 5],
  ['jun', 6],
  ['jul', 7],
  ['aug', 8],
  ['sep', 9],
  ['oct', 10],
  ['nov', 11],
  ['dec', 12],
]);
const WEEKDAY_NAMES = new Map([
  ['sun', 0],
  ['mon', 1],
  ['tue', 2],
  ['wed', 3],
  ['thu', 4],
  ['fri', 5],
  ['sat', 6],
]);

const FIELDS: readonly FieldSpec[] = [
  { lo: 0, hi: 59 },
  { lo: 0, hi: 23 },
  { lo: 1, hi: 31 },
  { lo: 1, hi: 12, names: MONTH_NAMES },
  { lo: 0, hi: 7, names: WEEKDAY_NAMES },
];

/** Value of one token (number or name) within a field, or null. */
function tokenValue(token: string, spec: FieldSpec): number | null {
  const named = spec.names?.get(token);
  if (named !== undefined) {
    return named;
  }
  if (!/^\d+$/.test(token)) {
    return null;
  }
  const n = Number(token);
  return n >= spec.lo && n <= spec.hi ? n : null;
}

function isValidField(field: string, spec: FieldSpec): boolean {
  return field.split(',').every((part) => {
    const pieces = part.split('/');
    if (pieces.length > 2) {
      return false;
    }
    const [range, step] = pieces;
    if (step !== undefined) {
      if (!/^\d+$/.test(step) || Number(step) < 1 || Number(step) > spec.hi - spec.lo + 1) {
        return false;
      }
    }
    if (range === '*') {
      return true;
    }
    const bounds = range.split('-');
    if (bounds.length > 2) {
      return false;
    }
    const from = tokenValue(bounds[0], spec);
    if (from === null) {
      return false;
    }
    if (bounds.length === 1) {
      return true;
    }
    const to = tokenValue(bounds[1], spec);
    return to !== null && from <= to;
  });
}

/**
 * Checks a 5-field cron expression or an @-macro with the same rules as the
 * server (internal/maintenance/cron.go): *, numbers, names, ranges, lists and steps.
 */
export function isValidCron(expr: string): boolean {
  const fields = expandCron(expr)
    .split(/\s+/)
    .filter((f) => f !== '');
  if (fields.length !== 5) {
    return false;
  }
  return fields.every((field, i) => isValidField(field, FIELDS[i]));
}

/** A cron expression recognised as one of the presets (or a weekday range). */
export interface SimpleCron {
  kind: 'daily' | 'weekly' | 'monthly' | 'weekdays';
  hour: number;
  minute: number;
  /** 0 = Sunday; set for kind weekly. */
  weekday: number;
  /** Set for kind monthly. */
  dayOfMonth: number;
}

/** Recognises the patterns the editor presets produce. Returns null for anything else. */
export function parseSimpleCron(expr: string): SimpleCron | null {
  if (!isValidCron(expr)) {
    return null;
  }
  const [minuteF, hourF, domF, monthF, dowF] = expandCron(expr).split(/\s+/);
  const minute = tokenValue(minuteF, FIELDS[0]);
  const hour = tokenValue(hourF, FIELDS[1]);
  if (
    minute === null ||
    hour === null ||
    monthF !== '*' ||
    !isSingleValue(minuteF) ||
    !isSingleValue(hourF)
  ) {
    return null;
  }
  const base = { hour, minute, weekday: 0, dayOfMonth: 1 };
  if (domF === '*' && dowF === '*') {
    return { ...base, kind: 'daily' };
  }
  if (domF === '*' && dowF === '1-5') {
    return { ...base, kind: 'weekdays' };
  }
  if (domF === '*' && isSingleValue(dowF)) {
    const weekday = tokenValue(dowF, FIELDS[4]);
    return weekday === null ? null : { ...base, kind: 'weekly', weekday: weekday % 7 };
  }
  if (dowF === '*' && isSingleValue(domF)) {
    const dayOfMonth = tokenValue(domF, FIELDS[2]);
    return dayOfMonth === null ? null : { ...base, kind: 'monthly', dayOfMonth };
  }
  return null;
}

function isSingleValue(field: string): boolean {
  return !/[*,/-]/.test(field);
}

/** Parses "HH:mm" into hour and minute, or null. */
export function parseTimeOfDay(value: string): { hour: number; minute: number } | null {
  const m = TIME_OF_DAY.exec(value.trim());
  if (!m) {
    return null;
  }
  const hour = Number(m[1]);
  const minute = Number(m[2]);
  return hour <= 23 && minute <= 59 ? { hour, minute } : null;
}

/** Formats hour and minute as "HH:mm". */
export function formatTimeOfDay(hour: number, minute: number): string {
  return `${pad(hour)}:${pad(minute)}`;
}

/** The presets the editor offers; "custom" means the cron field is edited directly. */
export type CronPreset = 'daily' | 'weekly' | 'monthly';

/**
 * Builds the cron expression of a preset. Returns null if the time, weekday or
 * day of month is invalid.
 */
export function buildCron(
  preset: CronPreset,
  time: string,
  weekday: number,
  dayOfMonth: number,
): string | null {
  const t = parseTimeOfDay(time);
  if (!t) {
    return null;
  }
  switch (preset) {
    case 'daily':
      return `${t.minute} ${t.hour} * * *`;
    case 'weekly':
      return Number.isInteger(weekday) && weekday >= 0 && weekday <= 6
        ? `${t.minute} ${t.hour} * * ${weekday}`
        : null;
    case 'monthly':
      return Number.isInteger(dayOfMonth) && dayOfMonth >= 1 && dayOfMonth <= 31
        ? `${t.minute} ${t.hour} ${dayOfMonth} * *`
        : null;
  }
}

/**
 * Human-readable text for the common cron patterns, e.g. "Every Sunday at
 * 03:00". Returns null for other expressions; callers show the raw expression then.
 *
 * @param translate looks up a `maintenance.cron.*` key with its parameters
 * @param weekdayName localised weekday name for a cron weekday number
 */
export function describeCron(
  expr: string,
  translate: (key: string, params: Record<string, string | number>) => string,
  weekdayName: (weekday: number) => string,
): string | null {
  const simple = parseSimpleCron(expr);
  if (!simple) {
    return null;
  }
  const time = formatTimeOfDay(simple.hour, simple.minute);
  switch (simple.kind) {
    case 'daily':
      return translate('maintenance.cron.daily', { time });
    case 'weekdays':
      return translate('maintenance.cron.weekdays', { time });
    case 'weekly':
      return translate('maintenance.cron.weekly', { day: weekdayName(simple.weekday), time });
    case 'monthly':
      return translate('maintenance.cron.monthly', { day: simple.dayOfMonth, time });
  }
}

function pad(n: number, width = 2): string {
  return String(n).padStart(width, '0');
}
