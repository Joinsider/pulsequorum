// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { provideEchartsCore } from 'ngx-echarts';
import { Subject, of, throwError } from 'rxjs';
import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { IncidentsService } from '../../api/gen/endpoints/incidents/incidents.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { ErrorClass, Heartbeat, Monitor, UptimeStats } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { MonitorDetailPage } from './monitor-detail-page';

const MONITOR: Monitor = {
  id: 'm1',
  name: 'Website',
  type: 'http',
  config: { url: 'https://example.com' },
  intervalSeconds: 60,
  status: 'UP',
  statusSince: new Date(Date.now() - 3600_000).toISOString(),
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

const UPTIME: UptimeStats = {
  range: '24h',
  uptimePercent: 99.5,
  upSeconds: 86_000,
  downSeconds: 400,
  maintenanceSeconds: 0,
  unknownSeconds: 0,
  latencyAvgMs: 42,
  latencyP95Ms: 90,
  buckets: [],
};

const SSRF_HINT =
  'The target address is in a blocked network range (private, loopback, link-local or cloud metadata). SSRF protection is on for this installation.';

interface Setup {
  monitor?: () => ReturnType<MonitorsService['getMonitor']>;
  canEdit?: boolean;
  heartbeats?: Heartbeat[];
}

function setup({
  monitor,
  canEdit = false,
  heartbeats = [],
}: Setup = {}): ComponentFixture<MonitorDetailPage> {
  const monitors = {
    getMonitor: monitor ?? (() => of(MONITOR)),
    listHeartbeats: () => of({ items: heartbeats }),
    getUptime: () => of(UPTIME),
  };
  const incidents = { listIncidents: () => of({ items: [] }) };
  const live = { events: new Subject().asObservable(), resync: signal(0).asReadonly() };

  TestBed.configureTestingModule({
    imports: [
      MonitorDetailPage,
      TranslocoTestingModule.forRoot({
        langs: {
          en: {
            monitorDetail: {
              errors: { notFound: 'Monitor not found' },
              errorHints: { ssrfBlocked: SSRF_HINT },
            },
          },
        },
        translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
      }),
    ],
    providers: [
      provideRouter([]),
      provideEchartsCore({ echarts: () => import('./echarts-setup').then((m) => m.default) }),
      {
        provide: ActivatedRoute,
        useValue: { paramMap: of(convertToParamMap({ id: 'm1' })), parent: null },
      },
      { provide: MonitorsService, useValue: monitors },
      { provide: IncidentsService, useValue: incidents },
      { provide: LiveStreamService, useValue: live },
      { provide: AuthService, useValue: { canEdit: signal(canEdit).asReadonly() } },
    ],
  });
  const fixture = TestBed.createComponent(MonitorDetailPage);
  fixture.detectChanges();
  return fixture;
}

async function settle(fixture: ComponentFixture<MonitorDetailPage>): Promise<string> {
  for (let i = 0; i < 5; i++) {
    await fixture.whenStable();
    fixture.detectChanges();
  }
  return (fixture.nativeElement as HTMLElement).textContent ?? '';
}

describe('MonitorDetailPage', () => {
  beforeAll(() => {
    // The latency chart observes its size; the test DOM has no ResizeObserver.
    if (!('ResizeObserver' in globalThis)) {
      globalThis.ResizeObserver = class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      } as unknown as typeof ResizeObserver;
    }
  });

  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('shows the monitor name, its target and the edit actions for editors', async () => {
    const fixture = setup({ canEdit: true });
    const text = await settle(fixture);

    expect(text).toContain('Website');
    expect(text).toContain('https://example.com');
    const edit = (fixture.nativeElement as HTMLElement).querySelector(
      'a[href="/monitors/m1/edit"]',
    );
    expect(edit).not.toBeNull();
  });

  it('hides the edit actions for viewers', async () => {
    const fixture = setup({ canEdit: false });
    await settle(fixture);

    expect(
      (fixture.nativeElement as HTMLElement).querySelector('a[href="/monitors/m1/edit"]'),
    ).toBeNull();
  });

  it('shows the not-found message when the monitor does not exist', async () => {
    const fixture = setup({
      monitor: () => throwError(() => new HttpErrorResponse({ status: 404 })),
    });
    const text = await settle(fixture);

    expect(text).toContain('Monitor not found');
  });

  it('shows the SSRF hint for a node result with error class ssrf_blocked', async () => {
    const fixture = setup({
      heartbeats: [heartbeat('ssrf_blocked')],
    });
    const text = await settle(fixture);

    expect(text).toContain('ssrf_blocked');
    expect(text).toContain(SSRF_HINT);
  });

  it('does not show the SSRF hint for another error class', async () => {
    const fixture = setup({
      heartbeats: [heartbeat('timeout')],
    });
    const text = await settle(fixture);

    expect(text).toContain('timeout');
    expect(text).not.toContain(SSRF_HINT);
  });
});

function heartbeat(errorClass: ErrorClass): Heartbeat {
  return {
    nodeId: 'node-1',
    ts: new Date(Date.now() - 60_000).toISOString(),
    status: 'DOWN',
    latencyMs: 0,
    errorClass,
    message: 'check failed',
  };
}
