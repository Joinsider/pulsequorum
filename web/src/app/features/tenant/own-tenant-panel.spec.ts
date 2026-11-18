// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Observable, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it } from 'vitest';
import { TenantService } from '../../api/gen/endpoints/tenant/tenant.service';
import { OwnTenant } from '../../api/gen/model';
import { OwnTenantPanel } from './own-tenant-panel';

const OWN: OwnTenant = {
  id: 'acme',
  name: 'Acme',
  plan: 'starter',
  effectiveLimits: { maxMonitors: 3, minIntervalSeconds: 60, maxChannels: 5 },
  usage: { monitors: 3, channels: 1, minIntervalSeconds: 30 },
  exceeded: [{ limit: 'minIntervalSeconds', allowed: 60, actual: 30 }],
};

describe('OwnTenantPanel', () => {
  let fixture: ComponentFixture<OwnTenantPanel>;

  function render(response: Observable<OwnTenant>): void {
    TestBed.configureTestingModule({
      imports: [
        OwnTenantPanel,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [{ provide: TenantService, useValue: { getOwnTenant: () => response } }],
    });
    fixture = TestBed.createComponent(OwnTenantPanel);
    fixture.detectChanges();
  }

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('shows one row per limit and marks the reached and exceeded ones', () => {
    render(of(OWN));

    const rows = [...root().querySelectorAll<HTMLElement>('[data-testid="usage-row"]')];
    expect(rows).toHaveLength(3);
    expect(rows[0].classList).toContain('row--reached');
    expect(rows[2].classList).toContain('row--exceeded');
    expect(rows[1].classList).not.toContain('row--reached');
    expect(rows[1].classList).not.toContain('row--exceeded');
  });

  it('shows a hint for a reached limit and for an exceeded interval', () => {
    render(of(OWN));

    const hints = [...root().querySelectorAll('.hint')].map((el) => el.textContent?.trim());
    expect(hints).toEqual([
      'dashboard.usage.hints.reached.monitors',
      'dashboard.usage.hints.exceeded.interval',
    ]);
  });

  it('renders nothing when the tenant cannot be loaded', () => {
    render(throwError(() => new Error('offline')));

    expect(root().querySelector('section')).toBeNull();
  });
});
