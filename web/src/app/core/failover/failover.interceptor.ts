// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  HttpClient,
  HttpContextToken,
  HttpErrorResponse,
  HttpInterceptorFn,
  HttpResponse,
} from '@angular/common/http';
import { inject } from '@angular/core';
import { catchError, of, switchMap, tap, throwError, timer } from 'rxjs';
import { NodeSelector } from './node-selector.service';
import { isFailoverError, isRetrySafe } from './retry-policy';

/** Set on the one retry of a request, so that a request switches nodes at most once. */
export const FAILOVER_RETRIED = new HttpContextToken<boolean>(() => false);

/** Longest wait honored from a `Retry-After` header (ADR 0036, question 3). */
export const MAX_RETRY_AFTER_MS = 5_000;

/**
 * Reports failed requests to the {@link NodeSelector} and repeats safe ones on
 * the node it switched to (ADR 0036, question 3, option 3B).
 *
 * It runs after the base-URL interceptor, so `req.url` is absolute here. A
 * retry is issued through `HttpClient` with the original relative URL, so the
 * auth and base-URL interceptors apply again: a fresh token and the new node.
 * Auth paths, 4xx responses and POST or PATCH requests without an
 * `Idempotency-Key` are never repeated, but the failure is still reported.
 */
export const failoverInterceptor: HttpInterceptorFn = (req, next) => {
  const selector = inject(NodeSelector);
  const http = inject(HttpClient);
  const target = nodeOf(req.url, selector.nodes());

  return next(req).pipe(
    tap((event) => {
      if (event instanceof HttpResponse && target !== '') {
        selector.reportSuccess(target);
      }
    }),
    catchError((err: unknown) => {
      if (target === '' || !isFailoverError(err)) {
        return throwError(() => err);
      }
      selector.reportFailure(target);

      const nextNode = selector.current();
      const mayRetry =
        !req.context.get(FAILOVER_RETRIED) &&
        isRetrySafe(req) &&
        nextNode !== '' &&
        nextNode !== target &&
        req.url.startsWith(target + '/api/');
      if (!mayRetry) {
        return throwError(() => err);
      }

      const retry = req.clone({
        url: req.url.slice(target.length),
        headers: req.headers.delete('Authorization'),
        context: req.context.set(FAILOVER_RETRIED, true),
      });
      const delay =
        err instanceof HttpErrorResponse && err.status === 503
          ? retryAfterMs(err.headers.get('Retry-After'))
          : 0;
      return (delay > 0 ? timer(delay) : of(0)).pipe(switchMap(() => http.request(retry)));
    }),
  );
};

/**
 * Milliseconds to wait for a `Retry-After` value (seconds or HTTP date), capped
 * at {@link MAX_RETRY_AFTER_MS}. Missing or unparsable values give 0.
 */
export function retryAfterMs(value: string | null): number {
  const trimmed = value?.trim() ?? '';
  if (trimmed === '') {
    return 0;
  }
  const ms = /^\d+$/.test(trimmed) ? Number(trimmed) * 1000 : Date.parse(trimmed) - Date.now();
  if (!Number.isFinite(ms)) {
    return 0;
  }
  return Math.min(MAX_RETRY_AFTER_MS, Math.max(0, ms));
}

/** The node origin of an absolute URL, or `''` if it is not one of the selector's nodes. */
function nodeOf(url: string, nodes: readonly string[]): string {
  if (!/^https?:\/\//i.test(url)) {
    return '';
  }
  try {
    // Node origins are stored in the form `URL.origin` produces, so they compare directly.
    const origin = new URL(url).origin;
    return nodes.includes(origin) ? origin : '';
  } catch {
    return '';
  }
}
