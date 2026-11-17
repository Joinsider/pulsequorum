// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, DestroyRef, inject, signal } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { SystemAlert } from '../../api/gen/model';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { detailRows, DetailRow } from './system-alert-rules';
import { SystemAlertsService } from './system-alerts.service';

/** Interval of the background reload while the tab is visible. */
const RELOAD_INTERVAL_MS = 10_000;

/**
 * "Active system alerts" on the cluster page (FA-OBS-01, ADR 0025): what the
 * leader currently reports, with the measured details. Refreshes like the node list.
 */
@Component({
  selector: 'app-system-alerts-section',
  imports: [MatButtonModule, RelativeTimePipe, TranslocoDirective],
  template: `
    <ng-container *transloco="let t">
      <section class="alerts" aria-labelledby="system-alerts-title">
        <h2 id="system-alerts-title" class="alerts__title">{{ t('admin.alerts.title') }}</h2>

        @if (loadError()) {
          <p class="state state--error" role="alert">
            {{ t('admin.alerts.loadError') }}
            <button mat-button type="button" (click)="load()">{{ t('common.retry') }}</button>
          </p>
        } @else if (alerts(); as list) {
          @if (list.length === 0) {
            <p class="state" data-testid="system-alerts-empty">{{ t('admin.alerts.empty') }}</p>
          } @else {
            <ul class="alerts__list" [attr.aria-labelledby]="'system-alerts-title'">
              @for (alert of list; track alert.kind + ':' + alert.subject) {
                <li class="alert" data-testid="system-alert">
                  <div class="alert__head">
                    <span class="alert__kind">{{ t('admin.alerts.kinds.' + alert.kind) }}</span>
                    <code class="alert__code">{{ alert.kind }}</code>
                  </div>
                  <dl class="facts">
                    <div>
                      <dt>{{ t('admin.alerts.subject') }}</dt>
                      <dd class="wrap">{{ alert.subject || '-' }}</dd>
                    </div>
                    <div>
                      <dt>{{ t('admin.alerts.since') }}</dt>
                      <dd>
                        <time [attr.datetime]="alert.since">{{ alert.since | relativeTime }}</time>
                      </dd>
                    </div>
                    <div>
                      <dt>{{ t('admin.alerts.level') }}</dt>
                      <dd>{{ alert.level }}</dd>
                    </div>
                  </dl>
                  @if (rowsOf(alert); as rows) {
                    @if (rows.length > 0) {
                      <dl class="details" [attr.aria-label]="t('admin.alerts.details')">
                        @for (row of rows; track row.key) {
                          <div>
                            <dt>{{ row.key }}</dt>
                            <dd class="wrap">{{ row.value }}</dd>
                          </div>
                        }
                      </dl>
                    }
                  }
                </li>
              }
            </ul>
          }
        } @else if (loading()) {
          <p class="state" role="status">{{ t('common.loading') }}</p>
        }
      </section>
    </ng-container>
  `,
  styles: `
    .alerts {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
      margin: 0 0 1.25rem;
    }
    .alerts__title {
      margin: 0;
      font-size: 1.0625rem;
      font-weight: 650;
    }
    .alerts__list {
      display: grid;
      gap: 0.75rem;
      margin: 0;
      padding: 0;
      list-style: none;
    }
    .alert {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 0.875rem 1rem;
      border: 1px solid var(--border);
      border-left: 4px solid var(--status-degraded);
      border-radius: 8px;
      background: var(--surface);
      min-width: 0;
    }
    .alert__head {
      display: flex;
      flex-wrap: wrap;
      align-items: baseline;
      gap: 0.5rem;
    }
    .alert__kind {
      font-weight: 600;
    }
    .alert__code {
      color: var(--muted);
      font-size: 0.8125rem;
    }
    .facts,
    .details {
      display: grid;
      gap: 0.5rem 1rem;
      grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
      margin: 0;
    }
    .details {
      grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    }
    .facts dt,
    .details dt {
      color: var(--muted);
      font-size: 0.75rem;
    }
    .facts dd,
    .details dd {
      margin: 0;
    }
    .wrap {
      overflow-wrap: anywhere;
    }
    .state {
      margin: 0;
      color: var(--muted);
    }
    .state--error {
      color: var(--danger);
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class SystemAlertsSection {
  private readonly service = inject(SystemAlertsService);

  protected readonly alerts = signal<SystemAlert[] | null>(null);
  protected readonly loading = signal(true);
  protected readonly loadError = signal(false);

  private loadInFlight = false;

  constructor() {
    void this.load();
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') {
        void this.load();
      }
    }, RELOAD_INTERVAL_MS);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
  }

  /** Reloads the alerts. Skips the call if one is already running. */
  protected async load(): Promise<void> {
    if (this.loadInFlight) {
      return;
    }
    this.loadInFlight = true;
    try {
      this.alerts.set(await firstValueFrom(this.service.active()));
      this.loadError.set(false);
    } catch {
      this.loadError.set(true);
    } finally {
      this.loadInFlight = false;
      this.loading.set(false);
    }
  }

  protected rowsOf(alert: SystemAlert): DetailRow[] {
    return detailRows(alert.details);
  }
}
