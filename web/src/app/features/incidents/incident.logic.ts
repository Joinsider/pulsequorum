// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Params } from '@angular/router';
import { Incident, ListIncidentsParams } from '../../api/gen/model';

export type IncidentState = 'ongoing' | 'resolved' | 'all';
export type IncidentRange = '24h' | '7d' | '30d' | 'all';

export interface IncidentFilters {
  readonly state: IncidentState;
  /** UUID of one monitor, or null for all monitors. */
  readonly monitorId: string | null;
  readonly range: IncidentRange;
}

export const INCIDENT_STATES: readonly IncidentState[] = ['all', 'ongoing', 'resolved'];
export const INCIDENT_RANGES: readonly IncidentRange[] = ['all', '24h', '7d', '30d'];

export const DEFAULT_FILTERS: IncidentFilters = { state: 'all', monitorId: null, range: 'all' };

const DAY_MS = 24 * 3600 * 1000;
const RANGE_MS: Record<Exclude<IncidentRange, 'all'>, number> = {
  '24h': DAY_MS,
  '7d': 7 * DAY_MS,
  '30d': 30 * DAY_MS,
};

/** Reads the filters from the URL query parameters; unknown values fall back to the defaults. */
export function parseFilters(params: { get(name: string): string | null }): IncidentFilters {
  const state = params.get('state');
  const range = params.get('range');
  const monitor = params.get('monitor');
  return {
    state: isOneOf(state, INCIDENT_STATES) ? state : DEFAULT_FILTERS.state,
    range: isOneOf(range, INCIDENT_RANGES) ? range : DEFAULT_FILTERS.range,
    monitorId: monitor && UUID.test(monitor) ? monitor : null,
  };
}

/** Query parameters for the filters; defaults become null so the URL stays clean. */
export function toQueryParams(filters: IncidentFilters): Params {
  return {
    state: filters.state === DEFAULT_FILTERS.state ? null : filters.state,
    range: filters.range === DEFAULT_FILTERS.range ? null : filters.range,
    monitor: filters.monitorId,
  };
}

/**
 * Parameters of GET /incidents for the filters. The API has no "resolved only"
 * switch (`open=false` simply means "no restriction"), so that state is applied
 * client-side with {@link matchesFilters}.
 */
export function toApiParams(
  filters: IncidentFilters,
  now: number,
): Pick<ListIncidentsParams, 'open' | 'monitorId' | 'from'> {
  return {
    open: filters.state === 'ongoing' ? true : undefined,
    monitorId: filters.monitorId ?? undefined,
    from:
      filters.range === 'all' ? undefined : new Date(now - RANGE_MS[filters.range]).toISOString(),
  };
}

/** Whether an incident belongs in a list shown with these filters (used for live updates). */
export function matchesFilters(incident: Incident, filters: IncidentFilters, now: number): boolean {
  if (filters.monitorId && incident.monitorId.toLowerCase() !== filters.monitorId.toLowerCase()) {
    return false;
  }
  if (filters.state === 'ongoing' && incident.resolvedAt) {
    return false;
  }
  if (filters.state === 'resolved' && !incident.resolvedAt) {
    return false;
  }
  if (filters.range !== 'all' && Date.parse(incident.startedAt) < now - RANGE_MS[filters.range]) {
    return false;
  }
  return true;
}

/** Newest first by start time; ties broken by id so the order is stable. */
export function sortNewestFirst(incidents: readonly Incident[]): Incident[] {
  return [...incidents].sort(
    (a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt) || compareIds(a.id, b.id),
  );
}

/** Adds or replaces an incident by id, keeping the list sorted and filtered. */
export function upsertIncident(
  list: readonly Incident[],
  incident: Incident,
  filters: IncidentFilters,
  now: number,
): Incident[] {
  const rest = list.filter((i) => i.id !== incident.id);
  if (!matchesFilters(incident, filters, now)) {
    return rest.length === list.length ? [...list] : rest;
  }
  return sortNewestFirst([...rest, incident]);
}

/** Appends a page to the list, dropping duplicates (a live update may already have added an entry). */
export function mergeIncidents(
  existing: readonly Incident[],
  page: readonly Incident[],
): Incident[] {
  const byId = new Map<string, Incident>();
  for (const incident of [...existing, ...page]) {
    byId.set(incident.id, incident);
  }
  return sortNewestFirst([...byId.values()]);
}

/** Absolute local date and time in the given language, e.g. "21 Oct 2026, 12:00". */
export function formatAbsolute(iso: string, lang: string): string {
  const time = Date.parse(iso);
  if (Number.isNaN(time)) {
    return '';
  }
  return new Intl.DateTimeFormat(lang, { dateStyle: 'medium', timeStyle: 'short' }).format(
    new Date(time),
  );
}

/** Milliseconds from the start to the resolution, or to `now` while ongoing. */
export function incidentDurationMs(incident: Incident, now: number): number {
  const start = Date.parse(incident.startedAt);
  const end = incident.resolvedAt ? Date.parse(incident.resolvedAt) : now;
  if (Number.isNaN(start) || Number.isNaN(end)) {
    return 0;
  }
  return Math.max(0, end - start);
}

export type DurationUnit = 'd' | 'h' | 'min' | 's';

export interface DurationPart {
  readonly unit: DurationUnit;
  readonly value: number;
}

/**
 * Splits a duration into at most two units, e.g. 2 h 5 min, 3 d 4 h, 45 s.
 * A zero second unit is dropped, so exactly 2 h yields only the hours.
 * Labels are localised by the caller (keys `incidents.units.<unit>`).
 */
export function durationParts(ms: number): DurationPart[] {
  const total = Math.max(0, Math.floor(ms / 1000));
  const days = Math.floor(total / 86_400);
  const hours = Math.floor((total % 86_400) / 3_600);
  const minutes = Math.floor((total % 3_600) / 60);
  const seconds = total % 60;

  if (days > 0) {
    return compact([
      { unit: 'd', value: days },
      { unit: 'h', value: hours },
    ]);
  }
  if (hours > 0) {
    return compact([
      { unit: 'h', value: hours },
      { unit: 'min', value: minutes },
    ]);
  }
  if (minutes > 0) {
    return compact([
      { unit: 'min', value: minutes },
      { unit: 's', value: seconds },
    ]);
  }
  return [{ unit: 's', value: seconds }];
}

/** Keeps the first part and drops a zero second part. */
function compact(parts: DurationPart[]): DurationPart[] {
  return parts.filter((part, index) => index === 0 || part.value > 0);
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function isOneOf<T extends string>(value: string | null, options: readonly T[]): value is T {
  return value !== null && (options as readonly string[]).includes(value);
}

function compareIds(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}
