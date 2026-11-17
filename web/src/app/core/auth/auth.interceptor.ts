// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  HttpContextToken,
  HttpErrorResponse,
  HttpInterceptorFn,
  HttpRequest,
} from '@angular/common/http';
import { inject } from '@angular/core';
import { catchError, from, switchMap, throwError } from 'rxjs';
import { AuthService, SKIP_AUTH } from './auth.service';

/** Set on the retry after a refresh, so a request is retried at most once. */
const RETRIED = new HttpContextToken<boolean>(() => false);

/**
 * Adds the bearer token to calls of our own API. It must run before the
 * base-URL interceptor: only relative `/api/` URLs (as the generated client
 * produces) are matched, so the token never reaches another host. A 401 is
 * answered with one shared refresh and a single retry.
 */
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  if (!req.url.startsWith('/api/') || req.context.get(SKIP_AUTH)) {
    return next(req);
  }
  const auth = inject(AuthService);
  const withToken = (r: HttpRequest<unknown>) => {
    const token = auth.accessToken();
    return token ? r.clone({ setHeaders: { Authorization: `Bearer ${token}` } }) : r;
  };

  return next(withToken(req)).pipe(
    catchError((err: unknown) => {
      if (
        !(err instanceof HttpErrorResponse) ||
        err.status !== 401 ||
        req.context.get(RETRIED) ||
        !auth.authenticated()
      ) {
        return throwError(() => err);
      }
      return from(auth.refresh()).pipe(
        switchMap((ok) => {
          if (!ok) {
            void auth.sessionExpired();
            return throwError(() => err);
          }
          return next(withToken(req.clone({ context: req.context.set(RETRIED, true) }))).pipe(
            catchError((retryErr: unknown) => {
              // OIDC: the freshly refreshed token was rejected too, so the session is over.
              if (
                auth.mode() === 'oidc' &&
                retryErr instanceof HttpErrorResponse &&
                retryErr.status === 401
              ) {
                void auth.sessionExpired();
              }
              return throwError(() => retryErr);
            }),
          );
        }),
      );
    }),
  );
};
