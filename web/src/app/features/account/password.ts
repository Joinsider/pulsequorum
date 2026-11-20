// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { AbstractControl, ValidationErrors } from '@angular/forms';

/** Mirrors internal/auth/password.go (MinPasswordLen, MaxPasswordLen). */
export const MIN_PASSWORD_LENGTH = 12;
export const MAX_PASSWORD_LENGTH = 256;

/** Group validator: the new password and its repetition must be equal. */
export function passwordsMatch(group: AbstractControl): ValidationErrors | null {
  const password = group.get('newPassword')?.value;
  const repeat = group.get('repeatPassword')?.value;
  return password !== repeat ? { mismatch: true } : null;
}

/** A translated failure of the password change, with the wait time for throttling. */
export interface PasswordFailure {
  key: string;
  retryAfter?: number;
}

/**
 * Maps a failed PUT /me/password to a translation key. The server text is not
 * shown: a 422 means either a wrong current password or a rejected new one.
 */
export function passwordFailure(err: unknown): PasswordFailure {
  if (!(err instanceof HttpErrorResponse)) {
    return { key: 'account.password.error.unexpected' };
  }
  switch (err.status) {
    case 0:
      return { key: 'account.password.error.offline' };
    case 422: {
      const detail = typeof err.error?.detail === 'string' ? err.error.detail : '';
      return /current/i.test(detail)
        ? { key: 'account.password.error.currentWrong' }
        : { key: 'account.password.error.invalid' };
    }
    case 429: {
      const seconds = Number(err.headers.get('Retry-After'));
      return seconds > 0
        ? { key: 'account.password.error.throttled', retryAfter: seconds }
        : { key: 'account.password.error.throttledGeneric' };
    }
    default:
      return { key: 'account.password.error.unexpected' };
  }
}
