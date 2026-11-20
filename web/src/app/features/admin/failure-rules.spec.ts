// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { failureDetail, failureKey } from './failure-rules';

function failure(status: number, error?: unknown): HttpErrorResponse {
  return new HttpErrorResponse({ status, error });
}

describe('failureKey', () => {
  it.each([
    [0, 'admin.failure.offline'],
    [403, 'admin.failure.forbidden'],
    [404, 'admin.failure.notFound'],
    [409, 'admin.failure.conflict'],
    [422, 'admin.failure.invalid'],
    [503, 'admin.failure.unavailable'],
    [500, 'admin.failure.generic'],
  ])('maps status %i to %s', (status, key) => {
    expect(failureKey(failure(status))).toBe(key);
  });

  it('falls back to generic for errors that are not HTTP failures', () => {
    expect(failureKey(new Error('boom'))).toBe('admin.failure.generic');
  });
});

describe('failureDetail', () => {
  it('returns the problem detail of a refusal', () => {
    const err = failure(409, {
      type: 'about:blank',
      title: 'Conflict',
      status: 409,
      detail: 'plan still used by 2 tenants',
    });
    expect(failureDetail(err)).toBe('plan still used by 2 tenants');
  });

  it('returns null without a problem body', () => {
    expect(failureDetail(failure(500, '<html>'))).toBeNull();
    expect(failureDetail(new Error('x'))).toBeNull();
  });
});
