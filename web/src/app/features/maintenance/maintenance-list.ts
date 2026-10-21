// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { MaintenanceService } from '../../api/gen/endpoints/maintenance/maintenance.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { MaintenanceWindow } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { loadAllMonitors, MONITOR_PAGE_SIZE } from './monitor-options';
import {
  browserTimeZone,
  describeCron,
  formatInstant,
  formatInterval,
  weekdayName,
} from './schedule';

/** Maintenance windows of the tenant with their schedule, affected monitors and next start. */
@Component({
  selector: 'app-maintenance-list',
  imports: [RouterLink, MatButtonModule, PageHeader, Icon, RelativeTimePipe, TranslocoDirective],
  template: `
    <ng-container *transloco="let t">
      <app-page-header [title]="t('maintenance.title')">
        @if (auth.canEdit()) {
          <a mat-flat-button routerLink="/maintenance/new">
            <app-icon name="add" />
            {{ t('maintenance.new') }}
          </a>
        }
      </app-page-header>

      @if (actionFailed()) {
        <p class="notice" role="alert">{{ t('maintenance.errors.action') }}</p>
      }

      @if (loadFailed()) {
        <div class="state" role="alert">
          <p>{{ t('common.error') }}</p>
          <button mat-stroked-button type="button" (click)="load()">{{ t('common.retry') }}</button>
        </div>
      } @else if (windows() === null) {
        <p class="state" role="status">{{ t('common.loading') }}</p>
      } @else if (windows()!.length === 0) {
        <div class="state">
          <p>{{ t('common.empty') }}</p>
          @if (auth.canEdit()) {
            <a mat-flat-button routerLink="/maintenance/new">{{ t('maintenance.new') }}</a>
          }
        </div>
      } @else {
        <ul class="list">
          @for (w of windows(); track w.id) {
            <li class="card">
              <header class="card__head">
                <h2 class="card__title">{{ w.name }}</h2>
                <div class="chips">
                  @if (w.inProgress) {
                    <span class="chip chip--live">{{ t('maintenance.list.inProgress') }}</span>
                  }
                  @if (w.active === false) {
                    <span class="chip chip--muted">{{ t('maintenance.list.disabled') }}</span>
                  }
                  <span class="chip">
                    {{
                      w.schedule.kind === 'recurring'
                        ? t('maintenance.list.recurring')
                        : t('maintenance.list.once')
                    }}
                  </span>
                </div>
                @if (auth.canEdit()) {
                  <div class="card__actions">
                    <a
                      mat-icon-button
                      [routerLink]="['/maintenance', w.id]"
                      [attr.aria-label]="t('maintenance.list.editNamed', { name: w.name })"
                    >
                      <app-icon name="edit" />
                    </a>
                    <button
                      mat-icon-button
                      type="button"
                      (click)="remove(w)"
                      [attr.aria-label]="t('maintenance.list.deleteNamed', { name: w.name })"
                    >
                      <app-icon name="delete" />
                    </button>
                  </div>
                }
              </header>

              @if (w.description) {
                <p class="muted">{{ w.description }}</p>
              }

              <dl class="facts">
                <dt>{{ t('maintenance.list.schedule') }}</dt>
                <dd>
                  @if (w.schedule.kind === 'once') {
                    <span>{{ zoneRange(w, lang()) }}</span>
                    <span class="muted">{{ zoneName(w) }}</span>
                    @if (localRange(w, lang()); as local) {
                      <span class="muted">{{
                        t('maintenance.list.yourTime', { range: local })
                      }}</span>
                    }
                  } @else {
                    <span>{{ cronText(w, lang()) }}</span>
                    <code>{{ w.schedule.cron }}</code>
                    <span class="muted">
                      {{
                        t('maintenance.list.recurringMeta', {
                          minutes: w.schedule.durationMinutes ?? 0,
                          zone: zoneName(w),
                        })
                      }}
                    </span>
                  }
                </dd>

                <dt>{{ t('maintenance.list.monitors') }}</dt>
                <dd>{{ monitorsOf(w, t) }}</dd>

                <dt>{{ t('maintenance.list.nextStart') }}</dt>
                <dd>
                  @if (w.nextStart) {
                    <span>{{ startText(w, lang()) }}</span>
                    <span class="muted">{{ w.nextStart | relativeTime: lang() }}</span>
                  } @else {
                    <span class="muted">{{ t('maintenance.list.noNextStart') }}</span>
                  }
                </dd>
              </dl>
            </li>
          }
        </ul>
      }
    </ng-container>
  `,
  styles: `
    .state,
    .notice {
      margin: 1rem 0;
    }
    .state {
      display: flex;
      flex-direction: column;
      align-items: flex-start;
      gap: 0.75rem;
      color: var(--muted);
    }
    .notice {
      padding: 0.75rem 1rem;
      color: var(--danger);
      border: 1px solid var(--danger);
      border-radius: 8px;
    }
    .list {
      display: grid;
      gap: 0.75rem;
      margin: 0;
      padding: 0;
      list-style: none;
    }
    .card {
      padding: 1rem;
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: 12px;
    }
    .card__head {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 0.75rem;
    }
    .card__title {
      margin: 0;
      min-width: 0;
      flex: 1 1 12rem;
      font-size: 1.1rem;
      overflow-wrap: anywhere;
    }
    .card__actions {
      display: flex;
      margin-left: auto;
    }
    .chips {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }
    .chip {
      padding: 0.125rem 0.5rem;
      font-size: 0.8rem;
      border: 1px solid var(--border);
      border-radius: 999px;
    }
    .chip--live {
      color: var(--status-maintenance, var(--muted));
      border-color: currentColor;
      font-weight: 600;
    }
    .chip--muted {
      color: var(--muted);
    }
    .muted {
      color: var(--muted);
    }
    p.muted {
      margin: 0.5rem 0 0;
    }
    .facts {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 0.375rem 1rem;
      margin: 0.75rem 0 0;
      font-size: 0.9rem;
    }
    .facts dt {
      color: var(--muted);
    }
    .facts dd {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem 0.5rem;
      margin: 0;
      min-width: 0;
      overflow-wrap: anywhere;
    }
    code {
      font-size: 0.85em;
    }
    @media (max-width: 480px) {
      .facts {
        grid-template-columns: 1fr;
      }
      .facts dt {
        margin-top: 0.25rem;
      }
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class MaintenanceList {
  private readonly api = inject(MaintenanceService);
  private readonly monitorsApi = inject(MonitorsService);
  private readonly dialog = inject(MatDialog);
  private readonly transloco = inject(TranslocoService);
  protected readonly auth = inject(AuthService);

  protected readonly lang = toSignal(this.transloco.langChanges$, {
    initialValue: this.transloco.getActiveLang(),
  });
  private readonly browserZone = browserTimeZone();

  protected readonly windows = signal<MaintenanceWindow[] | null>(null);
  protected readonly monitorNames = signal<ReadonlyMap<string, string>>(new Map());
  protected readonly loadFailed = signal(false);
  protected readonly actionFailed = signal(false);

  constructor() {
    void this.load();
  }

  /** Loads windows and monitor names; retries after an error. */
  protected async load(): Promise<void> {
    this.loadFailed.set(false);
    try {
      const [windows, monitors] = await Promise.all([
        firstValueFrom(this.api.listMaintenanceWindows()),
        loadAllMonitors((cursor) =>
          firstValueFrom(this.monitorsApi.listMonitors({ cursor, limit: MONITOR_PAGE_SIZE })),
        ),
      ]);
      this.monitorNames.set(new Map(monitors.map((m) => [m.id, m.name])));
      this.windows.set([...windows].sort((a, b) => a.name.localeCompare(b.name)));
    } catch {
      this.loadFailed.set(true);
    }
  }

  /** Deletes a window after confirmation (permitted roles only; the server enforces it). */
  protected async remove(w: MaintenanceWindow): Promise<void> {
    const ok = await confirm(this.dialog, {
      titleKey: 'maintenance.delete.title',
      messageKey: 'maintenance.delete.message',
      confirmKey: 'common.delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    this.actionFailed.set(false);
    try {
      await firstValueFrom(this.api.deleteMaintenanceWindow(w.id));
      this.windows.update((ws) => ws?.filter((x) => x.id !== w.id) ?? null);
    } catch {
      this.actionFailed.set(true);
    }
  }

  protected zoneName(w: MaintenanceWindow): string {
    return w.schedule.timezone || 'UTC';
  }

  /** One-time window: start to end in the window's own zone. */
  protected zoneRange(w: MaintenanceWindow, lang: string): string {
    const s = w.schedule;
    if (!s.startsAt || !s.endsAt) {
      return '';
    }
    return formatInterval(Date.parse(s.startsAt), Date.parse(s.endsAt), lang, this.zoneName(w));
  }

  /** One-time window in the browser's zone, shown only if it differs from the window's zone. */
  protected localRange(w: MaintenanceWindow, lang: string): string | null {
    if (this.zoneName(w) === this.browserZone || w.schedule.kind !== 'once') {
      return null;
    }
    const s = w.schedule;
    if (!s.startsAt || !s.endsAt) {
      return null;
    }
    return formatInterval(Date.parse(s.startsAt), Date.parse(s.endsAt), lang);
  }

  /** Human-readable recurring pattern, or the raw cron expression for other patterns. */
  protected cronText(w: MaintenanceWindow, lang: string): string {
    const cron = w.schedule.cron ?? '';
    return (
      describeCron(
        cron,
        (key, params) => this.transloco.translate(key, params),
        (day) => weekdayName(lang, day),
      ) ?? cron
    );
  }

  protected startText(w: MaintenanceWindow, lang: string): string {
    return w.nextStart ? formatInstant(Date.parse(w.nextStart), lang, this.zoneName(w)) : '';
  }

  /** Names of the affected monitors; a few are listed, the rest counted. */
  protected monitorsOf(
    w: MaintenanceWindow,
    t: (key: string, params?: Record<string, unknown>) => string,
  ): string {
    if (w.monitorIds.length === 0) {
      return t('maintenance.list.allMonitors');
    }
    const names = w.monitorIds.map(
      (id) => this.monitorNames().get(id) ?? t('maintenance.list.unknownMonitor'),
    );
    const shown = names.slice(0, 3).join(', ');
    return names.length > 3
      ? `${shown} ${t('maintenance.list.moreMonitors', { count: names.length - 3 })}`
      : shown;
  }
}
