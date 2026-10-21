// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { Monitor, MonitorPage } from '../../api/gen/model';

/** Page size of the monitor lists (the API maximum). */
export const MONITOR_PAGE_SIZE = 500;

/** Upper bound of pages, so a misbehaving cursor cannot loop forever. */
const MAX_PAGES = 200;

/**
 * Collects all monitors by following the cursor of `fetchPage`, sorted by name.
 * Throws if the cursor keeps going, instead of returning a silently truncated list.
 */
export async function loadAllMonitors(
  fetchPage: (cursor: string | undefined) => Promise<MonitorPage>,
): Promise<Monitor[]> {
  const monitors: Monitor[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const result = await fetchPage(cursor);
    monitors.push(...result.items);
    const next = result.nextCursor ?? undefined;
    if (!next) {
      return monitors.sort((a, b) => a.name.localeCompare(b.name));
    }
    if (seen.has(next)) {
      throw new Error('monitor pagination cursor repeated');
    }
    seen.add(next);
    cursor = next;
  }
  throw new Error('too many monitor pages');
}
