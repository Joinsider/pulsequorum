// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { TranslocoPipe } from '@jsverse/transloco';
import { MonitorStatus } from '../../api/gen/model';
import { statusColorVar } from './status-colors';

/**
 * Pill showing a monitor status with its translated label (`status.<STATUS>`).
 * The status is never conveyed by colour alone: the label carries the meaning.
 *
 * @example <app-status-badge [status]="monitor.status" />
 */
@Component({
  selector: 'app-status-badge',
  imports: [TranslocoPipe],
  template: `{{ 'status.' + status() | transloco }}`,
  styles: `
    :host {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.125rem 0.625rem;
      border-radius: 999px;
      font-size: 0.8125rem;
      font-weight: 600;
      line-height: 1.5;
      white-space: nowrap;
      color: var(--badge-color);
      background: color-mix(in srgb, var(--badge-color) 14%, transparent);
    }
  `,
  host: {
    '[style.--badge-color]': 'color()',
  },
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class StatusBadge {
  readonly status = input.required<MonitorStatus>();

  protected readonly color = computed(() => statusColorVar(this.status()));
}
