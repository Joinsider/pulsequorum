// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

/** One detail of a system alert as text for the list. */
export interface DetailRow {
  readonly key: string;
  readonly value: string;
}

/**
 * Turns the details of a system alert into rows sorted by key (ADR 0025). The
 * details hold measured values, node IDs and flags; nested objects are shown as
 * compact JSON.
 */
export function detailRows(
  details: Readonly<Record<string, unknown>> | null | undefined,
): DetailRow[] {
  return Object.entries(details ?? {})
    .map(([key, value]) => ({ key, value: formatValue(value) }))
    .sort((a, b) => a.key.localeCompare(b.key));
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) {
    return '-';
  }
  if (typeof value === 'object') {
    return JSON.stringify(value);
  }
  return String(value);
}
