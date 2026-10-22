// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Subject, Observable, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Incident, IncidentPage, StreamEvent } from '../../api/gen/model';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { IncidentsListPage } from './incidents-list-page';

const MON = '11111111-1111-4111-8111-111111111111';
/**
 * Fixed start time for fixtures. The list is sorted newest first by startedAt
 * (ties by id), so per-call Date.now() values would make the expected row order
 * depend on whether two calls fall into the same millisecond.
 */
const STARTED_AT = '2026-10-01T12:00:00.000Z';
/** Strictly later than STARTED_AT, used for the live-inserted incident. */
const LIVE_STARTED_AT = '2026-10-02T12:00:00.000Z';

function incident(id: string, overrides: Partial<Incident> = {}): Incident {
  return {
    id,
    monitorId: MON,
    monitorName: `monitor-${id}`,
    startedAt: STARTED_AT,
    cause: `cause-${id}`,
    ...overrides,
  };
}

interface Harness {
  fixture: ComponentFixture<IncidentsListPage>;
  listIncidents: ReturnType<typeof vi.fn>;
  events: Subject<StreamEvent>;
  resync: { value: number };
}

function setup(listResult: () => Observable<IncidentPage>): Harness {
  const events = new Subject<StreamEvent>();
  const resyncSignal = { value: 0 };
  const listIncidents = vi.fn(listResult);
  TestBed.configureTestingModule({
    imports: [
      IncidentsListPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([]),
      { provide: IncidentsService, useValue: { listIncidents } },
      { provide: MonitorsService, useValue: { listMonitors: () => of({ items: [] }) } },
      {
        provide: LiveStreamService,
        useValue: { events: events.asObservable(), resync: () => resyncSignal.value },
      },
    ],
  });
  const fixture = TestBed.createComponent(IncidentsListPage);
  fixture.detectChanges();
  return { fixture, listIncidents, events, resync: resyncSignal };
}

function text(fixture: ComponentFixture<IncidentsListPage>): string {
  return (fixture.nativeElement as HTMLElement).textContent ?? '';
}

function rowIds(fixture: ComponentFixture<IncidentsListPage>): string[] {
  return Array.from((fixture.nativeElement as HTMLElement).querySelectorAll('.row__cause')).map(
    (el) => el.textContent?.trim() ?? '',
  );
}

describe('IncidentsListPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('renders the incidents of the first page', () => {
    const { fixture } = setup(() =>
      of({ items: [incident('a'), incident('b')], nextCursor: null }),
    );
    expect(rowIds(fixture)).toEqual(['cause-a', 'cause-b']);
    expect(text(fixture)).toContain('monitor-a');
  });

  it('shows the empty state when no incident matches', () => {
    const { fixture } = setup(() => of({ items: [], nextCursor: null }));
    expect(text(fixture)).toContain('incidents.list.empty');
  });

  it('shows an error with a retry button and reloads on retry', () => {
    let fail = true;
    const { fixture, listIncidents } = setup(() =>
      fail
        ? throwError(() => new HttpErrorResponse({ status: 500 }))
        : of({ items: [incident('a')], nextCursor: null }),
    );
    expect(text(fixture)).toContain('incidents.list.loadError');

    fail = false;
    const retry = Array.from(
      (fixture.nativeElement as HTMLElement).querySelectorAll('button'),
    ).find((b) => b.textContent?.includes('common.retry'));
    retry?.click();
    fixture.detectChanges();

    expect(listIncidents).toHaveBeenCalledTimes(2);
    expect(rowIds(fixture)).toEqual(['cause-a']);
  });

  it('loads the next page with the cursor', () => {
    const first: IncidentPage = { items: [incident('a')], nextCursor: 'c1' };
    const second: IncidentPage = { items: [incident('b')], nextCursor: null };
    const { fixture, listIncidents } = setup(() => of(first));
    listIncidents.mockImplementation((params: { cursor?: string }) =>
      of(params.cursor ? second : first),
    );

    const more = Array.from((fixture.nativeElement as HTMLElement).querySelectorAll('button')).find(
      (b) => b.textContent?.includes('incidents.list.loadMore'),
    );
    more?.click();
    fixture.detectChanges();

    expect(listIncidents).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: 'c1' }));
    expect(rowIds(fixture)).toEqual(['cause-a', 'cause-b']);
  });

  it('inserts a live incident that matches the filters', () => {
    const { fixture, events } = setup(() => of({ items: [incident('a')], nextCursor: null }));
    events.next({
      type: 'incident',
      ts: new Date().toISOString(),
      incident: incident('live', { startedAt: LIVE_STARTED_AT }),
    });
    fixture.detectChanges();
    expect(rowIds(fixture)).toEqual(['cause-live', 'cause-a']);
  });

  it('updates an existing row when its incident is resolved', () => {
    const { fixture, events } = setup(() => of({ items: [incident('a')], nextCursor: null }));
    events.next({
      type: 'incident',
      ts: new Date().toISOString(),
      incident: incident('a', { resolvedAt: new Date().toISOString() }),
    });
    fixture.detectChanges();
    expect(rowIds(fixture)).toEqual(['cause-a']);
    expect(text(fixture)).toContain('incidents.chip.resolved');
    expect(text(fixture)).not.toContain('incidents.chip.ongoing');
  });

  it('marks unconfirmed incidents', () => {
    const { fixture } = setup(() =>
      of({ items: [incident('a', { confirmed: false })], nextCursor: null }),
    );
    expect(text(fixture)).toContain('incidents.chip.unconfirmed');
  });
});
