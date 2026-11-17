// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { FormControl } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import {
  emptyLimitTexts,
  limitTextsOf,
  limitValidator,
  limitValuesOf,
  parseLimitText,
  sameLimitValues,
  unlimitedOf,
} from './limit-rules';

describe('parseLimitText', () => {
  it.each([
    ['', undefined],
    ['   ', undefined],
    ['0', 0],
    ['10', 10],
    [' 42 ', 42],
  ])('parses %j as %j', (text, expected) => {
    expect(parseLimitText(text)).toBe(expected);
  });

  it.each(['-1', '1.5', 'abc', '1e3'])('rejects %j as not a whole number', (text) => {
    expect(Number.isNaN(parseLimitText(text))).toBe(true);
  });
});

describe('limitValidator', () => {
  it('accepts empty and in-range values', () => {
    expect(limitValidator('maxMonitors')(new FormControl(''))).toBeNull();
    expect(limitValidator('maxMonitors')(new FormControl('0'))).toBeNull();
    expect(limitValidator('minIntervalSeconds')(new FormControl('1'))).toBeNull();
  });

  it('reports a range error below the minimum of minIntervalSeconds and above the maximum', () => {
    expect(limitValidator('minIntervalSeconds')(new FormControl('0'))).toEqual({
      range: { min: 1, max: 1_000_000 },
    });
    expect(limitValidator('maxChannels')(new FormControl('1000001'))).toEqual({
      range: { min: 0, max: 1_000_000 },
    });
  });

  it('reports a non-integer value', () => {
    expect(limitValidator('maxMonitors')(new FormControl('2.5'))).toEqual({ integer: true });
  });
});

describe('limitValuesOf', () => {
  it('leaves empty fields out, sends unlimited as null and numbers as numbers', () => {
    const texts = { ...emptyLimitTexts(), maxMonitors: '10', maxChannels: '' };
    expect(limitValuesOf(texts, { maxStatusPages: true })).toEqual({
      maxMonitors: 10,
      maxStatusPages: null,
    });
  });

  it('ignores a number in a field that is marked unlimited', () => {
    const texts = { ...emptyLimitTexts(), maxMonitors: '10' };
    expect(limitValuesOf(texts, { maxMonitors: true })).toEqual({ maxMonitors: null });
  });

  it('returns an empty object when nothing is set', () => {
    expect(limitValuesOf(emptyLimitTexts())).toEqual({});
  });
});

describe('limitTextsOf and unlimitedOf', () => {
  it('shows numbers as text and null or absent as empty', () => {
    const texts = limitTextsOf({ maxMonitors: 5, maxChannels: null });
    expect(texts.maxMonitors).toBe('5');
    expect(texts.maxChannels).toBe('');
    expect(texts.minIntervalSeconds).toBe('');
  });

  it('flags only the null values as unlimited', () => {
    expect(unlimitedOf({ maxMonitors: null, maxChannels: 3 })).toEqual({ maxMonitors: true });
    expect(unlimitedOf(undefined)).toEqual({});
  });
});

describe('sameLimitValues', () => {
  it('treats absent and null differently (inherit versus unlimited)', () => {
    expect(sameLimitValues({}, {})).toBe(true);
    expect(sameLimitValues({ maxMonitors: null }, { maxMonitors: null })).toBe(true);
    expect(sameLimitValues({}, { maxMonitors: null })).toBe(false);
    expect(sameLimitValues({ maxMonitors: 3 }, { maxMonitors: 4 })).toBe(false);
  });

  it('compares missing and undefined input as equal', () => {
    expect(sameLimitValues(undefined, {})).toBe(true);
  });
});
