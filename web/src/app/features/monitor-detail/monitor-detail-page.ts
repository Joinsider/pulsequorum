// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  ChangeDetectionStrategy,
  ResourceRef,
  Component,
  DestroyRef,
  computed,
  effect,
  inject,
  resource,
  signal,
  untracked,
} from '@angular/core';
import { takeUntilDestroyed, toSignal } from '@angular/core/rxjs-interop';
import { MatButtonModule } from '@angular/material/button';
import { MatButtonToggleModule } from '@angular/material/button-toggle';
import { MatDialog } from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatSelectModule } from '@angular/material/select';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import type { EChartsCoreOption } from 'echarts/core';
import { NgxEchartsDirective } from 'ngx-echarts';
import { firstValueFrom, map } from 'rxjs';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { CheckResult, Heartbeat, Incident, StreamEvent } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { StatusBadge } from '../../shared/status/status-badge';
import { UptimeBar } from '../../shared/uptime-bar/uptime-bar';
import {
  DARK_PALETTE,
  LIGHT_PALETTE,
  buildLatencyOption,
  darkModeSignal,
} from './monitor-detail.chart';
import {
  HEARTBEAT_PAGE_LIMIT,
  INCIDENT_LIMIT,
  MAX_HEARTBEAT_PAGES,
  RANGES,
  Range,
  appendHeartbeat,
  buildNodeSeries,
  bucketSeries,
  collectPages,
  heartbeatWindowStart,
  incidentDuration,
  isNotFound,
  latestPerNode,
  monitorTarget,
  nodeIds,
  splitDuration,
  uptimeQuery,
} from './monitor-detail.model';

/**
 * Monitor detail (FA-UI-04): consensus status, status and latency per node,
 * uptime with range and node selection, recent incidents and live updates.
 */
