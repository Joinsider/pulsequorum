// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  Tenant,
  TenantCreate,
  TenantCreateLimits,
  TenantUpdate,
  TenantUpdateLimits,
} from '../../api/gen/model';
import { LimitKey, LimitTexts, limitValuesOf, sameLimitValues } from './limit-rules';

/**
 * Client-side rules of the tenant admin area (ADR 0029). The UI uses them to
 * build requests and to disable actions early; the server enforces every rule again.
 */

/** Same pattern as the OpenAPI spec for tenant and plan IDs. */
export const TENANT_ID_PATTERN = /^[a-z0-9][a-z0-9-]{0,63}$/;
export const TENANT_NAME_MAX_LENGTH = 100;
export const ORG_ID_MAX_LENGTH = 128;

/** The tenant that holds the platform's bootstrap data; its organization comes from the server configuration. */
export const DEFAULT_TENANT_ID = 'default';
/** The platform's own tenant. It is not listed by the API. */
export const SYSTEM_TENANT_ID = '_system';

/** Values of the create and edit dialog. */
export interface TenantFormValues {
  readonly id: string;
  readonly name: string;
  readonly plan: string;
  readonly orgId: string;
  readonly limits: LimitTexts;
  readonly unlimited: Partial<Record<LimitKey, boolean>>;
}

/** True for tenants the platform manages itself: never suspended or deleted from the UI. */
export function isReservedTenant(id: string): boolean {
  return id === DEFAULT_TENANT_ID || id === SYSTEM_TENANT_ID;
}

/** The organization of `default` comes from the server configuration and cannot be changed here. */
export function isOrgEditable(id: string): boolean {
  return id !== DEFAULT_TENANT_ID;
}

/** What the actor may do with one tenant row. The server enforces the same rules. */
export interface TenantRights {
  /** Suspend and resume are offered. */
  readonly canSuspendToggle: boolean;
  /** The delete button is shown at all. */
  readonly showDelete: boolean;
  /** The delete button is enabled: the tenant must be suspended first. */
  readonly canDelete: boolean;
  /** Translation key of the reason why deleting is not possible, or null. */
  readonly deleteLockKey: string | null;
}

export function tenantRights(tenant: Tenant): TenantRights {
  if (isReservedTenant(tenant.id)) {
    return {
      canSuspendToggle: false,
      showDelete: false,
      canDelete: false,
      deleteLockKey: null,
    };
  }
  return {
    canSuspendToggle: true,
    showDelete: true,
    canDelete: tenant.suspended,
    deleteLockKey: tenant.suspended ? null : 'admin.tenants.lock.suspendFirst',
  };
}

/** The POST body. Empty optional fields are left out; limits only when the form sets one. */
export function tenantCreatePayload(values: TenantFormValues): TenantCreate {
  const body: TenantCreate = { name: values.name.trim(), plan: values.plan };
  const id = values.id.trim();
  if (id !== '') {
    body.id = id;
  }
  const orgId = values.orgId.trim();
  if (orgId !== '') {
    body.orgId = orgId;
  }
  const limits = limitValuesOf(values.limits, values.unlimited);
  if (Object.keys(limits).length > 0) {
    body.limits = limits as TenantCreateLimits;
  }
  return body;
}

/**
 * The PATCH body with only the changed fields. `limits` replaces all overrides,
 * so it is sent whole whenever any override changed (an empty object removes them).
 * An empty result means nothing to send.
 */
export function tenantPatch(tenant: Tenant, values: TenantFormValues): TenantUpdate {
  const patch: TenantUpdate = {};
  const name = values.name.trim();
  if (name !== tenant.name) {
    patch.name = name;
  }
  if (values.plan !== tenant.plan) {
    patch.plan = values.plan;
  }
  if (isOrgEditable(tenant.id)) {
    const orgId = values.orgId.trim();
    if (orgId !== (tenant.orgId ?? '')) {
      // An empty string unbinds the organization (OpenAPI TenantUpdate.orgId).
      patch.orgId = orgId;
    }
  }
  const overrides = limitValuesOf(values.limits, values.unlimited);
  if (!sameLimitValues(overrides, tenant.limits)) {
    patch.limits = overrides as TenantUpdateLimits;
  }
  return patch;
}

/** Translation key of a suspended tenant's status label. */
export function statusKey(tenant: Tenant): string {
  return tenant.suspended ? 'admin.tenants.status.suspended' : 'admin.tenants.status.active';
}
