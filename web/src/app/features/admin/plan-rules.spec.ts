// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { FormControl } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import { Plan } from '../../api/gen/model';
import { emptyLimitTexts } from './limit-rules';
import { PLAN_ID_PATTERN, planChanged, planIdValidator, planInput, planRights } from './plan-rules';

const PLAN: Plan = {
  id: 'starter',
  name: 'Starter',
  limits: { maxMonitors: 10, minIntervalSeconds: 60, maxChannels: null },
  tenantCount: 0,
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
};

describe('PLAN_ID_PATTERN', () => {
  it.each(['starter', 'pro-2', 'a'])('accepts %j', (id) => {
    expect(PLAN_ID_PATTERN.test(id)).toBe(true);
  });

  it.each(['Pro', '-pro', 'pro_2', ''])('rejects %j', (id) => {
    expect(PLAN_ID_PATTERN.test(id)).toBe(false);
  });
});

describe('planRights', () => {
  it('forbids deleting the default plan', () => {
    expect(planRights({ ...PLAN, id: 'default' })).toEqual({
      canDelete: false,
      deleteLockKey: 'admin.plans.lock.default',
    });
  });

  it('forbids deleting a plan that tenants use', () => {
    expect(planRights({ ...PLAN, tenantCount: 2 })).toEqual({
      canDelete: false,
      deleteLockKey: 'admin.plans.lock.inUse',
    });
  });

  it('allows deleting an unused plan', () => {
    expect(planRights(PLAN)).toEqual({ canDelete: true, deleteLockKey: null });
  });
});

describe('planInput', () => {
  it('sends every limit, with an empty field as null (unlimited)', () => {
    expect(
      planInput({
        id: 'starter',
        name: '  Starter  ',
        limits: { ...emptyLimitTexts(), maxMonitors: '10', minIntervalSeconds: '30' },
      }),
    ).toEqual({
      name: 'Starter',
      limits: {
        maxMonitors: 10,
        minIntervalSeconds: 30,
        maxChannels: null,
        maxStatusPages: null,
        maxPrivateProbes: null,
      },
    });
  });
});

describe('planChanged', () => {
  it('is false when name and limits match the stored plan', () => {
    const input = planInput({
      id: 'starter',
      name: 'Starter',
      limits: { ...emptyLimitTexts(), maxMonitors: '10', minIntervalSeconds: '60' },
    });
    expect(planChanged(PLAN, input)).toBe(false);
  });

  it('is true when a limit or the name changes', () => {
    const limits = { ...emptyLimitTexts(), maxMonitors: '11', minIntervalSeconds: '60' };
    expect(planChanged(PLAN, planInput({ id: 'starter', name: 'Starter', limits }))).toBe(true);
    expect(
      planChanged(
        PLAN,
        planInput({
          id: 'starter',
          name: 'Start',
          limits: { ...emptyLimitTexts(), maxMonitors: '10', minIntervalSeconds: '60' },
        }),
      ),
    ).toBe(true);
  });
});

describe('planIdValidator', () => {
  const validator = planIdValidator(['starter']);

  it('requires an ID', () => {
    expect(validator(new FormControl(' '))).toEqual({ required: true });
  });

  it('checks the pattern', () => {
    expect(validator(new FormControl('Pro'))).toEqual({ pattern: true });
  });

  it('refuses the reserved system plan and existing IDs', () => {
    expect(validator(new FormControl('system'))).toEqual({ reserved: true });
    expect(validator(new FormControl('starter'))).toEqual({ taken: true });
  });

  it('accepts a new valid ID', () => {
    expect(validator(new FormControl('pro'))).toBeNull();
  });
});
