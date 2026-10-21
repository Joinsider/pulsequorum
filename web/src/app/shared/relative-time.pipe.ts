// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { inject, Pipe, PipeTransform } from '@angular/core';
import { TranslocoService } from '@jsverse/transloco';

const UNITS: ReadonlyArray<{ unit: Intl.RelativeTimeFormatUnit; seconds: number }> = [
  { unit: 'year', seconds: 365 * 24 * 3600 },
  { unit: 'month', seconds: 30 * 24 * 3600 },
  { unit: 'week', seconds: 7 * 24 * 3600 },
  { unit: 'day', seconds: 24 * 3600 },
  { unit: 'hour', seconds: 3600 },
  { unit: 'minute', seconds: 60 },
  { unit: 'second', seconds: 1 },
];

/**
 * Formats an ISO timestamp relative to now, e.g. "3 minutes ago", using
 * Intl.RelativeTimeFormat in the given language (defaults to the active
 * Transloco language). Pass the language as argument so the output follows
 * a language change: `{{ ts | relativeTime: lang }}`.
 *
 * Pure pipe: it does not tick by itself. Re-evaluate on new input.
 */
@Pipe({ name: 'relativeTime' })
export class RelativeTimePipe implements PipeTransform {
  private readonly transloco = inject(TranslocoService);

  transform(value: string | Date | null | undefined, lang?: string): string {
    if (!value) {
      return '';
    }
    const time = typeof value === 'string' ? Date.parse(value) : value.getTime();
    if (Number.isNaN(time)) {
      return '';
    }
    return formatRelative(time, Date.now(), lang ?? this.transloco.getActiveLang());
  }
}

/** Pure formatting, separated for testing. `now` is in milliseconds. */
export function formatRelative(time: number, now: number, lang: string): string {
  const diff = Math.round((time - now) / 1000);
  const rtf = new Intl.RelativeTimeFormat(lang, { numeric: 'auto' });
  const abs = Math.abs(diff);
  if (abs < 5) {
    return rtf.format(0, 'second');
  }
  for (const { unit, seconds } of UNITS) {
    if (abs >= seconds || unit === 'second') {
      return rtf.format(Math.round(diff / seconds), unit);
    }
  }
  return rtf.format(diff, 'second');
}
