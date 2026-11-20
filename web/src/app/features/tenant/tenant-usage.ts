// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { OwnTenant } from '../../api/gen/model';

/**
 * Usage of the own tenant against its effective limits (FA-TEN-04). Only the
 * limits that are enforced today are shown: monitors, channels and the shortest interval.
 */

export type UsageKey = 'monitors' | 'channels' | 'interval';

/**
 * ok: below the limit. reached: at the limit, so one more object is refused.
 * exceeded: above the limit (after a smaller plan); existing stock keeps running.
 */
export type UsageState = 'ok' | 'reached' | 'exceeded';

export interface UsageRow {
  readonly key: UsageKey;
  /** Current value, or null when there is nothing to measure (no monitors for the interval). */
  readonly used: number | null;
  /** Effective limit, or null if unlimited. */
  readonly limit: number | null;
  readonly state: UsageState;
}

/** The usage rows of a tenant, in display order. */
export function usageRows(tenant: OwnTenant): UsageRow[] {
  const exceeded = new Set<string>(tenant.exceeded.map((e) => e.limit));
  const limits = tenant.effectiveLimits;
  const counted = (
    key: 'monitors' | 'channels',
    used: number,
    limit: number | null | undefined,
  ) => {
    const max = limit ?? null;
    const exceededHere = exceeded.has(key === 'monitors' ? 'maxMonitors' : 'maxChannels');
    return {
      key,
      used,
      limit: max,
      state: exceededHere ? 'exceeded' : max !== null && used >= max ? 'reached' : 'ok',
    } satisfies UsageRow;
  };
  return [
    counted('monitors', tenant.usage.monitors, limits.maxMonitors),
    counted('channels', tenant.usage.channels, limits.maxChannels),
    {
      key: 'interval',
      used: tenant.usage.minIntervalSeconds ?? null,
      limit: limits.minIntervalSeconds ?? null,
      state: exceeded.has('minIntervalSeconds') ? 'exceeded' : 'ok',
    },
  ];
}

/** Translation key of the hint for a row that is at or above its limit, or null. */
export function hintKey(row: UsageRow): string | null {
  if (row.state === 'ok') {
    return null;
  }
  return `dashboard.usage.hints.${row.state}.${row.key}`;
}
