// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, input } from '@angular/core';

/**
 * Page title on the left and projected actions (buttons) on the right. Wraps
 * the actions below the title on narrow screens.
 *
 * @example
 * <app-page-header [title]="'Monitors' | transloco">
 *   <button mat-flat-button>New monitor</button>
 * </app-page-header>
 */
@Component({
  selector: 'app-page-header',
  template: `
    <h1 class="title">{{ title() }}</h1>
    <div class="actions"><ng-content /></div>
  `,
  styles: `
    :host {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem 1rem;
      margin-bottom: 1.25rem;
    }
    .title {
      margin: 0;
      min-width: 0;
      font-size: 1.5rem;
      font-weight: 700;
      letter-spacing: -0.01em;
      line-height: 1.25;
      overflow-wrap: anywhere;
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
      margin-left: auto;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class PageHeader {
  readonly title = input.required<string>();
}
