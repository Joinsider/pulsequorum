// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  OnInit,
  signal,
} from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatChipsModule } from '@angular/material/chips';
import { MatDialog } from '@angular/material/dialog';
import { MatProgressSpinnerModule } from '@angular/material/progress-spinner';
import { RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { AuthService } from '../../core/auth/auth.service';
import { DeliveryResult, NotificationChannel } from '../../api/gen/model';
import { ChannelApi, ChannelScope, channelListPath } from './channel-api';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { failureText } from './error-text';

/** Outcome of a test send, shown next to the channel. */
export type TestOutcome =
  | { readonly delivered: true; readonly durationMs?: number }
  | { readonly delivered: false; readonly error?: string };

/** Lists the notification channels of the tenant (FA-UI-04). */
@Component({
  selector: 'app-channels-page',
  imports: [
    RouterLink,
    MatButtonModule,
    MatChipsModule,
    MatProgressSpinnerModule,
    Icon,
    PageHeader,
    TranslocoDirective,
  ],
  template: `
    <ng-container *transloco="let t">
      <app-page-header
        [title]="scope() === 'system' ? t('channels.system.title') : t('channels.title')"
      >
        @if (auth.canAdmin()) {
          <a mat-flat-button [routerLink]="[base(), 'new']">
            <app-icon name="add" size="1.125rem" />
            {{ scope() === 'system' ? t('channels.system.new') : t('channels.new') }}
          </a>
        }
      </app-page-header>

      @if (notice(); as n) {
        <p class="notice" [class.notice--error]="n.error" role="status">{{ n.text }}</p>
      }

      @if (loading()) {
        <div class="state" role="status">
          <mat-spinner diameter="28" />
          <span>{{ t('common.loading') }}</span>
        </div>
      } @else if (loadError()) {
        <div class="state" role="alert">
          <p>{{ t('common.error') }}</p>
          <button mat-stroked-button type="button" (click)="load()">{{ t('common.retry') }}</button>
        </div>
      } @else if (channels().length === 0) {
        <div class="state">
          <p>{{ scope() === 'system' ? t('channels.system.empty') : t('channels.empty') }}</p>
          <p class="muted">
            {{ scope() === 'system' ? t('channels.system.emptyHint') : t('channels.emptyHint') }}
          </p>
        </div>
      } @else {
        <ul class="list">
          @for (c of channels(); track c.id) {
            <li class="card">
              <div class="head">
                <h2 class="name">{{ c.name }}</h2>
                <mat-chip-set [attr.aria-label]="t('channels.chipsLabel')">
                  <mat-chip>{{ t('channels.kinds.' + c.kind) }}</mat-chip>
                  @if (c.isDefault) {
                    <mat-chip>{{ t('channels.default') }}</mat-chip>
                  }
                  <mat-chip
                    [class.chip--ok]="c.state === 'ok'"
                    [class.chip--failing]="c.state === 'failing'"
                    [class.chip--disabled]="c.state === 'disabled'"
                  >
                    {{ t('channels.states.' + c.state) }}
                  </mat-chip>
                </mat-chip-set>
              </div>

              <p class="target">
                <code>{{ target(c) }}</code>
              </p>

              <mat-chip-set [attr.aria-label]="t('channels.eventsLabel')">
                @for (e of c.events; track e) {
                  <mat-chip>{{ t('channels.events.' + e) }}</mat-chip>
                }
              </mat-chip-set>

              @if (c.lastError) {
                <p class="error">{{ t('channels.lastError') }}: {{ c.lastError }}</p>
              }

              @if (testResults()[c.id]; as r) {
                <p class="test" role="status" [class.test--fail]="!r.delivered">
                  @if (r.delivered) {
                    {{ t('channels.testOk', { ms: r.durationMs ?? 0 }) }}
                  } @else if (r.error) {
                    {{ t('channels.testFailed', { error: r.error }) }}
                  } @else {
                    {{ t('channels.testRequestFailed') }}
                  }
                </p>
              }

              @if (auth.canAdmin()) {
                <div class="actions">
                  <a mat-stroked-button [routerLink]="[base(), c.id]">
                    <app-icon name="edit" size="1.125rem" />
                    {{ t('common.edit') }}
                  </a>
                  <button
                    mat-stroked-button
                    type="button"
                    [disabled]="isBusy(c.id)"
                    (click)="test(c)"
                  >
                    <app-icon name="playArrow" size="1.125rem" />
                    {{ isBusy(c.id) ? t('channels.testing') : t('channels.test') }}
                  </button>
                  <button
                    mat-button
                    type="button"
                    color="warn"
                    [disabled]="isBusy(c.id)"
                    (click)="remove(c)"
                  >
                    <app-icon name="delete" size="1.125rem" />
                    {{ t('common.delete') }}
                  </button>
                </div>
              }
            </li>
          }
        </ul>
      }
    </ng-container>
  `,
  styles: `
    .notice {
      margin: 0 0 1rem;
      padding: 0.625rem 0.875rem;
      border-radius: 6px;
      background: var(--success-bg);
      color: var(--success);
    }
    .notice--error {
      background: color-mix(in srgb, var(--danger) 12%, transparent);
      color: var(--danger);
    }
    .state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.75rem;
      padding: 2rem 1rem;
      color: var(--text);
      text-align: center;
    }
    .state p {
      margin: 0;
    }
    .muted {
      color: var(--muted);
    }
    .list {
      display: grid;
      gap: 0.875rem;
      margin: 0;
      padding: 0;
      list-style: none;
    }
    .card {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
      padding: 1rem;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--surface);
      min-width: 0;
    }
    .head {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }
    .name {
      margin: 0;
      font-size: 1.0625rem;
      font-weight: 650;
      overflow-wrap: anywhere;
    }
    .target {
      margin: 0;
      min-width: 0;
    }
    .target code {
      display: block;
      padding: 0.375rem 0.5rem;
      border-radius: 4px;
      background: var(--background);
      font-size: 0.8125rem;
      overflow-wrap: anywhere;
    }
    .chip--ok {
      color: var(--status-up);
    }
    .chip--failing {
      color: var(--status-degraded);
    }
    .chip--disabled {
      color: var(--status-down);
    }
    .error,
    .test--fail {
      margin: 0;
      color: var(--danger);
      overflow-wrap: anywhere;
    }
    .test {
      margin: 0;
      color: var(--success);
      overflow-wrap: anywhere;
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
    }
    .actions a,
    .actions button app-icon {
      margin-right: 0.25rem;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ChannelsPage implements OnInit {
  /** Tenant channels, or the platform's system channels for platform admins (ADR 0025). */
  readonly scope = input<ChannelScope>('tenant');
  protected readonly base = computed(() => channelListPath(this.scope()));

  protected readonly auth = inject(AuthService);
  private readonly api = inject(ChannelApi);
  private readonly dialog = inject(MatDialog);
  private readonly transloco = inject(TranslocoService);

  protected readonly channels = signal<NotificationChannel[]>([]);
  protected readonly loading = signal(true);
  protected readonly loadError = signal(false);
  protected readonly busyIds = signal<ReadonlySet<string>>(new Set());
  protected readonly testResults = signal<Readonly<Record<string, TestOutcome>>>({});
  protected readonly notice = signal<{ text: string; error: boolean } | null>(null);

  ngOnInit(): void {
    this.load();
  }

  protected load(): void {
    this.loading.set(true);
    this.loadError.set(false);
    this.api.list(this.scope()).subscribe({
      next: (list) => {
        this.channels.set(list);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.loadError.set(true);
      },
    });
  }

  /** Secrets are never shown: the server returns only the masked URL or the webhook URL. */
  protected target(c: NotificationChannel): string {
    return (c.kind === 'webhook' ? c.webhook?.url : c.maskedUrl) ?? '-';
  }

  protected isBusy(id: string): boolean {
    return this.busyIds().has(id);
  }

  protected async test(c: NotificationChannel): Promise<void> {
    this.setBusy(c.id, true);
    try {
      const result: DeliveryResult = await new Promise((resolve, reject) =>
        this.api.test(this.scope(), c.id).subscribe({ next: resolve, error: reject }),
      );
      const outcome: TestOutcome = result.delivered
        ? { delivered: true, durationMs: result.durationMs }
        : { delivered: false, error: result.error };
      this.testResults.update((r) => ({ ...r, [c.id]: outcome }));
    } catch {
      this.testResults.update((r) => ({ ...r, [c.id]: { delivered: false } }));
    } finally {
      this.setBusy(c.id, false);
    }
  }

  protected async remove(c: NotificationChannel): Promise<void> {
    const confirmed = await confirm(this.dialog, {
      titleKey: 'channels.deleteTitle',
      messageKey: 'channels.deleteText',
      confirmKey: 'common.delete',
      danger: true,
    });
    if (!confirmed) {
      return;
    }
    this.setBusy(c.id, true);
    try {
      await new Promise<void>((resolve, reject) =>
        this.api.remove(this.scope(), c.id).subscribe({ next: () => resolve(), error: reject }),
      );
      this.channels.update((list) => list.filter((x) => x.id !== c.id));
      this.notice.set({ text: this.transloco.translate('channels.deleted'), error: false });
    } catch (err) {
      this.notice.set({
        text: failureText(err, this.transloco.translate('common.error')),
        error: true,
      });
    } finally {
      this.setBusy(c.id, false);
    }
  }

  private setBusy(id: string, busy: boolean): void {
    this.busyIds.update((set) => {
      const next = new Set(set);
      if (busy) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  }
}
