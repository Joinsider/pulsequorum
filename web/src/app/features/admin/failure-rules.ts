// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { problemOf } from '../../shared/problem';

/**
 * Translation key for a failed call of the platform admin area (tenants, plans).
 * The server's problem detail, when there is one, is shown below it by the caller.
 */
export function failureKey(err: unknown): string {
  if (!(err instanceof HttpErrorResponse)) {
    return 'admin.failure.generic';
  }
  switch (err.status) {
    case 0:
      return 'admin.failure.offline';
    case 403:
      return 'admin.failure.forbidden';
    case 404:
      return 'admin.failure.notFound';
    case 409:
      return 'admin.failure.conflict';
    case 422:
      return 'admin.failure.invalid';
    case 503:
      return 'admin.failure.unavailable';
    default:
      return 'admin.failure.generic';
  }
}

/** The problem detail of a failed call (server text, free of secrets), or null. */
export function failureDetail(err: unknown): string | null {
  return problemOf(err)?.detail ?? null;
}
