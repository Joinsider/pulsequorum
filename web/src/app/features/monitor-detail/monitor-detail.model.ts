// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  GetUptimeParams,
  GetUptimeRange,
  Heartbeat,
  Incident,
  UptimeBucket,
} from '../../api/gen/model';

/** Uptime range of the detail page (same values as the API enum). */
export type Range = GetUptimeRange;

export const RANGES: readonly Range[] = ['24h', '7d', '30d', '365d'];

/** Heartbeats are loaded and kept for this window (node table, live chart). */
export const LIVE_WINDOW_MS = 24 * 60 * 60 * 1000;
/** Upper bound of heartbeats kept in memory while the page is open. */
export const MAX_LIVE_HEARTBEATS = 50_000;
export const HEARTBEAT_PAGE_LIMIT = 5000;
export const MAX_HEARTBEAT_PAGES = 10;
export const INCIDENT_LIMIT = 10;

/** A point of a time series: [epoch milliseconds, value]. */
export type TimePoint = [number, number];

/** Latency of one node; failed checks are listed separately for markers. */
export interface NodeSeries {
  nodeId: string;
  points: TimePoint[];
  failures: TimePoint[];
}

/** One page of a cursor-paginated API response. */
export interface Page<T> {
  items: T[];
  nextCursor?: string | null;
}

/** A duration split into the largest fitting unit, for translation. */
export interface DurationParts {
  unit: 'seconds' | 'minutes' | 'hours' | 'days';
  count: number;
}

/** Parameters of the uptime request; the node is omitted for the consensus view. */
export function uptimeQuery(range: Range, node: string | null): GetUptimeParams {
  return node ? { range, node } : { range };
}

/** Start of the heartbeat window, as an ISO timestamp for the API. */
export function heartbeatWindowStart(now: number): string {
  return new Date(now - LIVE_WINDOW_MS).toISOString();
}

export function heartbeatTime(h: Heartbeat): number {
  return Date.parse(h.ts);
}

/** Sorted, de-duplicated node ids of the heartbeats. */
export function nodeIds(heartbeats: readonly Heartbeat[]): string[] {
  return [...new Set(heartbeats.map((h) => h.nodeId))].sort((a, b) => a.localeCompare(b));
}

/**
 * Latency series per node, ascending by time. With a node filter only that
 * node is returned. Failed checks (status DOWN) are collected as markers.
 */
export function buildNodeSeries(
  heartbeats: readonly Heartbeat[],
  node: string | null,
): NodeSeries[] {
  const byNode = new Map<string, NodeSeries>();
  const sorted = [...heartbeats].sort((a, b) => heartbeatTime(a) - heartbeatTime(b));
  for (const h of sorted) {
    if (node && h.nodeId !== node) {
      continue;
    }
    const t = heartbeatTime(h);
    if (Number.isNaN(t)) {
      continue;
    }
    let series = byNode.get(h.nodeId);
    if (!series) {
      series = { nodeId: h.nodeId, points: [], failures: [] };
      byNode.set(h.nodeId, series);
    }
    series.points.push([t, h.latencyMs]);
    if (h.status === 'DOWN') {
      series.failures.push([t, h.latencyMs]);
    }
  }
  return [...byNode.values()].sort((a, b) => a.nodeId.localeCompare(b.nodeId));
}

/** Average latency per bucket; buckets without latency (no up checks) are skipped. */
export function bucketSeries(buckets: readonly UptimeBucket[]): TimePoint[] {
  const points: TimePoint[] = [];
  for (const b of buckets) {
    const t = Date.parse(b.start);
    if (!Number.isNaN(t) && b.latencyAvgMs != null) {
      points.push([t, b.latencyAvgMs]);
    }
  }
  return points;
}

/** Newest heartbeat per node, sorted by node id. */
export function latestPerNode(heartbeats: readonly Heartbeat[]): Heartbeat[] {
  const latest = new Map<string, Heartbeat>();
  for (const h of heartbeats) {
    const prev = latest.get(h.nodeId);
    if (!prev || heartbeatTime(h) >= heartbeatTime(prev)) {
      latest.set(h.nodeId, h);
    }
  }
  return [...latest.values()].sort((a, b) => a.nodeId.localeCompare(b.nodeId));
}

