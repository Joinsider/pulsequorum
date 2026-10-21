// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting } from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Subject, of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Monitor, MonitorStatus, StreamEvent } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { LiveStreamService } from '../../core/stream/live-stream.service';
import { DashboardPage } from './dashboard-page';

function monitor(id: string, name: string, status: MonitorStatus): Monitor {
  return {
    id,
    name,
    status,
    type: 'http',
    config: { url: `https://${id}.example` },
    intervalSeconds: 60,
    statusSince: '2026-10-01T00:00:00Z',
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    uptime24h: null,
    lastLatencyMs: null,
  };
}

describe('DashboardPage', () => {
  let fixture: ComponentFixture<DashboardPage>;
  let events: Subject<StreamEvent>;
  let listMonitors: ReturnType<typeof vi.fn>;
  let pauseMonitor: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    events = new Subject<StreamEvent>();
    listMonitors = vi.fn();
    pauseMonitor = vi.fn();
    TestBed.configureTestingModule({
      imports: [
        DashboardPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        { provide: AuthService, useValue: { canEdit: signal(false) } },
        {
          provide: LiveStreamService,
          useValue: { resync: signal(0), events: events.asObservable() },
        },
        { provide: MonitorsService, useValue: { listMonitors, pauseMonitor } },
      ],
    });
  });

  function create(): void {
    fixture = TestBed.createComponent(DashboardPage);
    fixture.detectChanges();
  }

  /** Lets the async page load finish, then renders the result. */
  async function settle(): Promise<void> {
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();
  }

  function text(): string {
    return (fixture.nativeElement as HTMLElement).textContent ?? '';
  }

  it('follows the cursor until the last page and renders sorted rows', async () => {
    listMonitors
      .mockReturnValueOnce(
        of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)], nextCursor: 'c2' }),
      )
      .mockReturnValueOnce(
        of({ items: [monitor('b', 'Alpha', MonitorStatus.DOWN)], nextCursor: null }),
      );
    create();
    await settle();

    expect(listMonitors).toHaveBeenNthCalledWith(2, expect.objectContaining({ cursor: 'c2' }));
    const names = Array.from(
      (fixture.nativeElement as HTMLElement).querySelectorAll('tbody .name'),
    ).map((el) => el.textContent?.trim());
    expect(names).toEqual(['Alpha', 'Zeta']);
  });

  it('applies a state event to the rendered status', async () => {
    listMonitors.mockReturnValue(of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)] }));
    create();
    await settle();

    events.next({
      type: 'state',
      ts: '2026-10-21T11:00:00Z',
      monitorId: 'a',
      status: MonitorStatus.DOWN,
    });
    fixture.detectChanges();

    expect(text()).toContain('status.DOWN');
  });

  it('hides edit actions without edit rights', async () => {
    listMonitors.mockReturnValue(of({ items: [monitor('a', 'Zeta', MonitorStatus.UP)] }));
    create();
    await settle();

    expect((fixture.nativeElement as HTMLElement).querySelector('td.actions')).toBeNull();
    expect(
      (fixture.nativeElement as HTMLElement).querySelector('a[href="/monitors/new"]'),
    ).toBeNull();
  });
});
