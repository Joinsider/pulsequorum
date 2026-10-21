// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { TranslocoPipe, TranslocoService } from '@jsverse/transloco';
import { UptimeBucket } from '../../api/gen/model';

/** Visual state of one bucket, mapped to a colour in the template. */
export type BucketState = 'up' | 'degraded' | 'down' | 'maintenance' | 'unknown';

/** Share of down time from which a bucket counts as down rather than degraded. */
export const DOWN_SHARE_THRESHOLD = 0.05;

/**
 * Decides the colour of a bucket:
 * - no observed time at all: unknown
 * - down time with a down share of at least 5 %: down
 * - any other down time: degraded
 * - only maintenance time: maintenance
 * - otherwise: up
 */
export function bucketState(b: UptimeBucket): BucketState {
  const {
    upSeconds: up,
    downSeconds: down,
    maintenanceSeconds: maint,
    unknownSeconds: unknown,
  } = b;
  const total = up + down + maint + unknown;
  if (total <= 0 || up + down + maint <= 0) {
    return 'unknown';
  }
  if (down > 0) {
    return down / total >= DOWN_SHARE_THRESHOLD ? 'down' : 'degraded';
  }
  if (maint > 0 && up === 0) {
    return 'maintenance';
  }
  return 'up';
}

/**
 * Uptime percentage of a bucket, excluding maintenance and unknown time.
 * Null when there is no up or down time to compare.
 */
export function uptimePercent(b: UptimeBucket): number | null {
  const denominator = b.upSeconds + b.downSeconds;
  return denominator > 0 ? (b.upSeconds / denominator) * 100 : null;
}

/**
 * Thin vertical bars, one per bucket, that flex to the available width.
 * Pure input: the parent loads the buckets. Each bar has a tooltip with the
 * start time and the uptime.
 *
 * @example <app-uptime-bar [buckets]="stats.buckets" />
 */
@Component({
  selector: 'app-uptime-bar',
  imports: [TranslocoPipe],
  template: `
    @for (b of buckets(); track b.start) {
      <span class="bar" [attr.data-state]="stateOf(b)" [title]="tooltip(b)"></span>
    } @empty {
      <span class="empty">{{ 'uptimeBar.noData' | transloco }}</span>
    }
  `,
  styles: `
    :host {
      display: flex;
      align-items: stretch;
      gap: 1px;
      width: 100%;
      height: 2.5rem;
      min-width: 0;
    }
    .bar {
      flex: 1 1 0;
      min-width: 2px;
      border-radius: 1px;
      background: var(--status-unknown);
    }
    .bar[data-state='up'] {
      background: var(--status-up);
    }
    .bar[data-state='degraded'] {
      background: var(--status-degraded);
    }
    .bar[data-state='down'] {
      background: var(--status-down);
    }
    .bar[data-state='maintenance'] {
      background: var(--status-maintenance);
    }
    .empty {
      align-self: center;
      color: var(--muted);
      font-size: 0.875rem;
    }
  `,
  host: {
    role: 'img',
    '[attr.aria-label]': 'label()',
  },
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class UptimeBar {
  private readonly transloco = inject(TranslocoService);

  readonly buckets = input<UptimeBucket[]>([]);

  /**
   * Re-evaluated when the language or its translations change, so tooltips
   * follow them even if they were rendered before the translations loaded.
   */
  private readonly locale = computed(() => {
    this.translation();
    return this.transloco.activeLang();
  });
  private readonly translation = toSignal(this.transloco.selectTranslation(), { initialValue: {} });

  protected readonly label = computed(() => {
    this.locale();
    return this.transloco.translate('uptimeBar.label');
  });

  protected stateOf(b: UptimeBucket): BucketState {
    return bucketState(b);
  }

  protected tooltip(b: UptimeBucket): string {
    const lang = this.locale();
    const time = new Intl.DateTimeFormat(lang, { dateStyle: 'medium', timeStyle: 'short' }).format(
      new Date(b.start),
    );
    const state = bucketState(b);
    let detail: string;
    if (state === 'unknown') {
      detail = this.transloco.translate('uptimeBar.noData');
    } else if (state === 'maintenance') {
      detail = this.transloco.translate('status.MAINTENANCE');
    } else {
      const percent = uptimePercent(b);
      detail =
        percent === null
          ? this.transloco.translate('uptimeBar.noData')
          : this.transloco.translate('uptimeBar.percent', {
              value: new Intl.NumberFormat(lang, { maximumFractionDigits: 2 }).format(percent),
            });
    }
    return this.transloco.translate('uptimeBar.tooltip', { time, detail });
  }
}
