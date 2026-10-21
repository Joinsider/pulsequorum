// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';

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
