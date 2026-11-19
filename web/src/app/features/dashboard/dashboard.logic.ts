// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import { problemOf } from '../../shared/problem';

/** Every status in display order for the summary chips. */
export const STATUS_ORDER: readonly MonitorStatus[] = [
  MonitorStatus.DOWN,
  MonitorStatus.DEGRADED,
  MonitorStatus.PARTIAL,
  MonitorStatus.UP,
  MonitorStatus.PENDING,
  MonitorStatus.MAINTENANCE,
  MonitorStatus.PAUSED,
  MonitorStatus.UNKNOWN,
];

/** Config keys that usually hold the probe target, in order of preference. */
const TARGET_KEYS = ['url', 'host', 'hostname', 'address', 'target'] as const;

/** Sort rank: problems first, then warnings, then everything else. */
export function statusRank(status: MonitorStatus): number {
  switch (status) {
    case MonitorStatus.DOWN:
      return 0;
    case MonitorStatus.DEGRADED:
    case MonitorStatus.PARTIAL:
      return 1;
    default:
      return 2;
  }
}

/** DOWN first, then DEGRADED/PARTIAL, then the rest; ties by name. Returns a new array. */
export function sortMonitors(monitors: readonly Monitor[]): Monitor[] {
  return [...monitors].sort(
    (a, b) =>
      statusRank(a.status) - statusRank(b.status) ||
      a.name.localeCompare(b.name, undefined, { sensitivity: 'base', numeric: true }) ||
      a.id.localeCompare(b.id),
  );
}

/** Number of monitors per status; statuses without monitors count 0. */
export function countByStatus(monitors: readonly Monitor[]): Record<MonitorStatus, number> {
  const counts = Object.fromEntries(STATUS_ORDER.map((s) => [s, 0])) as Record<
    MonitorStatus,
    number
  >;
  for (const m of monitors) {
    counts[m.status] = (counts[m.status] ?? 0) + 1;
  }
  return counts;
}

/**
 * Applies the status chip and the name search. The search is case-insensitive
 * and ignores surrounding whitespace; an empty search matches everything.
 */
export function filterMonitors(
  monitors: readonly Monitor[],
  status: MonitorStatus | null,
  query: string,
): Monitor[] {
  const needle = query.trim().toLocaleLowerCase();
  return monitors.filter(
    (m) =>
      (status === null || m.status === status) &&
      (needle === '' || m.name.toLocaleLowerCase().includes(needle)),
  );
}

/**
 * Human readable probe target taken from the type-specific config, e.g.
 * `https://example.org` or `db.internal:5432`. Returns null for unknown shapes.
 */
export function targetOf(config: unknown): string | null {
  if (typeof config !== 'object' || config === null) {
    return null;
  }
  const c = config as Record<string, unknown>;
  for (const key of TARGET_KEYS) {
    const value = c[key];
    if (typeof value === 'string' && value.trim() !== '') {
      const port = c['port'];
      if (key === 'host' || key === 'hostname' || key === 'address') {
        return typeof port === 'number' || typeof port === 'string' ? `${value}:${port}` : value;
      }
      return value;
    }
  }
  return null;
}

/** Formats a 0–100 percentage with two decimals, or the dash for no data. */
export function formatPercent(value: number | null | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return '–';
  }
  return `${value.toFixed(2)} %`;
}

/** Latency in whole milliseconds, or the dash for no data. */
export function formatLatency(value: number | null | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return '–';
  }
  return `${Math.round(value)}`;
}

/**
 * Folds a live event into the monitor list without a round trip.
 * - `state`: new status, `statusSince` is the event time.
 * - `heartbeat`: `lastCheckAt` and `lastLatencyMs` of the newest check.
 * Returns the same array when nothing changed, so callers can skip work.
 * Events for unknown monitors are ignored here; the caller reloads for them.
 */
