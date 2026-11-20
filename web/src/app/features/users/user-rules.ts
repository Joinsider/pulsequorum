// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { AbstractControl, ValidationErrors } from '@angular/forms';
import { Me, Role, User, UserUpdate } from '../../api/gen/model';

/**
 * Client-side mirror of internal/api/user_policy.go. The UI uses it to disable
 * actions early and explain why; the server enforces every rule again.
 */

/** Mirrors internal/auth/password.go (MinPasswordLen, MaxPasswordLen). */
export const MIN_PASSWORD_LENGTH = 12;
export const MAX_PASSWORD_LENGTH = 256;

const ROLES: readonly Role[] = ['platform-admin', 'tenant-admin', 'tenant-editor', 'tenant-viewer'];

/** Roles the actor may assign: only platform admins may assign platform-admin. */
export function assignableRoles(actorRoles: readonly Role[]): Role[] {
  const actorIsPlatform = actorRoles.includes('platform-admin');
  return ROLES.filter((role) => role !== 'platform-admin' || actorIsPlatform);
}

/** What the actor may do with one user row. */
export interface UserRights {
  /** False if the row cannot be edited at all (platform admin, actor is not one). */
  editable: boolean;
  canChangeRole: boolean;
  canDisable: boolean;
  canDelete: boolean;
  /** Admin password reset. Not for oneself: that needs the current password (/account). */
  canResetPassword: boolean;
  /** Translation key of the reason for locked actions, or null. */
  lockKey: string | null;
}

/** Rights of the actor on one user of the same tenant; `users` is the full tenant list. */
export function userRights(target: User, actor: Me, users: readonly User[]): UserRights {
  const actorIsPlatform = actor.roles.includes('platform-admin');
  if (target.role === 'platform-admin' && !actorIsPlatform) {
    return {
      editable: false,
      canChangeRole: false,
      canDisable: false,
      canDelete: false,
      canResetPassword: false,
      lockKey: 'users.lock.platformTarget',
    };
  }
  const self = target.id === actor.id;
  const lastPlatformAdmin =
    target.role === 'platform-admin' &&
    !target.disabled &&
    !users.some((u) => u.id !== target.id && u.role === 'platform-admin' && !u.disabled);
  const locked = self || lastPlatformAdmin;
  return {
    editable: true,
    canChangeRole: !locked,
    canDisable: !locked,
    canDelete: !locked,
    canResetPassword: !self,
    lockKey: self ? 'users.lock.self' : lastPlatformAdmin ? 'users.lock.lastPlatformAdmin' : null,
  };
}

/** Group validator: the password and its repetition must be equal (both empty is fine). */
export function passwordsMatch(group: AbstractControl): ValidationErrors | null {
  const password = group.get('password')?.value;
  const repeat = group.get('passwordRepeat')?.value;
  return password !== repeat ? { mismatch: true } : null;
}

/** Values of the edit form that can be sent to the server. */
export interface UserFormValues {
  displayName: string;
  role: Role;
  disabled: boolean;
  password: string;
}

/**
 * The PUT body with only the fields that changed and that the actor may change.
 * An empty object means nothing to send.
 */
export function updatePatch(user: User, values: UserFormValues, rights: UserRights): UserUpdate {
  const patch: UserUpdate = {};
  const displayName = values.displayName.trim();
  if (displayName !== (user.displayName ?? '')) {
    patch.displayName = displayName;
  }
  if (rights.canChangeRole && values.role !== user.role) {
    patch.role = values.role;
  }
  if (rights.canDisable && values.disabled !== user.disabled) {
    patch.disabled = values.disabled;
  }
  if (rights.canResetPassword && values.password !== '') {
    patch.password = values.password;
  }
  return patch;
}

/** Translation key for a failed user call. Field errors (422) are applied to the form separately. */
export function userErrorKey(err: unknown): string {
  if (!(err instanceof HttpErrorResponse)) {
    return 'users.error.unexpected';
  }
  switch (err.status) {
    case 0:
      return 'users.error.offline';
    case 403:
      return 'users.error.forbidden';
    case 404:
      return 'users.error.notFound';
    case 409:
      return 'users.error.conflict';
    case 422:
      return 'users.error.invalid';
    default:
      return 'users.error.unexpected';
  }
}
