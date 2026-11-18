// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { AbstractControl, ValidationErrors, ValidatorFn } from '@angular/forms';
import { Plan, PlanInput } from '../../api/gen/model';
import { LIMIT_KEYS, LimitTexts, parseLimitText } from './limit-rules';

/**
 * Client-side rules of the plan admin area (ADR 0029). The server enforces every rule again.
 */

/** Same pattern as the OpenAPI spec for plan IDs. */
export const PLAN_ID_PATTERN = /^[a-z0-9][a-z0-9-]{0,63}$/;
export const PLAN_NAME_MAX_LENGTH = 100;

/** The plan every new tenant starts with; it cannot be deleted. */
export const DEFAULT_PLAN_ID = 'default';
/** The plan of the platform's own tenant. It is not listed and cannot be changed. */
export const SYSTEM_PLAN_ID = 'system';

/** Values of the create and edit dialog. An empty limit field means unlimited. */
export interface PlanFormValues {
  readonly id: string;
  readonly name: string;
  readonly limits: LimitTexts;
}

/** What the actor may do with one plan row. */
export interface PlanRights {
  readonly canDelete: boolean;
  /** Translation key of the reason why deleting is not possible, or null. */
  readonly deleteLockKey: string | null;
}

export function planRights(plan: Plan): PlanRights {
  if (plan.id === DEFAULT_PLAN_ID) {
    return { canDelete: false, deleteLockKey: 'admin.plans.lock.default' };
  }
  if (plan.tenantCount > 0) {
    return { canDelete: false, deleteLockKey: 'admin.plans.lock.inUse' };
  }
  return { canDelete: true, deleteLockKey: null };
}

/**
 * The PUT body. Every limit is sent: an empty field becomes null (unlimited),
 * so the stored plan matches the form exactly.
 */
export function planInput(values: PlanFormValues): PlanInput {
  // Only the five known names: PlanInput.limits is a map and unknown names are refused (422).
  const limits: PlanInput['limits'] = {};
  for (const key of LIMIT_KEYS) {
    const parsed = parseLimitText(values.limits[key]);
    limits[key] = parsed === undefined || Number.isNaN(parsed) ? null : parsed;
  }
  return { name: values.name.trim(), limits };
}

/** True if the input differs from the stored plan. Absent and null both mean unlimited here. */
export function planChanged(plan: Plan, input: PlanInput): boolean {
  if (input.name !== plan.name) {
    return true;
  }
  return LIMIT_KEYS.some((key) => (input.limits[key] ?? null) !== (plan.limits[key] ?? null));
}

/**
 * Validator of a new plan ID: required, the OpenAPI pattern, not `system` and not
 * an existing plan. Without it a PUT would silently replace the existing plan.
 * Error keys: `required`, `pattern`, `reserved`, `taken`.
 */
export function planIdValidator(existingIds: readonly string[]): ValidatorFn {
  return (control: AbstractControl): ValidationErrors | null => {
    const id = String(control.value ?? '').trim();
    if (id === '') {
      return { required: true };
    }
    if (!PLAN_ID_PATTERN.test(id)) {
      return { pattern: true };
    }
    if (id === SYSTEM_PLAN_ID) {
      return { reserved: true };
    }
    return existingIds.includes(id) ? { taken: true } : null;
  };
}