/**
 * Adds a live heartbeat to the window. Duplicates (same node and timestamp)
 * are ignored, heartbeats older than the live window are dropped and the
 * result is capped at {@link MAX_LIVE_HEARTBEATS} (newest kept).
 */
export function appendHeartbeat(
  heartbeats: readonly Heartbeat[],
  heartbeat: Heartbeat,
  now: number,
): Heartbeat[] {
  if (Number.isNaN(heartbeatTime(heartbeat))) {
    return [...heartbeats];
  }
  if (heartbeats.some((h) => h.nodeId === heartbeat.nodeId && h.ts === heartbeat.ts)) {
    return [...heartbeats];
  }
  const cutoff = now - LIVE_WINDOW_MS;
  const next = [...heartbeats, heartbeat]
    .filter((h) => heartbeatTime(h) >= cutoff)
    .sort((a, b) => heartbeatTime(a) - heartbeatTime(b));
  return next.length > MAX_LIVE_HEARTBEATS ? next.slice(next.length - MAX_LIVE_HEARTBEATS) : next;
}

/**
 * Collects all pages of a cursor-paginated endpoint, at most `maxPages` pages.
 * Stops early when the server returns no next cursor.
 */
export async function collectPages<T>(
  fetchPage: (cursor: string | undefined) => Promise<Page<T>>,
  maxPages: number,
): Promise<T[]> {
  const items: T[] = [];
  let cursor: string | undefined;
  for (let i = 0; i < maxPages; i++) {
    const page = await fetchPage(cursor);
    items.push(...page.items);
    if (!page.nextCursor) {
      break;
    }
    cursor = page.nextCursor;
  }
  return items;
}

/** Milliseconds from start to end (or now while ongoing); never negative. */
export function incidentDuration(incident: Incident, now: number): number {
  const start = Date.parse(incident.startedAt);
  const end = incident.resolvedAt ? Date.parse(incident.resolvedAt) : now;
  if (Number.isNaN(start) || Number.isNaN(end)) {
    return 0;
  }
  return Math.max(0, end - start);
}

/** Splits a duration into the largest unit that keeps the count below its limit. */
export function splitDuration(ms: number): DurationParts {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 120) {
    return { unit: 'seconds', count: seconds };
  }
  const minutes = Math.round(seconds / 60);
  if (minutes < 120) {
    return { unit: 'minutes', count: minutes };
  }
  const hours = Math.round(seconds / 3600);
  if (hours < 48) {
    return { unit: 'hours', count: hours };
  }
  return { unit: 'days', count: Math.round(seconds / 86_400) };
}

/**
 * Client certificate facts of a check result (ADR 0042). The API sends them in
 * the result `details`; they are absent when the check used no client certificate.
 */
export interface ClientCertificateInfo {
  subject: string;
  issuer: string;
  /** Expiry as RFC 3339 timestamp; empty when the server did not send one. */
  notAfter: string;
  /** Whether the certificate was sent in the handshake. */
  sent: boolean;
}

/** Reads the client certificate facts from check result details, or null when absent. */
export function clientCertificateInfo(
  details: Record<string, unknown> | undefined,
): ClientCertificateInfo | null {
  const subject = details?.['clientCertSubject'];
  if (typeof subject !== 'string' || subject === '') {
    return null;
  }
  const issuer = details?.['clientCertIssuer'];
  const notAfter = details?.['clientCertNotAfter'];
  return {
    subject,
    issuer: typeof issuer === 'string' ? issuer : '',
    notAfter: typeof notAfter === 'string' ? notAfter : '',
    sent: details?.['clientCertSent'] === true,
  };
}

/** Monitor target for display: the first address-like field of the type config. */
export function monitorTarget(config: Record<string, unknown> | undefined): string {
  if (!config) {
    return '';
  }
  for (const key of ['url', 'host', 'hostname', 'address', 'target']) {
    const value = config[key];
    if (typeof value === 'string' && value !== '') {
      const port = config['port'];
      return key === 'host' && (typeof port === 'number' || typeof port === 'string')
        ? `${value}:${port}`
        : value;
    }
  }
  return '';
}

/** Whether an error is an HTTP 404 (for example a monitor that does not exist). */
export function isNotFound(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { status?: unknown }).status === 404;
}
