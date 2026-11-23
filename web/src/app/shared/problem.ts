// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { AbstractControl, FormGroup } from '@angular/forms';
import { FieldError, Problem } from '../api/gen/model';

/**
 * Reads the RFC 9457 problem details of a failed HTTP call (application/problem+json).
 * Returns null for anything else (network errors, non-problem bodies), so callers
 * can fall back to a generic message.
 */
export function problemOf(err: unknown): Problem | null {
  if (!(err instanceof HttpErrorResponse)) {
    return null;
  }
  const body: unknown = err.error;
  if (typeof body !== 'object' || body === null) {
    return null;
  }
  const p = body as Partial<Problem>;
  if (typeof p.type !== 'string' || typeof p.title !== 'string' || typeof p.status !== 'number') {
    return null;
  }
  const errors = Array.isArray(p.errors) ? p.errors.filter(isFieldError) : undefined;
  return {
    type: p.type,
    title: p.title,
    status: p.status,
    detail: typeof p.detail === 'string' ? p.detail : undefined,
    instance: typeof p.instance === 'string' ? p.instance : undefined,
    errors,
  };
}

/**
 * Puts the server's field errors onto the matching form controls as
 * `{ server: message }`. A field is addressed by its JSON pointer
 * (`/config/url` maps to `config` -> `url`). Unknown fields are skipped.
 * The error clears when the user changes the value, because Angular re-validates.
 *
 * @returns the number of controls that received an error
 */
export function applyFieldErrors(form: FormGroup, problem: Problem): number {
  let applied = 0;
  for (const fieldError of problem.errors ?? []) {
    const control = findControl(form, fieldError.field);
    if (control) {
      control.setErrors({ ...control.errors, server: fieldError.message });
      control.markAsTouched();
      applied++;
    }
  }
  return applied;
}

function isFieldError(e: unknown): e is FieldError {
  return (
    typeof e === 'object' &&
    e !== null &&
    typeof (e as FieldError).field === 'string' &&
    typeof (e as FieldError).message === 'string'
  );
}

/** Resolves a JSON pointer (RFC 6901) against a form group. */
function findControl(form: FormGroup, pointer: string): AbstractControl | null {
  if (!pointer.startsWith('/')) {
    return null;
  }
  const segments = pointer
    .slice(1)
    .split('/')
    .map((s) => s.replaceAll('~1', '/').replaceAll('~0', '~'));
  if (segments.includes('')) {
    return null;
  }
  return form.get(segments);
}
