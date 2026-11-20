// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Plan } from '../../api/gen/model';
import { PlansPage } from './plans-page';

function plan(overrides: Partial<Plan>): Plan {
  return {
    id: 'starter',
    name: 'Starter',
    limits: { maxMonitors: 10 },
    tenantCount: 0,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    ...overrides,
  };
}

describe('PlansPage', () => {
  let fixture: ComponentFixture<PlansPage>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        PlansPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(PlansPage);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function button(label: string): HTMLButtonElement | null {
    return root().querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
  }

  async function loadWith(plans: Plan[]): Promise<void> {
    fixture.detectChanges();
    http.expectOne({ method: 'GET', url: '/api/v1/admin/plans' }).flush(plans);
    await fixture.whenStable();
    fixture.detectChanges();
  }

  it('lists the plans with their limits and tenant count', async () => {
    await loadWith([plan({ tenantCount: 2 })]);

    expect(root().querySelector('.row__name')?.textContent?.trim()).toBe('Starter');
    expect(root().querySelector('[data-limit="maxMonitors"]')?.textContent).toContain('10');
    expect(root().querySelector('[data-limit="maxChannels"]')?.textContent).toContain(
      'admin.limits.unlimited',
    );
    expect(root().querySelector('.count')?.textContent).toContain('admin.plans.tenantCount');
  });

  it('disables delete while tenants use the plan', async () => {
    await loadWith([plan({ tenantCount: 2 })]);

    expect(button('common.delete: Starter')?.disabled).toBe(true);
  });

  it('disables delete for the default plan even when unused', async () => {
    await loadWith([plan({ id: 'default', name: 'Default', tenantCount: 0 })]);

    expect(button('common.delete: Default')?.disabled).toBe(true);
  });

  it('enables delete for an unused plan other than default', async () => {
    await loadWith([plan({ tenantCount: 0 })]);

    expect(button('common.delete: Starter')?.disabled).toBe(false);
  });
});
