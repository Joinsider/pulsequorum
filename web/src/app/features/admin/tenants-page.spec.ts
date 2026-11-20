// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Plan, Tenant } from '../../api/gen/model';
import { TenantsPage } from './tenants-page';

const PLANS: Plan[] = [
  {
    id: 'default',
    name: 'Default',
    limits: {},
    tenantCount: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
  },
  {
    id: 'starter',
    name: 'Starter',
    limits: { maxMonitors: 10 },
    tenantCount: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
  },
];

function tenant(overrides: Partial<Tenant>): Tenant {
  return {
    id: 'acme',
    name: 'Acme',
    plan: 'starter',
    limits: {},
    effectiveLimits: { maxMonitors: 10 },
    suspended: false,
    source: 'admin',
    usage: { monitors: 3, channels: 0 },
    exceeded: [],
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    ...overrides,
  };
}

const TENANTS: Tenant[] = [
  tenant({
    id: 'default',
    name: 'Default',
    plan: 'default',
    effectiveLimits: {},
    source: 'bootstrap',
  }),
  tenant({
    id: 'globex',
    name: 'Globex',
    usage: { monitors: 12, channels: 0, minIntervalSeconds: 30 },
    effectiveLimits: { maxMonitors: 10, minIntervalSeconds: 60 },
    exceeded: [{ limit: 'maxMonitors', allowed: 10, actual: 12 }],
  }),
  tenant({ id: 'idle', name: 'Idle', suspended: true }),
];

describe('TenantsPage', () => {
  let fixture: ComponentFixture<TenantsPage>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        TenantsPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(TenantsPage);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function button(label: string): HTMLButtonElement | null {
    return root().querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
  }

  async function loadWith(tenants: Tenant[]): Promise<void> {
    fixture.detectChanges();
    http.expectOne({ method: 'GET', url: '/api/v1/admin/tenants' }).flush(tenants);
    http.expectOne({ method: 'GET', url: '/api/v1/admin/plans' }).flush(PLANS);
    await fixture.whenStable();
    fixture.detectChanges();
  }

  it('lists the tenants and highlights the ones over a limit', async () => {
    await loadWith(TENANTS);

    const rows = [...root().querySelectorAll<HTMLElement>('[data-testid="tenant-row"]')];
    expect(rows).toHaveLength(3);
    expect(rows.map((r) => r.classList.contains('row--exceeded'))).toEqual([false, true, false]);
    expect(root().querySelector('.exceeded')?.textContent).toContain(
      'admin.tenants.exceeded.maxMonitors',
    );
  });

  it('shows the plan name instead of the plan ID when the plan is known', async () => {
    await loadWith(TENANTS);

    expect(root().textContent).toContain('admin.tenants.planLabel: Starter');
  });

  it('offers no suspend, resume or delete for the default tenant', async () => {
    await loadWith(TENANTS);

    expect(button('admin.tenants.suspend: Default')).toBeNull();
    expect(button('admin.tenants.resume: Default')).toBeNull();
    expect(button('common.delete: Default')).toBeNull();
    expect(button('common.edit: Default')).not.toBeNull();
  });

  it('enables delete only for a suspended tenant', async () => {
    await loadWith(TENANTS);

    expect(button('common.delete: Globex')?.disabled).toBe(true);
    expect(button('common.delete: Idle')?.disabled).toBe(false);
    expect(button('admin.tenants.resume: Idle')).not.toBeNull();
    expect(button('admin.tenants.suspend: Globex')).not.toBeNull();
  });

  it('shows a translated error with the server detail when the list cannot be loaded', async () => {
    fixture.detectChanges();
    http.expectOne({ method: 'GET', url: '/api/v1/admin/plans' }).flush([]);
    http
      .expectOne({ method: 'GET', url: '/api/v1/admin/tenants' })
      .flush({ title: 'Forbidden', status: 403 }, { status: 403, statusText: 'Forbidden' });
    await fixture.whenStable();
    fixture.detectChanges();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'admin.failure.forbidden',
    );
  });
});
