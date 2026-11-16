// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  signal,
  untracked,
} from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { MatButtonModule } from '@angular/material/button';
import { RouterLink } from '@angular/router';
import { TranslocoDirective } from '@jsverse/transloco';
import { finalize, firstValueFrom } from 'rxjs';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { FlappingBadge } from '../../shared/status/flapping-badge';
import { StatusBadge } from '../../shared/status/status-badge';
import { statusColorVar } from '../../shared/status/status-colors';
import {
  STATUS_ORDER,
  applyStreamEvent,
  countByStatus,
  filterMonitors,
  formatLatency,
  formatPercent,
  isUnknownMonitorEvent,
  sortMonitors,
  targetOf,
} from './dashboard.logic';

type LoadState = 'loading' | 'ok' | 'error';

/** Debounce for reloads triggered by events of unknown monitors. */
const RELOAD_DEBOUNCE_MS = 500;
/** Largest page the API accepts; fewer requests for big tenants. */
const PAGE_LIMIT = 500;

/** One table row: the monitor plus its derived target. */
export interface DashboardRow {
  readonly monitor: Monitor;
  readonly target: string | null;
}

@Component({
  selector: 'app-dashboard-page',
  imports: [
    TranslocoDirective,
    MatButtonModule,
    RouterLink,
    Icon,
    PageHeader,
    RelativeTimePipe,
    FlappingBadge,
    StatusBadge,
  ],
  templateUrl: './dashboard-page.html',
  styleUrl: './dashboard-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class DashboardPage {
  private readonly monitors = inject(MonitorsService);
  private readonly auth = inject(AuthService);
  private readonly live = inject(LiveStreamService);
  private readonly destroyRef = inject(DestroyRef);

  protected readonly canEdit = this.auth.canEdit;
  protected readonly statuses = STATUS_ORDER;
  protected readonly statusColor = statusColorVar;
  protected readonly formatPercent = formatPercent;
  protected readonly formatLatency = formatLatency;

  protected readonly loadState = signal<LoadState>('loading');
  protected readonly actionFailed = signal(false);
  /** Monitors with a pause/resume request in flight. */
  protected readonly pending = signal<ReadonlySet<string>>(new Set());

  private readonly all = signal<readonly Monitor[]>([]);
  protected readonly total = computed(() => this.all().length);
  protected readonly counts = computed(() => countByStatus(this.all()));
  protected readonly statusFilter = signal<MonitorStatus | null>(null);
  protected readonly query = signal('');

  protected readonly rows = computed<DashboardRow[]>(() =>
    sortMonitors(filterMonitors(this.all(), this.statusFilter(), this.query())).map((monitor) => ({
      monitor,
      target: targetOf(monitor.config),
    })),
  );

  private loadGeneration = 0;
  private reloadTimer: ReturnType<typeof setTimeout> | undefined;

  constructor() {
    // Runs once on start and again after every reconnect (missed events).
    effect(() => {
      this.live.resync();
      untracked(() => void this.load());
    });

    this.live.events.pipe(takeUntilDestroyed(this.destroyRef)).subscribe((event) => {
      this.onEvent(event);
    });

    this.destroyRef.onDestroy(() => clearTimeout(this.reloadTimer));
  }

  /** Loads every page of the monitor list; stale responses are dropped. */
  protected async load(): Promise<void> {
    const generation = ++this.loadGeneration;
    if (this.all().length === 0) {
      this.loadState.set('loading');
    }
    try {
      const items = await this.fetchAll();
      if (generation === this.loadGeneration) {
        this.all.set(items);
        this.loadState.set('ok');
      }
    } catch {
      if (generation === this.loadGeneration && this.all().length === 0) {
        this.loadState.set('error');
      }
    }
  }

  protected setStatusFilter(status: MonitorStatus | null): void {
    this.statusFilter.set(this.statusFilter() === status ? null : status);
  }

  protected onSearch(event: Event): void {
    this.query.set((event.target as HTMLInputElement).value);
  }

  protected isPaused(monitor: Monitor): boolean {
    return monitor.paused === true || monitor.status === MonitorStatus.PAUSED;
  }

  /** Pauses a running monitor or resumes a paused one. */
  protected togglePause(monitor: Monitor): void {
    if (this.pending().has(monitor.id)) {
      return;
    }
    this.setPending(monitor.id, true);
    const request = this.isPaused(monitor)
      ? this.monitors.resumeMonitor(monitor.id)
      : this.monitors.pauseMonitor(monitor.id);
    request
      .pipe(
        finalize(() => this.setPending(monitor.id, false)),
        takeUntilDestroyed(this.destroyRef),
      )
      .subscribe({
        next: (updated) => {
          this.actionFailed.set(false);
          this.all.update((list) => list.map((m) => (m.id === updated.id ? updated : m)));
        },
        error: () => this.actionFailed.set(true),
      });
  }

  private onEvent(event: StreamEvent): void {
    const current = this.all();
    if (isUnknownMonitorEvent(current, event)) {
      this.reloadSoon();
      return;
    }
    const next = applyStreamEvent(current, event);
    if (next !== current) {
      this.all.set(next);
    }
  }

  private reloadSoon(): void {
    clearTimeout(this.reloadTimer);
    this.reloadTimer = setTimeout(() => void this.load(), RELOAD_DEBOUNCE_MS);
  }

  private async fetchAll(): Promise<Monitor[]> {
    const items: Monitor[] = [];
    const seenCursors = new Set<string>();
    let cursor: string | undefined;
    do {
      const page = await firstValueFrom(this.monitors.listMonitors({ cursor, limit: PAGE_LIMIT }));
      items.push(...page.items);
      cursor = page.nextCursor ?? undefined;
      if (cursor !== undefined) {
        if (seenCursors.has(cursor)) {
          break; // defensive: a repeating cursor would loop forever
        }
        seenCursors.add(cursor);
      }
    } while (cursor !== undefined);
    return items;
  }

  private setPending(id: string, on: boolean): void {
    this.pending.update((set) => {
      const next = new Set(set);
      if (on) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  }
}
