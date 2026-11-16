// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component } from '@angular/core';
import { TranslocoPipe } from '@jsverse/transloco';

/**
 * Pill that marks a monitor as flapping (FA-ALR-06): its status changes often,
 * and its alerts are summarized. Shown next to the status badge; the label
 * carries the meaning, so the colour is not the only signal.
 *
 * @example @if (monitor.flapping) { <app-flapping-badge /> }
 */
@Component({
  selector: 'app-flapping-badge',
  imports: [TranslocoPipe],
  template: `{{ 'status.flapping' | transloco }}`,
  styles: `
    :host {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.625rem;
      border: 1px dashed var(--status-degraded);
      border-radius: 999px;
      font-size: 0.8125rem;
      font-weight: 600;
      line-height: 1.5;
      white-space: nowrap;
      color: var(--status-degraded);
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class FlappingBadge {}
