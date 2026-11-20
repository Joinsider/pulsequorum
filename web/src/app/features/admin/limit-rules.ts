// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { AbstractControl, ValidationErrors, ValidatorFn } from '@angular/forms';

/**
 * Plan limits (FA-TEN-04, ADR 0029). Shared by the plan and tenant dialogs.
 * Bounds mirror the OpenAPI schema `Limits`; the server validates again.
 */

export const LIMIT_KEYS = [
  'maxMonitors',
  'minIntervalSeconds',
  'maxChannels',
  'maxStatusPages',
  'maxPrivateProbes',
] as const;

export type LimitKey = (typeof LIMIT_KEYS)[number];

/** Limits the server stores but does not enforce yet (their features come later, ADR 0029). */
export const NOT_ENFORCED_YET: readonly LimitKey[] = ['maxStatusPages', 'maxPrivateProbes'];

export const LIMIT_BOUNDS: Readonly<
  Record<LimitKey, { readonly min: number; readonly max: number }>
> = {
  maxMonitors: { min: 0, max: 1_000_000 },
  minIntervalSeconds: { min: 1, max: 1_000_000 },
  maxChannels: { min: 0, max: 1_000_000 },
  maxStatusPages: { min: 0, max: 1_000_000 },
  maxPrivateProbes: { min: 0, max: 1_000_000 },
};

/** Text of the form fields: '' means "no value" (inherit or unlimited, depending on the dialog). */
export type LimitTexts = Record<LimitKey, string>;

/** Limit values as the API takes them: a number, null (unlimited) or absent (not set). */
export type LimitValues = Partial<Record<LimitKey, number | null>>;

export function emptyLimitTexts(): LimitTexts {
  return {
    maxMonitors: '',
    minIntervalSeconds: '',
    maxChannels: '',
    maxStatusPages: '',
    maxPrivateProbes: '',
  };
}

/**
 * Parses a field text into a whole number. Returns undefined for an empty text
 * and NaN for anything that is not a whole number.
 */
export function parseLimitText(text: string): number | undefined {
  const trimmed = text.trim();
  if (trimmed === '') {
    return undefined;
  }
  if (!/^\d+$/.test(trimmed)) {
    return Number.NaN;
  }
  return Number(trimmed);
}

/**
 * Validator for one limit field: empty is always valid, otherwise a whole
 * number within the bounds of the key.
 */
export function limitValidator(key: LimitKey): ValidatorFn {
  const { min, max } = LIMIT_BOUNDS[key];
  return (control: AbstractControl): ValidationErrors | null => {
    const value = parseLimitText(String(control.value ?? ''));
    if (value === undefined) {
      return null;
    }
    if (Number.isNaN(value)) {
      return { integer: true };
    }
    return value < min || value > max ? { range: { min, max } } : null;
  };
}

/**
 * Limit values for the API from the field texts. A field with `unlimited` set
 * becomes null, an empty field is left out (inherit or no value), a number is kept.
 */
export function limitValuesOf(
  texts: LimitTexts,
  unlimited: Partial<Record<LimitKey, boolean>> = {},
): LimitValues {
  const values: LimitValues = {};
  for (const key of LIMIT_KEYS) {
    if (unlimited[key]) {
      values[key] = null;
      continue;
    }
    const parsed = parseLimitText(texts[key]);
    if (parsed !== undefined && !Number.isNaN(parsed)) {
      values[key] = parsed;
    }
  }
  return values;
}

/** Field texts that show the given values. Missing values become '', null becomes '' too. */
export function limitTextsOf(values: LimitValues | undefined): LimitTexts {
  const texts = emptyLimitTexts();
  for (const key of LIMIT_KEYS) {
    const value = values?.[key];
    texts[key] = typeof value === 'number' ? String(value) : '';
  }
  return texts;
}

/** Which limits a dialog shows as unlimited: a null value. */
export function unlimitedOf(values: LimitValues | undefined): Partial<Record<LimitKey, boolean>> {
  const flags: Partial<Record<LimitKey, boolean>> = {};
  for (const key of LIMIT_KEYS) {
    if (values?.[key] === null) {
      flags[key] = true;
    }
  }
  return flags;
}

/**
 * True if both value sets say the same for every limit. Absent equals absent,
 * null (unlimited) equals null, and null differs from absent (inherit).
 */
export function sameLimitValues(a: LimitValues | undefined, b: LimitValues | undefined): boolean {
  return LIMIT_KEYS.every((key) => a?.[key] === b?.[key]);
}

/** Translation key for a limit's name. */
export function limitLabelKey(key: LimitKey): string {
  return `admin.limits.fields.${key}`;
}
