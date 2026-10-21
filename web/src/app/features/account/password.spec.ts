// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse, HttpHeaders } from '@angular/common/http';
import { FormControl, FormGroup } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import { passwordFailure, passwordsMatch } from './password';

function httpError(status: number, body: unknown = null, retryAfter?: string) {
  const headers = new HttpHeaders(retryAfter ? { 'Retry-After': retryAfter } : {});
  return new HttpErrorResponse({ status, error: body, headers });
}

describe('passwordsMatch', () => {
  function group(newPassword: string, repeatPassword: string) {
    return new FormGroup({
      newPassword: new FormControl(newPassword),
      repeatPassword: new FormControl(repeatPassword),
    });
  }

  it.each([
    ['same', 'same', null],
    ['one', 'two', { mismatch: true }],
  ])('compares %s with %s', (a, b, expected) => {
    expect(passwordsMatch(group(a, b))).toEqual(expected);
  });
});

describe('passwordFailure', () => {
  it.each([
    [httpError(0), 'account.password.error.offline', undefined],
    [
      httpError(422, { detail: 'current password is wrong' }),
      'account.password.error.currentWrong',
      undefined,
    ],
    [
      httpError(422, { detail: 'password must have 12 to 256 characters' }),
      'account.password.error.invalid',
      undefined,
    ],
    [httpError(429, null, '42'), 'account.password.error.throttled', 42],
    [httpError(429), 'account.password.error.throttledGeneric', undefined],
    [httpError(500), 'account.password.error.unexpected', undefined],
    [new Error('boom'), 'account.password.error.unexpected', undefined],
  ])('maps the failure %#', (err, key, retryAfter) => {
    expect(passwordFailure(err)).toEqual(retryAfter === undefined ? { key } : { key, retryAfter });
  });
});
