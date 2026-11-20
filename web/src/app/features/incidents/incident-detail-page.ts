// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  DestroyRef,
  effect,
  inject,
  signal,
  untracked,
} from '@angular/core';
import { takeUntilDestroyed, toSignal } from '@angular/core/rxjs-interop';
import { MatButtonModule } from '@angular/material/button';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { filter, map } from 'rxjs';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { Incident, StreamEvent } from '../../api/gen/model';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { problemOf } from '../../shared/problem';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { IncidentDuration } from './incident-duration';
import { formatAbsolute, incidentDurationMs } from './incident.logic';

type LoadState = 'loading' | 'ok' | 'error' | 'notFound';

const TICK_MS = 30_000;

/** All fields of one incident (FA-UI-04). The list filters are kept in the URL for the back link. */
@Component({
  selector: 'app-incident-detail-page',
  imports: [
    MatButtonModule,
    RouterLink,
    TranslocoDirective,
    RelativeTimePipe,
    Icon,
    PageHeader,
    IncidentDuration,
  ],
  templateUrl: './incident-detail-page.html',
  styleUrl: './incident-detail-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class IncidentDetailPage {
  private readonly api = inject(IncidentsService);
  private readonly route = inject(ActivatedRoute);
  private readonly stream = inject(LiveStreamService);
  private readonly transloco = inject(TranslocoService);
  private readonly destroyRef = inject(DestroyRef);

  protected readonly formatAbsolute = formatAbsolute;
  protected readonly lang = toSignal(this.transloco.langChanges$, {
    initialValue: this.transloco.getActiveLang(),
  });

  private readonly incidentId = toSignal(
    this.route.paramMap.pipe(map((params) => params.get('incidentId') ?? '')),
    { initialValue: this.route.snapshot.paramMap.get('incidentId') ?? '' },
  );

  protected readonly incident = signal<Incident | null>(null);
  protected readonly state = signal<LoadState>('loading');
  protected readonly now = signal(Date.now());

  /** Bumped on every load so responses of superseded requests are ignored. */
  private generation = 0;

  constructor() {
    effect(() => {
      // Loads on navigation to another incident and after a live-stream resynchronisation.
      const id = this.incidentId();
      this.stream.resync();
      untracked(() => this.load(id));
    });

    this.stream.events
      .pipe(
        filter(
          (e): e is StreamEvent & { incident: Incident } =>
            e.type === 'incident' && e.incident !== undefined,
        ),
        filter((e) => e.incident.id === this.incidentId()),
        takeUntilDestroyed(this.destroyRef),
      )
      .subscribe((event) => {
        this.incident.set(event.incident);
        this.state.set('ok');
      });

    const timer = setInterval(() => this.now.set(Date.now()), TICK_MS);
    this.destroyRef.onDestroy(() => clearInterval(timer));
  }

  protected retry(): void {
    this.load(this.incidentId());
  }

  protected durationOf(incident: Incident, now: number): number {
    return incidentDurationMs(incident, now);
  }

  private load(id: string): void {
    const gen = ++this.generation;
    this.state.set('loading');
    this.api
      .getIncident(id)
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (incident) => {
          if (gen !== this.generation) {
            return;
          }
          this.incident.set(incident);
          this.state.set('ok');
        },
        error: (err: unknown) => {
          if (gen !== this.generation) {
            return;
          }
          this.incident.set(null);
          this.state.set(problemOf(err)?.status === 404 ? 'notFound' : 'error');
        },
      });
  }
}
