// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';

/** Maps a failed auth call to a translation key; server texts are not shown verbatim. */
export function authErrorKey(err: unknown): { key: string; retryAfter?: number } {
  if (!(err instanceof HttpErrorResponse)) {
    return { key: 'auth.error.unexpected' };
  }
  switch (err.status) {
    case 0:
      return { key: 'auth.error.offline' };
    case 401:
      return { key: 'auth.error.invalidCredentials' };
    case 403:
      return { key: 'auth.error.setupToken' };
    case 404:
      return { key: 'auth.error.localDisabled' };
    case 409:
      return { key: 'auth.error.setupDone' };
    case 422:
      return { key: 'auth.error.weakPassword' };
    case 429:
      return {
        key: 'auth.error.throttled',
        retryAfter: Number(err.headers.get('Retry-After')) || undefined,
      };
    case 503:
      return { key: 'auth.error.unavailable' };
    default:
      return { key: 'auth.error.unexpected' };
  }
}
