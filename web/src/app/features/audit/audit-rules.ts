// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ListAdminAuditParams } from '../../api/gen/model';
import { problemOf } from '../../shared/problem';

/**
 * Which audit log a page shows: the tenant's own log (`tenant`, FA-DAT-01) or the
 * platform view across all tenants (`platform`, ADR 0031).
 */
export type AuditScope = 'tenant' | 'platform';

/** Entries per page. The server allows at most 200. */
export const AUDIT_PAGE_SIZE = 50;

/** Raw values of the filter form. An empty string means "no filter". */
export interface AuditFilter {
  readonly tenant: string;
  readonly action: string;
  readonly objectKind: string;
  readonly objectId: string;
  readonly actor: string;
  /** Calendar day from `<input type="date">`, YYYY-MM-DD, in local time. */
  readonly from: string;
  /** Calendar day from `<input type="date">`, YYYY-MM-DD, in local time; included in full. */
  readonly to: string;
}

export const EMPTY_AUDIT_FILTER: AuditFilter = {
  tenant: '',
  action: '',
  objectKind: '',
  objectId: '',
  actor: '',
  from: '',
  to: '',
};

/**
 * Query parameters for one page. Trims the text fields, drops the empty ones and
 * converts the days to RFC 3339 timestamps (`from` at 00:00:00, `to` at 23:59:59
 * local time). Unknown or malformed days are dropped instead of sent.
 */
export function buildAuditParams(
  filter: AuditFilter,
  cursor?: string | null,
): ListAdminAuditParams {
  return compact({
    cursor: cursor?.trim() || undefined,
    limit: AUDIT_PAGE_SIZE,
    tenant: nonEmpty(filter.tenant),
    action: nonEmpty(filter.action),
    objectKind: nonEmpty(filter.objectKind),
    objectId: nonEmpty(filter.objectId),
    actor: nonEmpty(filter.actor),
    from: toRfc3339(filter.from, false),
    to: toRfc3339(filter.to, true),
  }) as ListAdminAuditParams;
}

/**
 * Translation key of a failed audit request. The server's problem status wins;
 * a body that is not a problem document falls back to the HTTP status.
 */
export function auditErrorKey(err: unknown): string {
  const status =
    problemOf(err)?.status ?? (err instanceof HttpErrorResponse ? err.status : undefined);
  switch (status) {
    case 400:
    case 422:
      return 'audit.error.invalid';
    case 403:
      return 'audit.error.forbidden';
    case 503:
      return 'audit.error.unavailable';
    default:
      return 'audit.error.generic';
  }
}

/** Translation key for the kind of actor shown next to its name. */
export function actorKindKey(kind: string): string {
  switch (kind) {
    case 'user':
      return 'audit.actorKind.user';
    case 'api_token':
      return 'audit.actorKind.apiToken';
    case 'system':
      return 'audit.actorKind.system';
    default:
      return 'audit.actorKind.unknown';
  }
}

/**
 * Short form of a request ID for the table; the full value goes into a tooltip.
 * Bulk requests carry a prefix such as `api:default:<uuid>`, so the part after the
 * last colon is shortened; otherwise all bulk entries would read `api:defa`.
 */
export function shortRequestId(requestId: string): string {
  return requestId.slice(requestId.lastIndexOf(':') + 1).slice(0, 8);
}

/** Absolute timestamp in the given language, local time zone. Unparseable input is returned as is. */
export function formatAuditTime(iso: string, lang: string): string {
  const time = Date.parse(iso);
  if (Number.isNaN(time)) {
    return iso;
  }
  return new Intl.DateTimeFormat(lang, { dateStyle: 'medium', timeStyle: 'medium' }).format(
    new Date(time),
  );
}

function nonEmpty(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed === '' ? undefined : trimmed;
}

/** `YYYY-MM-DD` to RFC 3339 in local time, start or end of the day. */
function toRfc3339(day: string, endOfDay: boolean): string | undefined {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day.trim());
  if (!match) {
    return undefined;
  }
  const year = Number(match[1]);
  const month = Number(match[2]) - 1;
  const date = Number(match[3]);
  const at = endOfDay
    ? new Date(year, month, date, 23, 59, 59)
    : new Date(year, month, date, 0, 0, 0);
  if (Number.isNaN(at.getTime())) {
    return undefined;
  }
  return at.toISOString().replace('.000Z', 'Z');
}

function compact<T extends object>(value: T): Partial<T> {
  return Object.fromEntries(
    Object.entries(value).filter(([, v]) => v !== undefined && v !== ''),
  ) as Partial<T>;
}
