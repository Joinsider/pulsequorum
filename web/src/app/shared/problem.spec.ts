// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { FormControl, FormGroup, Validators } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import { Problem } from '../api/gen/model';
import { applyFieldErrors, problemOf } from './problem';

function httpError(body: unknown, status = 400): HttpErrorResponse {
  return new HttpErrorResponse({ error: body, status, url: '/api/v1/monitors' });
}

const PROBLEM: Problem = {
  type: 'about:blank',
  title: 'Validation failed',
  status: 422,
  detail: 'Some fields are invalid',
  errors: [
    { field: '/name', message: 'name is taken' },
    { field: '/config/url', message: 'must be https' },
    { field: '/unknown', message: 'no such control' },
  ],
};

describe('problemOf', () => {
  it('reads an RFC 9457 body', () => {
    expect(problemOf(httpError(PROBLEM, 422))).toEqual(PROBLEM);
  });

  it('returns null for errors without a problem body', () => {
    expect(problemOf(httpError(null, 0))).toBeNull();
    expect(problemOf(httpError('<html>bad gateway</html>', 502))).toBeNull();
    expect(problemOf(httpError({ message: 'not a problem' }))).toBeNull();
  });

  it('returns null for non-HTTP errors', () => {
    expect(problemOf(new Error('boom'))).toBeNull();
    expect(problemOf(undefined)).toBeNull();
  });

  it('drops malformed field errors', () => {
    const problem = problemOf(
      httpError({
        type: 'about:blank',
        title: 'Validation failed',
        status: 422,
        errors: [{ field: '/name', message: 'x' }, { field: 5 }, 'junk'],
      }),
    );

    expect(problem?.errors).toEqual([{ field: '/name', message: 'x' }]);
  });
});

describe('applyFieldErrors', () => {
  function setup() {
    const name = new FormControl('', { nonNullable: true, validators: [Validators.required] });
    const url = new FormControl('', { nonNullable: true });
    const form = new FormGroup({ name, config: new FormGroup({ url }) });
    return { form, name, url };
  }

  it('sets server errors on matching controls, including nested ones', () => {
    const { form, name, url } = setup();
    const applied = applyFieldErrors(form, PROBLEM);

    expect(applied).toBe(2);
    expect(name.errors).toMatchObject({ server: 'name is taken' });
    expect(url.errors).toEqual({ server: 'must be https' });
    expect(name.touched).toBe(true);
  });

  it('keeps client-side errors and clears the server error on change', () => {
    const { form, name } = setup();
    name.setErrors({ required: true });
    applyFieldErrors(form, { ...PROBLEM, errors: [{ field: '/name', message: 'taken' }] });

    expect(name.errors).toEqual({ required: true, server: 'taken' });

    name.setValue('new');
    expect(name.errors).toBeNull();
  });

  it('ignores pointers that do not start with a slash or point to nothing', () => {
    const { form } = setup();
    const applied = applyFieldErrors(form, {
      ...PROBLEM,
      errors: [
        { field: 'name', message: 'no slash' },
        { field: '/', message: 'empty' },
        { field: '/config/missing', message: 'gone' },
      ],
    });

    expect(applied).toBe(0);
  });

  it('decodes JSON pointer escapes', () => {
    const slash = new FormControl('', { nonNullable: true });
    const form = new FormGroup({ 'a/b': slash });
    applyFieldErrors(form, { ...PROBLEM, errors: [{ field: '/a~1b', message: 'escaped' }] });

    expect(slash.errors).toEqual({ server: 'escaped' });
  });
});
