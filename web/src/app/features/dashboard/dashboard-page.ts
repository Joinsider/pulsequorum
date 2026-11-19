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
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { finalize, firstValueFrom } from 'rxjs';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { BulkMonitorsInput, Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { OwnTenantPanel } from '../tenant/own-tenant-panel';
import { FlappingBadge } from '../../shared/status/flapping-badge';
import { StatusBadge } from '../../shared/status/status-badge';
import { statusColorVar } from '../../shared/status/status-colors';
import { newRequestKey } from '../monitor-editor/monitor-form';
import { BulkDeleteData, BulkDeleteDialog } from './bulk-delete-dialog';
import {
  BULK_MAX_IDS,
  STATUS_ORDER,
  applyStreamEvent,
  bulkErrorKey,
  countByStatus,
  filterMonitors,
  formatLatency,
  formatPercent,
  isUnknownMonitorEvent,
  parseTags,
  pruneSelection,
  selectionState,
  sortMonitors,
  targetOf,
  toggleAll,
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
    OwnTenantPanel,
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
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  protected readonly canEdit = this.auth.canEdit;
  protected readonly statuses = STATUS_ORDER;
  protected readonly statusColor = statusColorVar;
  protected readonly formatPercent = formatPercent;
  protected readonly formatLatency = formatLatency;
  protected readonly maxIds = BULK_MAX_IDS;

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

  /** IDs of the selected monitors; may include monitors hidden by the filter. */
  protected readonly selected = signal<ReadonlySet<string>>(new Set());
  protected readonly selectedCount = computed(() => this.selected().size);
  protected readonly visibleSelection = computed(() =>
    selectionState(
      this.selected(),
      this.rows().map((row) => row.monitor.id),
    ),
  );
  protected readonly tooMany = computed(() => this.selectedCount() > BULK_MAX_IDS);
  /** True while a bulk call is in flight; the actions stay off until it returns. */
  protected readonly bulkBusy = signal(false);
  /** Bulk actions are off while a call runs, with nothing selected, or above the atomic limit. */
  protected readonly bulkDisabled = computed(
    () => this.bulkBusy() || this.selectedCount() === 0 || this.tooMany(),
  );
  protected readonly tagInput = signal('');
  /** Translation key of the problem with the tag field, or null. */
  protected readonly tagError = signal<string | null>(null);

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
        this.selected.update((set) =>
          pruneSelection(
            set,
            items.map((m) => m.id),
          ),
        );
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

  protected isSelected(id: string): boolean {
    return this.selected().has(id);
  }

  protected toggleOne(id: string): void {
    this.selected.update((set) => {
      const next = new Set(set);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }

  /** Selects every visible monitor, or clears them when all are selected already. */
  protected toggleAllVisible(): void {
    this.selected.set(
      toggleAll(
        this.selected(),
        this.rows().map((row) => row.monitor.id),
      ),
    );
  }

  protected clearSelection(): void {
    this.selected.set(new Set());
    this.tagError.set(null);
  }

  protected onTagInput(event: Event): void {
    this.tagInput.set((event.target as HTMLInputElement).value);
    this.tagError.set(null);
  }

  protected pauseSelected(): void {
    void this.runBulk({ action: 'pause', ids: this.selectedIds() }, 'dashboard.bulk.done.pause');
  }

  protected resumeSelected(): void {
    void this.runBulk({ action: 'resume', ids: this.selectedIds() }, 'dashboard.bulk.done.resume');
  }

  protected addTags(): void {
    this.applyTags('addTags', 'dashboard.bulk.done.addTags');
  }

  protected removeTags(): void {
    this.applyTags('removeTags', 'dashboard.bulk.done.removeTags');
  }

  /** Asks for confirmation first; the selection is only sent after a yes. */
  protected async deleteSelected(): Promise<void> {
    if (this.bulkDisabled()) {
      return;
    }
    const data: BulkDeleteData = { count: this.selectedCount() };
    const confirmed = await firstValueFrom(
      this.dialog
        .open<BulkDeleteDialog, BulkDeleteData, boolean>(BulkDeleteDialog, {
          data,
          role: 'alertdialog',
          autoFocus: 'dialog',
          restoreFocus: true,
        })
        .afterClosed(),
      { defaultValue: false },
    );
    if (confirmed === true) {
      await this.runBulk(
        { action: 'delete', ids: this.selectedIds() },
        'dashboard.bulk.done.delete',
      );
    }
  }

  private applyTags(action: 'addTags' | 'removeTags', doneKey: string): void {
    if (this.bulkDisabled()) {
      return;
    }
    const parsed = parseTags(this.tagInput());
    if ('error' in parsed) {
      this.tagError.set(parsed.error);
      return;
    }
    this.tagError.set(null);
    void this.runBulk({ action, ids: this.selectedIds(), tags: parsed.tags }, doneKey);
  }

  private selectedIds(): string[] {
    return [...this.selected()];
  }

  /**
   * Sends one atomic bulk call. On success the selection is cleared and the list
   * reloaded; on failure the selection stays so the user can retry.
   */
  private async runBulk(input: BulkMonitorsInput, doneKey: string): Promise<void> {
    if (this.bulkBusy() || input.ids.length === 0 || input.ids.length > BULK_MAX_IDS) {
      return;
    }
    this.bulkBusy.set(true);
    try {
      await firstValueFrom(
        this.monitors.bulkMonitors(input, {
          headers: { 'Idempotency-Key': newRequestKey() },
        }),
      );
      this.selected.set(new Set());
      this.tagError.set(null);
      this.snack.open(this.transloco.translate(doneKey, { count: input.ids.length }), undefined, {
        duration: 4000,
      });
      await this.load();
    } catch (err) {
      this.snack.open(this.transloco.translate(bulkErrorKey(err)), undefined, { duration: 6000 });
    } finally {
      this.bulkBusy.set(false);
    }
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