export function applyStreamEvent(
  monitors: readonly Monitor[],
  event: StreamEvent,
): readonly Monitor[] {
  if (!event.monitorId) {
    return monitors;
  }
  const index = monitors.findIndex((m) => m.id === event.monitorId);
  if (index < 0) {
    return monitors;
  }
  const current = monitors[index];
  let next: Monitor | null = null;

  if (event.type === 'state' && event.status && event.status !== current.status) {
    next = { ...current, status: event.status, statusSince: event.ts };
  } else if (event.type === 'heartbeat' && event.heartbeat) {
    const at = Date.parse(event.heartbeat.ts);
    const known = current.lastCheckAt ? Date.parse(current.lastCheckAt) : NaN;
    if (!Number.isNaN(at) && (Number.isNaN(known) || at >= known)) {
      next = {
        ...current,
        lastCheckAt: event.heartbeat.ts,
        lastLatencyMs: event.heartbeat.latencyMs,
      };
    }
  }
  if (!next) {
    return monitors;
  }
  const copy = [...monitors];
  copy[index] = next;
  return copy;
}

/** True when the event refers to a monitor that is not in the list. */
export function isUnknownMonitorEvent(monitors: readonly Monitor[], event: StreamEvent): boolean {
  return !!event.monitorId && !monitors.some((m) => m.id === event.monitorId);
}

/** Largest selection the API accepts in one atomic bulk call (ADR 0031). */
export const BULK_MAX_IDS = 500;
/** Limits of a single tag as the API validates them. */
export const TAG_MAX_LENGTH = 64;
export const TAG_MAX_COUNT = 20;

/** Checkbox state of the "select all visible" control. */
export type SelectionState = 'none' | 'some' | 'all';

/** Whether all, some or none of the visible monitors are selected. */
export function selectionState(
  selected: ReadonlySet<string>,
  visibleIds: readonly string[],
): SelectionState {
  let hits = 0;
  for (const id of visibleIds) {
    if (selected.has(id)) {
      hits++;
    }
  }
  if (hits === 0) {
    return 'none';
  }
  return hits === visibleIds.length ? 'all' : 'some';
}

/**
 * Selects every visible monitor, or clears the visible ones when all of them
 * are already selected. Selected monitors outside the visible list are kept.
 * Returns a new set.
 */
export function toggleAll(
  selected: ReadonlySet<string>,
  visibleIds: readonly string[],
): Set<string> {
  const next = new Set(selected);
  const clear = selectionState(selected, visibleIds) === 'all';
  for (const id of visibleIds) {
    if (clear) {
      next.delete(id);
    } else {
      next.add(id);
    }
  }
  return next;
}

/**
 * Drops selected IDs that are no longer in the list, e.g. after a delete on
 * another client. Returns the same set when nothing was dropped.
 */
export function pruneSelection(
  selected: ReadonlySet<string>,
  existingIds: readonly string[],
): ReadonlySet<string> {
  const existing = new Set(existingIds);
  const kept = [...selected].filter((id) => existing.has(id));
  return kept.length === selected.size ? selected : new Set(kept);
}

/** Result of {@link parseTags}: the cleaned tags, or the translation key of the problem. */
export type TagParseResult = { tags: string[] } | { error: string };

/**
 * Parses the comma-separated tag field of the bulk tag actions. Entries are
 * trimmed, empty entries and duplicates are dropped. Each tag may have 1 to 64
 * characters and there may be at most 20 tags.
 */
export function parseTags(input: string): TagParseResult {
  const tags: string[] = [];
  const seen = new Set<string>();
  for (const raw of input.split(',')) {
    const tag = raw.trim();
    if (tag === '') {
      continue;
    }
    if (Array.from(tag).length > TAG_MAX_LENGTH) {
      return { error: 'dashboard.bulk.tags.tooLong' };
    }
    if (!seen.has(tag)) {
      seen.add(tag);
      tags.push(tag);
    }
  }
  if (tags.length === 0) {
    return { error: 'dashboard.bulk.tags.empty' };
  }
  if (tags.length > TAG_MAX_COUNT) {
    return { error: 'dashboard.bulk.tags.tooMany' };
  }
  return { tags };
}

/** Translation key of the message for a failed bulk call, by HTTP status. */
export function bulkErrorKey(err: unknown): string {
  const status =
    problemOf(err)?.status ?? (err instanceof HttpErrorResponse ? err.status : undefined);
  switch (status) {
    case 403:
      return 'dashboard.bulk.errors.forbidden';
    case 409:
      return 'dashboard.bulk.errors.conflict';
    case 422:
      return 'dashboard.bulk.errors.invalid';
    case 503:
      return 'dashboard.bulk.errors.unavailable';
    default:
      return 'dashboard.bulk.errors.generic';
  }
}
