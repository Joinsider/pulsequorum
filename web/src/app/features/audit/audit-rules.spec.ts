// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import {
  AUDIT_PAGE_SIZE,
  AuditFilter,
  EMPTY_AUDIT_FILTER,
  actorKindKey,
  auditErrorKey,
  buildAuditParams,
  formatAuditTime,
  shortRequestId,
} from './audit-rules';

function filter(overrides: Partial<AuditFilter>): AuditFilter {
  return { ...EMPTY_AUDIT_FILTER, ...overrides };
}

/** Local day boundaries as the browser sees them, so the tests do not depend on the time zone. */
function localIso(y: number, m: number, d: number, h: number, min: number, s: number): string {
  return new Date(y, m - 1, d, h, min, s).toISOString().replace('.000Z', 'Z');
}

function problem(status: number): HttpErrorResponse {
  return new HttpErrorResponse({
    status,
    error: { type: 'about:blank', title: 'Problem', status },
  });
}

describe('buildAuditParams', () => {
  it.each([
    {
      name: 'empty filter sends only the page size',
      input: EMPTY_AUDIT_FILTER,
      cursor: undefined,
      expected: { limit: AUDIT_PAGE_SIZE },
    },
    {
      name: 'whitespace-only fields are dropped',
      input: filter({ action: '   ', objectKind: '\t', actor: ' ' }),
      cursor: undefined,
      expected: { limit: AUDIT_PAGE_SIZE },
    },
    {
      name: 'text fields are trimmed',
      input: filter({ action: '  monitor.update ', objectId: ' 6f1c ', actor: ' bob ' }),
      cursor: undefined,
      expected: {
        limit: AUDIT_PAGE_SIZE,
        action: 'monitor.update',
        objectId: '6f1c',
        actor: 'bob',
      },
    },
    {
      name: 'the cursor is passed on and trimmed',
      input: EMPTY_AUDIT_FILTER,
      cursor: ' c2 ',
      expected: { limit: AUDIT_PAGE_SIZE, cursor: 'c2' },
    },
    {
      name: 'an empty cursor is dropped',
      input: EMPTY_AUDIT_FILTER,
      cursor: '',
      expected: { limit: AUDIT_PAGE_SIZE },
    },
    {
      name: 'tenant is passed for the platform view',
      input: filter({ tenant: ' _system ' }),
      cursor: undefined,
      expected: { limit: AUDIT_PAGE_SIZE, tenant: '_system' },
    },
    {
      name: 'malformed days are dropped',
      input: filter({ from: '19.11.2026', to: 'yesterday' }),
      cursor: undefined,
      expected: { limit: AUDIT_PAGE_SIZE },
    },
  ])('$name', ({ input, cursor, expected }) => {
    expect(buildAuditParams(input, cursor)).toEqual(expected);
  });

  it('turns from into the start and to into the end of its local day', () => {
    const params = buildAuditParams(filter({ from: '2026-11-01', to: '2026-11-03' }));

    expect(params.from).toBe(localIso(2026, 11, 1, 0, 0, 0));
    expect(params.to).toBe(localIso(2026, 11, 3, 23, 59, 59));
    expect(params.from).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  });
});

describe('auditErrorKey', () => {
  it.each([
    { name: 'bad request', err: problem(400), key: 'audit.error.invalid' },
    { name: 'unprocessable', err: problem(422), key: 'audit.error.invalid' },
    { name: 'forbidden', err: problem(403), key: 'audit.error.forbidden' },
    { name: 'unavailable', err: problem(503), key: 'audit.error.unavailable' },
    { name: 'server error', err: problem(500), key: 'audit.error.generic' },
    {
      name: 'network failure',
      err: new HttpErrorResponse({ status: 0 }),
      key: 'audit.error.generic',
    },
    { name: 'plain error', err: new Error('boom'), key: 'audit.error.generic' },
  ])('maps $name to $key', ({ err, key }) => {
    expect(auditErrorKey(err)).toBe(key);
  });
});

describe('actorKindKey', () => {
  it.each([
    { kind: 'user', key: 'audit.actorKind.user' },
    { kind: 'api_token', key: 'audit.actorKind.apiToken' },
    { kind: 'system', key: 'audit.actorKind.system' },
    { kind: 'unknown', key: 'audit.actorKind.unknown' },
    { kind: 'something-new', key: 'audit.actorKind.unknown' },
  ])('maps $kind to $key', ({ kind, key }) => {
    expect(actorKindKey(kind)).toBe(key);
  });
});

describe('shortRequestId', () => {
  it.each([
    { id: '0123456789abcdef', short: '01234567' },
    { id: 'abc', short: 'abc' },
    { id: '', short: '' },
  ])('shortens $id to $short', ({ id, short }) => {
    expect(shortRequestId(id)).toBe(short);
  });
});

describe('formatAuditTime', () => {
  it('formats a valid timestamp', () => {
    const iso = '2026-11-19T10:00:00Z';
    expect(formatAuditTime(iso, 'en')).toBe(
      new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'medium' }).format(
        new Date(iso),
      ),
    );
  });

  it('returns unparseable input unchanged', () => {
    expect(formatAuditTime('not a date', 'en')).toBe('not a date');
  });
});
