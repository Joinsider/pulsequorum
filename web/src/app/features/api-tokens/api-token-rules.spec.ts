// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { ApiToken } from '../../api/gen/model';
import { apiTokenErrorKey, sortTokens } from './api-token-rules';

function problem(status: number): HttpErrorResponse {
  return new HttpErrorResponse({
    status,
    error: { type: 'about:blank', title: 'Problem', status },
  });
}

function token(id: string, createdAt: string, expired = false): ApiToken {
  return {
    id,
    name: id,
    role: 'tenant-viewer',
    createdAt,
    expiresAt: '2030-01-01T00:00:00Z',
    expired,
    createdBy: { kind: 'user', id: 'me', name: 'admin' },
  };
}

describe('apiTokenErrorKey', () => {
  it.each([
    [403, 'apiTokens.error.forbidden'],
    [404, 'apiTokens.error.notFound'],
    [409, 'apiTokens.error.conflict'],
    [503, 'apiTokens.error.unavailable'],
    [500, 'apiTokens.error.generic'],
    [422, 'apiTokens.error.generic'],
  ])('maps problem status %i', (status, key) => {
    expect(apiTokenErrorKey(problem(status))).toBe(key);
  });

  it('falls back to the generic message for non-problem errors', () => {
    expect(apiTokenErrorKey(new HttpErrorResponse({ status: 403 }))).toBe(
      'apiTokens.error.generic',
    );
    expect(apiTokenErrorKey(new Error('boom'))).toBe('apiTokens.error.generic');
  });
});

describe('sortTokens', () => {
  it('lists active tokens before expired ones, newest first within each group', () => {
    const tokens = [
      token('old-active', '2026-01-01T00:00:00Z'),
      token('expired-new', '2026-06-01T00:00:00Z', true),
      token('new-active', '2026-03-01T00:00:00Z'),
      token('expired-old', '2025-01-01T00:00:00Z', true),
    ];

    expect(sortTokens(tokens).map((t) => t.id)).toEqual([
      'new-active',
      'old-active',
      'expired-new',
      'expired-old',
    ]);
  });

  it.each([
    ['empty list', [], []],
    ['single token', ['a'], ['a']],
  ])('handles %s', (_label, ids, expected) => {
    const tokens = ids.map((id) => token(id, '2026-01-01T00:00:00Z'));
    expect(sortTokens(tokens).map((t) => t.id)).toEqual(expected);
  });

  it('does not mutate the input', () => {
    const tokens = [token('a', '2026-01-01T00:00:00Z'), token('b', '2026-02-01T00:00:00Z')];
    const copy = [...tokens];

    sortTokens(tokens);

    expect(tokens).toEqual(copy);
  });
});
