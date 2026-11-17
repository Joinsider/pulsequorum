// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MAT_DIALOG_DATA, MatDialogRef } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { Plan, Tenant } from '../../api/gen/model';
import { TenantDialog, TenantDialogData } from './tenant-dialog';

const PLANS: Plan[] = [
  {
    id: 'default',
    name: 'Default',
    limits: {},
    tenantCount: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
  },
];

const TENANT: Tenant = {
  id: 'acme',
  name: 'Acme',
  plan: 'default',
  orgId: 'org-1',
  limits: { maxMonitors: null },
  effectiveLimits: {},
  suspended: false,
  source: 'admin',
  usage: { monitors: 0, channels: 0 },
  exceeded: [],
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
};

describe('TenantDialog', () => {
  let fixture: ComponentFixture<TenantDialog>;
  let api: { createTenant: ReturnType<typeof vi.fn>; updateTenant: ReturnType<typeof vi.fn> };
  let ref: { close: ReturnType<typeof vi.fn> };

  function setup(data: TenantDialogData): Record<string, any> {
    api = {
      createTenant: vi.fn(() => of(TENANT)),
      updateTenant: vi.fn(() => of(TENANT)),
    };
    ref = { close: vi.fn() };
    TestBed.configureTestingModule({
      imports: [
        TenantDialog,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        { provide: MAT_DIALOG_DATA, useValue: data },
        { provide: MatDialogRef, useValue: ref },
        { provide: TenantAdminService, useValue: api },
      ],
    });
    fixture = TestBed.createComponent(TenantDialog);
    fixture.detectChanges();
    return fixture.componentInstance as unknown as Record<string, any>;
  }

  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('keeps the organization of the default tenant read-only', () => {
    const p = setup({
      mode: 'edit',
      plans: PLANS,
      tenant: { ...TENANT, id: 'default', name: 'Default' },
    });

    expect(p['form'].controls.orgId.disabled).toBe(true);
  });

  it('sends a PATCH with only the changed name and closes with true', async () => {
    const p = setup({ mode: 'edit', plans: PLANS, tenant: TENANT });
    p['form'].controls.name.setValue('Acme Ltd');

    await p['submit']();

    expect(api.updateTenant).toHaveBeenCalledWith('acme', { name: 'Acme Ltd' });
    expect(ref.close).toHaveBeenCalledWith(true);
  });

  it('closes without a request when nothing changed', async () => {
    const p = setup({ mode: 'edit', plans: PLANS, tenant: TENANT });

    await p['submit']();

    expect(api.updateTenant).not.toHaveBeenCalled();
    expect(ref.close).toHaveBeenCalledWith(false);
  });

  it('creates a tenant with an idempotency key and only the filled fields', async () => {
    const p = setup({ mode: 'create', plans: PLANS });
    p['form'].controls.id.setValue('globex');
    p['form'].controls.name.setValue('Globex');
    p['form'].controls.plan.setValue('default');

    await p['submit']();

    const [body, options] = api.createTenant.mock.calls[0];
    expect(body).toEqual({ id: 'globex', name: 'Globex', plan: 'default' });
    expect(options.headers['Idempotency-Key']).toMatch(/^[0-9a-f-]{36}$/);
    expect(ref.close).toHaveBeenCalledWith(true);
  });

  it('disables the limit field and sends null when "Unlimited" is checked', async () => {
    const p = setup({ mode: 'edit', plans: PLANS, tenant: TENANT });
    p['setUnlimited']('maxMonitors', false);
    p['setUnlimited']('maxChannels', true);

    expect(p['form'].controls.limits.controls.maxChannels.disabled).toBe(true);
    expect(p['form'].controls.limits.controls.maxMonitors.enabled).toBe(true);

    await p['submit']();

    expect(api.updateTenant).toHaveBeenCalledWith('acme', { limits: { maxChannels: null } });
  });
});
