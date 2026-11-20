// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { RelativeTimePipe, formatRelative } from './relative-time.pipe';

const NOW = Date.parse('2026-10-21T12:00:00Z');

describe('formatRelative', () => {
  it.each<[string, number, string]>([
    ['now for a few seconds', -2_000, 'now'],
    ['seconds ago', -30_000, '30 seconds ago'],
    ['minutes ago', -3 * 60_000, '3 minutes ago'],
    ['hours ago', -5 * 3_600_000, '5 hours ago'],
    ['days ago', -2 * 86_400_000, '2 days ago'],
    ['yesterday with numeric auto', -86_400_000, 'yesterday'],
    ['in the future', 10 * 60_000, 'in 10 minutes'],
  ])('%s', (_name, offsetMs, expected) => {
    expect(formatRelative(NOW + offsetMs, NOW, 'en')).toBe(expected);
  });

  it('follows the requested language', () => {
    expect(formatRelative(NOW - 3 * 60_000, NOW, 'de')).toBe('vor 3 Minuten');
  });
});

describe('RelativeTimePipe', () => {
  let pipe: RelativeTimePipe;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
    });
    pipe = TestBed.runInInjectionContext(() => new RelativeTimePipe());
  });

  it('formats an ISO timestamp with the given language', () => {
    const ts = new Date(Date.now() - 3 * 60_000).toISOString();

    expect(pipe.transform(ts, 'de')).toBe('vor 3 Minuten');
  });

  it('accepts a Date', () => {
    expect(pipe.transform(new Date(Date.now() - 2 * 3_600_000), 'en')).toBe('2 hours ago');
  });

  it('returns an empty string for missing or invalid input', () => {
    expect(pipe.transform(null)).toBe('');
    expect(pipe.transform(undefined)).toBe('');
    expect(pipe.transform('not a date', 'en')).toBe('');
  });
});
