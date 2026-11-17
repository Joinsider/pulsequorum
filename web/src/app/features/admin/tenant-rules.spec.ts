// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { describe, expect, it } from 'vitest';
import { Tenant } from '../../api/gen/model';
import { emptyLimitTexts } from './limit-rules';
import {
  TENANT_ID_PATTERN,
  TenantFormValues,
  isReservedTenant,
  tenantCreatePayload,
  tenantPatch,
  tenantRights,
} from './tenant-rules';

const TENANT: Tenant = {
  id: 'acme',
  name: 'Acme',
  plan: 'starter',
  orgId: 'org-1',
  limits: {},
  effectiveLimits: { maxMonitors: 10, minIntervalSeconds: 60, maxChannels: 2 },
  suspended: false,
  source: 'admin',
  usage: { monitors: 3, channels: 1, minIntervalSeconds: 60 },
  exceeded: [],
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
};

/** Form values of an unchanged edit of TENANT; tests override single fields. */
function values(overrides: Partial<TenantFormValues> = {}): TenantFormValues {
  return {
    id: '',
    name: 'Acme',
    plan: 'starter',
    orgId: 'org-1',
    limits: emptyLimitTexts(),
    unlimited: {},
    ...overrides,
  };
}

describe('TENANT_ID_PATTERN', () => {
  it.each(['acme', 'a', 'acme-2', '0tenant'])('accepts %j', (id) => {
    expect(TENANT_ID_PATTERN.test(id)).toBe(true);
  });

  it.each(['-acme', 'Acme', 'acme_2', '', 'a'.repeat(65)])('rejects %j', (id) => {
    expect(TENANT_ID_PATTERN.test(id)).toBe(false);
  });
});

describe('isReservedTenant', () => {
  it('marks default and _system as reserved', () => {
    expect(isReservedTenant('default')).toBe(true);
    expect(isReservedTenant('_system')).toBe(true);
    expect(isReservedTenant('acme')).toBe(false);
  });
});

describe('tenantRights', () => {
  it('offers no suspend, resume or delete for the reserved tenants', () => {
    const rights = tenantRights({ ...TENANT, id: 'default' });
    expect(rights.canSuspendToggle).toBe(false);
    expect(rights.showDelete).toBe(false);
    expect(rights.canDelete).toBe(false);
  });

  it('allows deleting only a suspended tenant, with the reason otherwise', () => {
    expect(tenantRights(TENANT)).toMatchObject({
      canSuspendToggle: true,
      showDelete: true,
      canDelete: false,
      deleteLockKey: 'admin.tenants.lock.suspendFirst',
    });
    expect(tenantRights({ ...TENANT, suspended: true })).toMatchObject({
      canDelete: true,
      deleteLockKey: null,
    });
  });
});

describe('tenantCreatePayload', () => {
  it('sends only the required fields when the optional ones are empty', () => {
    expect(tenantCreatePayload(values({ name: '  Acme  ', id: ' ', orgId: '' }))).toEqual({
      name: 'Acme',
      plan: 'starter',
    });
  });

  it('sends the ID and organization when given, and limits with null for unlimited', () => {
    const payload = tenantCreatePayload(
      values({
        id: 'acme',
        orgId: ' org-9 ',
        limits: { ...emptyLimitTexts(), maxMonitors: '25', maxChannels: '' },
        unlimited: { maxStatusPages: true },
      }),
    );
    expect(payload).toEqual({
      id: 'acme',
      name: 'Acme',
      plan: 'starter',
      orgId: 'org-9',
      limits: { maxMonitors: 25, maxStatusPages: null },
    });
  });

  it('leaves limits out when every field inherits the plan', () => {
    expect(tenantCreatePayload(values()).limits).toBeUndefined();
  });
});

describe('tenantPatch', () => {
  it('sends nothing for an unchanged tenant', () => {
    expect(tenantPatch(TENANT, values())).toEqual({});
  });

  it('sends only the changed name and plan', () => {
    expect(tenantPatch(TENANT, values({ name: ' Acme Ltd ', plan: 'pro' }))).toEqual({
      name: 'Acme Ltd',
      plan: 'pro',
    });
  });

  it('sends an empty orgId to unbind a bound organization', () => {
    expect(tenantPatch(TENANT, values({ orgId: '' }))).toEqual({ orgId: '' });
  });

  it('never sends the organization of the default tenant', () => {
    const defaultTenant: Tenant = { ...TENANT, id: 'default', name: 'Default', orgId: 'platform' };
    expect(tenantPatch(defaultTenant, values({ orgId: 'other', name: 'Default' }))).toEqual({});
  });

  it('sends all overrides at once when one limit changes, and {} to remove them', () => {
    const withOverride: Tenant = { ...TENANT, limits: { maxMonitors: 5 } };
    expect(
      tenantPatch(withOverride, values({ limits: { ...emptyLimitTexts(), maxMonitors: '7' } })),
    ).toEqual({ limits: { maxMonitors: 7 } });
    expect(tenantPatch(withOverride, values())).toEqual({ limits: {} });
  });

  it('treats an unlimited override as a change from an inherited limit', () => {
    expect(tenantPatch(TENANT, values({ unlimited: { maxChannels: true } }))).toEqual({
      limits: { maxChannels: null },
    });
  });

  it('keeps an unchanged unlimited override without sending it again', () => {
    const unlimited: Tenant = { ...TENANT, limits: { maxChannels: null } };
    expect(tenantPatch(unlimited, values({ unlimited: { maxChannels: true } }))).toEqual({});
  });
});
