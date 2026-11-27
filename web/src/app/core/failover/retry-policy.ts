// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse, HttpRequest } from '@angular/common/http';

/**
 * Which requests the SPA may repeat on another node after a failure (ADR 0036,
 * question 3, option 3B). This file is security code: a retry of an auth call
 * on another node could replay a rotated refresh token and revoke the whole
 * session family, and a retried POST without idempotency key could create a
 * duplicate.
 */

/** Methods that are safe or idempotent by HTTP semantics (RFC 9110 9.2). */
const IDEMPOTENT_METHODS = new Set(['GET', 'HEAD', 'PUT', 'DELETE']);

/** Methods that may be retried only with an `Idempotency-Key` header. */
const KEYED_METHODS = new Set(['POST', 'PATCH']);

/** Status codes that mean "this node cannot serve now, another one may". */
const FAILOVER_STATUS = new Set([0, 502, 503, 504]);

const AUTH_PREFIX = '/api/v1/auth';

/**
 * Reports whether `req` may be sent again to another node:
 *
 * - never for `/api/v1/auth` and everything below it (refresh, login, logout);
 * - never for URLs outside `/api/`;
 * - always for GET, HEAD, PUT and DELETE;
 * - POST and PATCH only with a non-empty `Idempotency-Key` header, which the
 *   FSM deduplicates (ADR 0012 A8);
 * - nothing else.
 *
 * `req.url` may be relative or absolute (after the base-URL interceptor).
 */
export function isRetrySafe(req: HttpRequest<unknown>): boolean {
  const path = apiPath(req.url);
  if (path === null || isAuthPath(path)) {
    return false;
  }
  const method = req.method.toUpperCase();
  if (IDEMPOTENT_METHODS.has(method)) {
    return true;
  }
  if (KEYED_METHODS.has(method)) {
    return (req.headers.get('Idempotency-Key') ?? '').trim() !== '';
  }
  return false;
}

/**
 * Reports whether `err` is a failure another node may not have: a network
 * error (status 0, which includes a CORS rejection), a gateway error
 * (502/503/504) or an RxJS timeout. A 4xx never is, 401 and 403 included.
 */
export function isFailoverError(err: unknown): boolean {
  if (err instanceof HttpErrorResponse) {
    return FAILOVER_STATUS.has(err.status);
  }
  return err instanceof Error && err.name === 'TimeoutError';
}

/**
 * Returns the decoded, dot-segment-free path of an API URL, or `null` if the
 * URL is not under `/api/` or cannot be parsed. The backend router decodes
 * percent-escapes before matching, so the check must see the same path.
 */
function apiPath(url: string): string | null {
  let path: string;
  try {
    path = new URL(url, 'https://placeholder.invalid').pathname;
    // Decoded "%2f" or "%2e" may form new dot segments; resolve them again.
    path = decodeURIComponent(path).replace(/\/{2,}/g, '/');
    path = new URL(path, 'https://placeholder.invalid').pathname;
  } catch {
    return null;
  }
  return path.startsWith('/api/') ? path : null;
}

function isAuthPath(path: string): boolean {
  const p = path.toLowerCase();
  return p === AUTH_PREFIX || p.startsWith(AUTH_PREFIX + '/');
}
