// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Observable, Subject, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it } from 'vitest';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { Incident, StreamEvent } from '../../api/gen/model';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { IncidentDetailPage } from './incident-detail-page';

const ID = '33333333-3333-4333-8333-333333333333';
const MON = '11111111-1111-4111-8111-111111111111';

const INCIDENT: Incident = {
  id: ID,
  monitorId: MON,
  monitorName: 'api',
  startedAt: new Date(Date.now() - 7_200_000).toISOString(),
  resolvedAt: new Date(Date.now() - 3_600_000).toISOString(),
  cause: 'connection refused',
  errorClass: 'connect',
  affectedNodes: ['node-a', 'node-b'],
  confirmed: false,
};

function setup(result: Observable<Incident>): {
  fixture: ComponentFixture<IncidentDetailPage>;
  events: Subject<StreamEvent>;
} {
  const events = new Subject<StreamEvent>();
  TestBed.configureTestingModule({
    imports: [
      IncidentDetailPage,
      TranslocoTestingModule.forRoot({
        langs: { en: {} },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([]),
      { provide: IncidentsService, useValue: { getIncident: () => result } },
      {
        provide: LiveStreamService,
        useValue: { events: events.asObservable(), resync: () => 0 },
      },
      {
        provide: ActivatedRoute,
        useValue: {
          paramMap: of(convertToParamMap({ incidentId: ID })),
          snapshot: { paramMap: convertToParamMap({ incidentId: ID }) },
        },
      },
    ],
  });
  const fixture = TestBed.createComponent(IncidentDetailPage);
  fixture.detectChanges();
  return { fixture, events };
}

function text(fixture: ComponentFixture<IncidentDetailPage>): string {
  return (fixture.nativeElement as HTMLElement).textContent ?? '';
}

describe('IncidentDetailPage', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('shows all fields of the incident', () => {
    const { fixture } = setup(of(INCIDENT));
    const content = text(fixture);
    expect(content).toContain('connection refused');
    expect(content).toContain('api');
    expect(content).toContain('node-a');
    expect(content).toContain('node-b');
    expect(content).toContain('incidents.errorClass.connect');
    expect(content).toContain('incidents.detail.unconfirmed');
    expect(content).toContain(ID);
  });

  it('shows the not-found message for a 404', () => {
    const { fixture } = setup(
      throwError(
        () =>
          new HttpErrorResponse({
            status: 404,
            error: { type: 'about:blank', title: 'Not Found', status: 404 },
          }),
      ),
    );
    expect(text(fixture)).toContain('incidents.detail.notFound');
  });

  it('shows an error with retry for other failures', () => {
    const { fixture } = setup(throwError(() => new HttpErrorResponse({ status: 500 })));
    expect(text(fixture)).toContain('incidents.detail.loadError');
  });

  it('applies a live update for the shown incident', () => {
    const { fixture, events } = setup(of(INCIDENT));
    events.next({
      type: 'incident',
      ts: new Date().toISOString(),
      incident: { ...INCIDENT, resolvedAt: undefined, confirmed: true },
    });
    fixture.detectChanges();
    expect(text(fixture)).toContain('incidents.detail.ongoing');
  });
});