@Component({
  selector: 'app-monitor-detail-page',
  imports: [
    RouterLink,
    MatButtonModule,
    MatButtonToggleModule,
    MatFormFieldModule,
    MatSelectModule,
    NgxEchartsDirective,
    TranslocoDirective,
    PageHeader,
    StatusBadge,
    UptimeBar,
    Icon,
    RelativeTimePipe,
  ],
  templateUrl: './monitor-detail-page.html',
  styleUrl: './monitor-detail-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class MonitorDetailPage {
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  private readonly dialog = inject(MatDialog);
  private readonly monitorsApi = inject(MonitorsService);
  private readonly incidentsApi = inject(IncidentsService);
  private readonly transloco = inject(TranslocoService);
  private readonly live = inject(LiveStreamService);
  protected readonly auth = inject(AuthService);

  protected readonly dark = darkModeSignal(inject(DestroyRef));
  /** Re-read on language change so computed labels follow it. */
  private readonly translation = toSignal(this.transloco.selectTranslation(), {
    initialValue: {},
  });

  /** The `:id` param is declared on the parent route; it is inherited here. */
  private readonly routeId = toSignal(
    this.route.paramMap.pipe(
      map((p) => p.get('id') ?? this.route.parent?.snapshot.paramMap.get('id') ?? ''),
    ),
    { initialValue: '' },
  );

  protected readonly ranges = RANGES;
  protected readonly range = signal<Range>('24h');
  /** Null is the consensus view; otherwise the id of one node. */
  protected readonly nodeFilter = signal<string | null>(null);

  protected readonly busy = signal(false);
  protected readonly actionFailed = signal(false);
  protected readonly testBusy = signal(false);
  protected readonly testFailed = signal(false);
  protected readonly testResult = signal<CheckResult | null>(null);

  protected readonly monitorRes = resource({
    params: () => this.routeId() || undefined,
    loader: ({ params: id }) => firstValueFrom(this.monitorsApi.getMonitor(id)),
  });

  /** Heartbeats of the last 24 hours, all pages (node table, live chart). */
  protected readonly heartbeatsRes = resource({
    params: () => this.routeId() || undefined,
    loader: ({ params: id }) =>
      collectPages(
        (cursor) =>
          firstValueFrom(
            this.monitorsApi.listHeartbeats(id, {
              from: heartbeatWindowStart(Date.now()),
              limit: HEARTBEAT_PAGE_LIMIT,
              cursor,
            }),
          ),
        MAX_HEARTBEAT_PAGES,
      ),
  });

  protected readonly incidentsRes = resource({
    params: () => this.routeId() || undefined,
    loader: ({ params: id }) =>
      firstValueFrom(this.incidentsApi.listIncidents({ monitorId: id, limit: INCIDENT_LIMIT })),
  });

  protected readonly uptimeRes = resource({
    params: () => {
      const id = this.routeId();
      return id ? { id, range: this.range(), node: this.nodeFilter() } : undefined;
    },
    loader: ({ params }) =>
      firstValueFrom(this.monitorsApi.getUptime(params.id, uptimeQuery(params.range, params.node))),
  });

  protected readonly monitor = computed(() => valueOf(this.monitorRes));
  protected readonly isPaused = computed(() => {
    const m = this.monitor();
    return !!m && (m.status === 'PAUSED' || m.paused === true);
  });
  protected readonly target = computed(() => monitorTarget(this.monitor()?.config));
  protected readonly heartbeats = computed(() => valueOf(this.heartbeatsRes) ?? []);
  protected readonly nodes = computed(() => nodeIds(this.heartbeats()));
  protected readonly latest = computed(() => latestPerNode(this.heartbeats()));
  protected readonly uptime = computed(() => valueOf(this.uptimeRes));
  protected readonly incidents = computed(() => valueOf(this.incidentsRes)?.items ?? []);

  private readonly nodeSeries = computed(() =>
    buildNodeSeries(this.heartbeats(), this.nodeFilter()),
  );
  private readonly bucketPoints = computed(() => bucketSeries(this.uptime()?.buckets ?? []));

  protected readonly hasChartData = computed(() =>
    this.range() === '24h' ? this.nodeSeries().length > 0 : this.bucketPoints().length > 0,
  );

  protected readonly chart = computed<EChartsCoreOption>(() => {
    const palette = this.dark() ? DARK_PALETTE : LIGHT_PALETTE;
    const labels = {
      yAxis: this.tr('monitorDetail.chart.yAxis'),
      failure: this.tr('monitorDetail.chart.failure'),
      average: this.tr('monitorDetail.chart.average'),
    };
    return this.range() === '24h'
      ? buildLatencyOption({ mode: 'live', nodes: this.nodeSeries() }, palette, labels)
      : buildLatencyOption({ mode: 'buckets', points: this.bucketPoints() }, palette, labels);
  });

  protected readonly isNotFound = isNotFound;

  constructor() {
    this.live.events.pipe(takeUntilDestroyed()).subscribe((ev) => this.onLiveEvent(ev));

    // Reload everything after a reconnect; the first emission is not a reconnect.
    let lastResync = untracked(() => this.live.resync());
    effect(() => {
      const n = this.live.resync();
      if (n !== lastResync) {
        lastResync = n;
        untracked(() => this.reloadAll());
      }
    });

    this.route.paramMap.pipe(takeUntilDestroyed()).subscribe(() => this.nodeFilter.set(null));
  }

  protected setRange(value: Range): void {
    this.range.set(value);
  }

  protected percent(value: number | null | undefined): string {
    if (value == null) {
      return '–';
    }
    return `${new Intl.NumberFormat(this.locale(), { maximumFractionDigits: 3 }).format(value)} %`;
  }

  protected ms(value: number | null | undefined): string {
    if (value == null) {
      return '–';
    }
    return `${new Intl.NumberFormat(this.locale(), { maximumFractionDigits: 0 }).format(value)} ms`;
  }

  protected duration(ms: number): string {
    const parts = splitDuration(ms);
    return this.tr(`monitorDetail.duration.${parts.unit}`, { count: parts.count });
  }

  protected incidentMs(incident: Incident): number {
    return incidentDuration(incident, Date.now());
  }

  protected formatTime(iso: string): string {
    const time = Date.parse(iso);
    if (Number.isNaN(time)) {
      return '';
    }
    return new Intl.DateTimeFormat(this.locale(), {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(time);
  }

  protected locale(): string {
    this.translation();
    return this.transloco.getActiveLang();
  }

  protected async togglePause(): Promise<void> {
    const m = this.monitor();
    if (!m) {
      return;
    }
    const wasPaused = this.isPaused();
    await this.run(
      () =>
        firstValueFrom(
          wasPaused ? this.monitorsApi.resumeMonitor(m.id) : this.monitorsApi.pauseMonitor(m.id),
        ),
      (updated) => this.monitorRes.set(updated),
    );
  }

  protected async clone(): Promise<void> {
    const m = this.monitor();
    if (!m) {
      return;
    }
    await this.run(
      () => firstValueFrom(this.monitorsApi.cloneMonitor(m.id)),
      (copy) => void this.router.navigate(['/monitors', copy.id]),
    );
  }

  protected async remove(): Promise<void> {
    const m = this.monitor();
    if (!m) {
      return;
    }
    const confirmed = await confirm(this.dialog, {
      titleKey: 'monitorDetail.delete.title',
      messageKey: 'monitorDetail.delete.message',
      confirmKey: 'common.delete',
      danger: true,
    });
    if (!confirmed) {
      return;
    }
    await this.run(
      () => firstValueFrom(this.monitorsApi.deleteMonitor(m.id)),
      () => void this.router.navigateByUrl('/'),
    );
  }

  protected async runTest(): Promise<void> {
    const m = this.monitor();
    if (!m || this.testBusy()) {
      return;
    }
    this.testBusy.set(true);
    this.testFailed.set(false);
    this.testResult.set(null);
    try {
      this.testResult.set(await firstValueFrom(this.monitorsApi.testMonitor(m.id)));
    } catch {
      this.testFailed.set(true);
    } finally {
      this.testBusy.set(false);
    }
  }

  private async run<T>(action: () => Promise<T>, onDone: (value: T) => void): Promise<void> {
    this.busy.set(true);
    this.actionFailed.set(false);
    try {
      onDone(await action());
    } catch {
      this.actionFailed.set(true);
    } finally {
      this.busy.set(false);
    }
  }

  private onLiveEvent(ev: StreamEvent): void {
    if (!ev.monitorId || ev.monitorId !== this.routeId()) {
      return;
    }
    switch (ev.type) {
      case 'heartbeat':
        this.appendLive(ev.heartbeat);
        break;
      case 'state':
        this.monitorRes.reload();
        break;
      case 'incident':
        this.incidentsRes.reload();
        break;
      default:
        break;
    }
  }

  private appendLive(heartbeat: Heartbeat | undefined): void {
    const current = valueOf(this.heartbeatsRes);
    if (heartbeat && current) {
      this.heartbeatsRes.set(appendHeartbeat(current, heartbeat, Date.now()));
    }
  }

  private reloadAll(): void {
    this.monitorRes.reload();
    this.heartbeatsRes.reload();
    this.incidentsRes.reload();
    this.uptimeRes.reload();
  }

  /** Translates with a dependency on the translations, so the result updates with them. */
  protected tr(key: string, params?: Record<string, unknown>): string {
    this.translation();
    return this.transloco.translate(key, params);
  }
}

/** Value of a resource, or undefined while loading or after an error (value() throws then). */
function valueOf<T>(res: ResourceRef<T>): T | undefined {
  return res.hasValue() ? res.value() : undefined;
}
