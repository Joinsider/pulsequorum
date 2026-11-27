// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse, HttpHeaders, HttpRequest } from '@angular/common/http';
import { TimeoutError } from 'rxjs';
import { describe, expect, it } from 'vitest';
import { isFailoverError, isRetrySafe } from './retry-policy';

function request(method: string, url: string, idempotencyKey?: string): HttpRequest<unknown> {
  const headers =
    idempotencyKey === undefined
      ? new HttpHeaders()
      : new HttpHeaders({ 'Idempotency-Key': idempotencyKey });
  return new HttpRequest<unknown>(method, url, null, { headers });
}

describe('isRetrySafe', () => {
  const cases: [string, string, string | undefined, boolean][] = [
    ['GET', '/api/v1/monitors', undefined, true],
    ['HEAD', '/api/v1/monitors', undefined, true],
    ['PUT', '/api/v1/monitors/m1', undefined, true],
    ['DELETE', '/api/v1/monitors/m1', undefined, true],
    ['GET', 'https://n2.example.com/api/v1/monitors', undefined, true],
    ['POST', '/api/v1/monitors', undefined, false],
    ['POST', '/api/v1/monitors', '', false],
    ['POST', '/api/v1/monitors', '   ', false],
    ['POST', '/api/v1/monitors', 'k-1', true],
    ['PATCH', '/api/v1/monitors/m1', 'k-1', true],
    ['PATCH', '/api/v1/monitors/m1', undefined, false],
    ['OPTIONS', '/api/v1/monitors', undefined, false],
    ['TRACE', '/api/v1/monitors', undefined, false],
    // Auth is never retried, whatever the method or key.
    ['POST', '/api/v1/auth/refresh', 'k-1', false],
    ['POST', '/api/v1/auth/login', 'k-1', false],
    ['GET', '/api/v1/auth', undefined, false],
    ['GET', '/api/v1/auth/oidc/callback', undefined, false],
    ['POST', 'https://n2.example.com/api/v1/auth/refresh', 'k-1', false],
    ['POST', '/api/v1/AUTH/refresh', 'k-1', false],
    ['POST', '/api/v1//auth/refresh', 'k-1', false],
    ['POST', '/api/v1/x/../auth/refresh', 'k-1', false],
    ['POST', '/api/v1/%61uth/refresh', 'k-1', false],
    ['POST', '/api/v1/x%2f..%2fauth/refresh', 'k-1', false],
    ['POST', '/api/v1/%2e%2e/v1/auth/refresh', 'k-1', false],
    ['GET', '/api/v1/authors', undefined, true],
    // Only our API.
    ['GET', '/config.json', undefined, false],
    ['GET', 'https://id.example.com/.well-known/openid-configuration', undefined, false],
    ['GET', '/api/v1/%zz', undefined, false],
  ];
  it.each(cases)('%s %s (key %j) -> %s', (method, url, key, want) => {
    expect(isRetrySafe(request(method, url, key))).toBe(want);
  });
});

describe('isFailoverError', () => {
  const status = (s: number) => new HttpErrorResponse({ status: s });

  it.each([0, 502, 503, 504])('fails over on status %i', (s) => {
    expect(isFailoverError(status(s))).toBe(true);
  });

  it.each([400, 401, 403, 404, 409, 422, 429, 500, 501])('does not fail over on status %i', (s) => {
    expect(isFailoverError(status(s))).toBe(false);
  });

  it('fails over on an RxJS timeout', () => {
    expect(isFailoverError(new TimeoutError())).toBe(true);
  });

  it('does not fail over on other errors', () => {
    expect(isFailoverError(new Error('boom'))).toBe(false);
    expect(isFailoverError('timeout')).toBe(false);
    expect(isFailoverError(undefined)).toBe(false);
  });
});
