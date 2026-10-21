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
import { takeUntilDestroyed, toSignal } from '@angular/core/rxjs-interop';
import { MatButtonModule } from '@angular/material/button';
import { MatButtonToggleModule } from '@angular/material/button-toggle';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatSelectModule } from '@angular/material/select';
import { ActivatedRoute, Router, RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { filter, map } from 'rxjs';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Incident, Monitor, StreamEvent } from '../../api/gen/model';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { IncidentDuration } from './incident-duration';
import {
  INCIDENT_RANGES,
  INCIDENT_STATES,
  IncidentFilters,
  IncidentRange,
  IncidentState,
  formatAbsolute,
  incidentDurationMs,
  matchesFilters,
  mergeIncidents,
  parseFilters,
  toApiParams,
  toQueryParams,
  upsertIncident,
} from './incident.logic';

const PAGE_SIZE = 50;
/** Pages without a visible match (client-side "resolved" filter) fetched in a row before giving up. */
const MAX_EMPTY_PAGES = 5;
const TICK_MS = 30_000;
const MONITOR_OPTIONS_LIMIT = 500;

type LoadState = 'loading' | 'ok' | 'error';

/** Incident list with URL-based filters, cursor pagination and live updates (FA-UI-04, FA-INC-02). */
@Component({
  selector: 'app-incidents-list-page',
  imports: [
    MatButtonModule,
    MatButtonToggleModule,
    MatFormFieldModule,
    MatSelectModule,
    RouterLink,
    TranslocoDirective,
    RelativeTimePipe,
    Icon,
    PageHeader,
    IncidentDuration,
  ],
  templateUrl: './incidents-list-page.html',
  styleUrl: './incidents-list-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class IncidentsListPage {
  private readonly api = inject(IncidentsService);
  private readonly monitorsApi = inject(MonitorsService);
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  private readonly stream = inject(LiveStreamService);
  private readonly transloco = inject(TranslocoService);
  private readonly destroyRef = inject(DestroyRef);

  protected readonly formatAbsolute = formatAbsolute;
  protected readonly states = INCIDENT_STATES;
  protected readonly ranges = INCIDENT_RANGES;

  /** Filters come from the URL, so every filtered view is shareable. */
  protected readonly filters = toSignal(this.route.queryParamMap.pipe(map(parseFilters)), {
    initialValue: parseFilters(this.route.snapshot.queryParamMap),
  });
  private readonly filterKey = computed(() => JSON.stringify(this.filters()));

  protected readonly lang = toSignal(this.transloco.langChanges$, {
    initialValue: this.transloco.getActiveLang(),
  });

  protected readonly incidents = signal<Incident[]>([]);
  protected readonly nextCursor = signal<string | null>(null);
  protected readonly state = signal<LoadState>('loading');
  protected readonly loadingMore = signal(false);
  protected readonly loadMoreFailed = signal(false);
  /** Ticks every 30 s so that ongoing durations stay current. */
  protected readonly now = signal(Date.now());

  protected readonly monitors = signal<Monitor[]>([]);
  protected readonly monitorsState = signal<LoadState>('loading');

  /** Bumped on every reload so responses of superseded requests are ignored. */
  private generation = 0;

  constructor() {
    effect(() => {
      // Reloads on filter changes and after a live-stream resynchronisation.
      this.filterKey();
      this.stream.resync();
      untracked(() => this.reload());
    });

    this.stream.events
      .pipe(
        filter(
          (e): e is StreamEvent & { incident: Incident } =>
            e.type === 'incident' && e.incident !== undefined,
        ),
        takeUntilDestroyed(this.destroyRef),
      )
      .subscribe((event) => {
        this.incidents.update((list) =>
          upsertIncident(list, event.incident, this.filters(), Date.now()),
        );
      });

    const timer = setInterval(() => this.now.set(Date.now()), TICK_MS);
    this.destroyRef.onDestroy(() => clearInterval(timer));

    this.loadMonitors();
  }

  protected setState(value: IncidentState): void {
    this.navigateWith({ state: value });
  }

  protected setRange(value: IncidentRange): void {
    this.navigateWith({ range: value });
  }

  protected setMonitor(value: string | null): void {
    this.navigateWith({ monitorId: value || null });
  }

  protected retry(): void {
    this.reload();
  }

  protected loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.loadingMore()) {
      return;
    }
    this.loadingMore.set(true);
    this.loadMoreFailed.set(false);
    this.fetchPage(cursor, this.generation, [], MAX_EMPTY_PAGES, true);
  }

  protected loadMonitors(): void {
    this.monitorsState.set('loading');
    this.monitorsApi
      .listMonitors({ limit: MONITOR_OPTIONS_LIMIT })
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (page) => {
          this.monitors.set([...page.items].sort((a, b) => a.name.localeCompare(b.name)));
          this.monitorsState.set('ok');
        },
        error: () => this.monitorsState.set('error'),
      });
  }

  protected durationOf(incident: Incident, now: number): number {
    return incidentDurationMs(incident, now);
  }

  private navigateWith(patch: Partial<IncidentFilters>): void {
    const next: IncidentFilters = { ...this.filters(), ...patch };
    void this.router.navigate([], {
      relativeTo: this.route,
      queryParams: toQueryParams(next),
    });
  }

  private reload(): void {
    const gen = ++this.generation;
    this.state.set('loading');
    this.loadingMore.set(false);
    this.loadMoreFailed.set(false);
    this.fetchPage(null, gen, [], MAX_EMPTY_PAGES, false);
  }

  /**
   * Fetches one page. For the client-side "resolved" filter, pages without a
   * visible match are skipped (up to MAX_EMPTY_PAGES) so the list is not
   * blank while more matches exist.
   */
  private fetchPage(
    cursor: string | null,
    gen: number,
    collected: Incident[],
    pagesLeft: number,
    append: boolean,
  ): void {
    const filters = this.filters();
    const now = Date.now();
    this.api
      .listIncidents({
        ...toApiParams(filters, now),
        limit: PAGE_SIZE,
        cursor: cursor ?? undefined,
      })
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (page) => {
          if (gen !== this.generation) {
            return;
          }
          const visible = page.items.filter((i) => matchesFilters(i, filters, now));
          const found = [...collected, ...visible];
          if (visible.length === 0 && page.nextCursor && pagesLeft > 1) {
            this.fetchPage(page.nextCursor, gen, found, pagesLeft - 1, append);
            return;
          }
          const base = append ? this.incidents() : [];
          this.incidents.set(mergeIncidents(base, found));
          this.nextCursor.set(page.nextCursor ?? null);
          this.state.set('ok');
          this.loadingMore.set(false);
        },
        error: () => {
          if (gen !== this.generation) {
            return;
          }
          this.loadingMore.set(false);
          if (append) {
            this.loadMoreFailed.set(true);
          } else {
            this.state.set('error');
          }
        },
      });
  }
}
