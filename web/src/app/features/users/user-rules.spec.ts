// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { describe, expect, it } from 'vitest';
import { Me, User } from '../../api/gen/model';
import { assignableRoles, updatePatch, userErrorKey, userRights, UserRights } from './user-rules';

const TENANT_ADMIN: Me = {
  id: 'me',
  username: 'admin',
  tenantId: 't1',
  roles: ['tenant-admin'],
  authMode: 'local',
};
const PLATFORM_ADMIN: Me = { ...TENANT_ADMIN, roles: ['platform-admin'] };

function user(id: string, role: User['role'], disabled = false): User {
  return { id, username: id, role, disabled, createdAt: '2026-01-01T00:00:00Z' };
}

const ALL_ALLOWED: UserRights = {
  editable: true,
  canChangeRole: true,
  canDisable: true,
  canDelete: true,
  canResetPassword: true,
  lockKey: null,
};

describe('assignableRoles', () => {
  it('excludes platform-admin for tenant admins', () => {
    expect(assignableRoles(['tenant-admin'])).toEqual([
      'tenant-admin',
      'tenant-editor',
      'tenant-viewer',
    ]);
  });

  it('offers every role to platform admins', () => {
    expect(assignableRoles(['platform-admin'])).toContain('platform-admin');
    expect(assignableRoles(['platform-admin'])).toHaveLength(4);
  });
});

describe('userRights', () => {
  it('locks a platform admin row for a tenant admin', () => {
    const rights = userRights(user('p1', 'platform-admin'), TENANT_ADMIN, [
      user('p1', 'platform-admin'),
    ]);
    expect(rights).toEqual({
      editable: false,
      canChangeRole: false,
      canDisable: false,
      canDelete: false,
      canResetPassword: false,
      lockKey: 'users.lock.platformTarget',
    });
  });

  it('locks role, disable and delete for oneself but keeps the row editable', () => {
    const self = user('me', 'tenant-admin');
    const rights = userRights(self, TENANT_ADMIN, [self]);
    expect(rights).toMatchObject({
      editable: true,
      canChangeRole: false,
      canDisable: false,
      canDelete: false,
      canResetPassword: false,
      lockKey: 'users.lock.self',
    });
  });

  it('allows everything for another user of a lower role', () => {
    const other = user('bob', 'tenant-viewer');
    expect(userRights(other, TENANT_ADMIN, [other])).toEqual(ALL_ALLOWED);
  });

  it('locks the last enabled platform admin', () => {
    const last = user('p1', 'platform-admin');
    const rights = userRights(last, PLATFORM_ADMIN, [last, user('p2', 'platform-admin', true)]);
    expect(rights).toMatchObject({
      canChangeRole: false,
      canDisable: false,
      canDelete: false,
      canResetPassword: true,
      lockKey: 'users.lock.lastPlatformAdmin',
    });
  });

  it('does not lock a platform admin while another enabled one exists', () => {
    const a = user('p1', 'platform-admin');
    const b = user('p2', 'platform-admin');
    expect(userRights(a, PLATFORM_ADMIN, [a, b]).canDisable).toBe(true);
  });

  it('does not lock an already disabled platform admin', () => {
    const disabled = user('p1', 'platform-admin', true);
    expect(userRights(disabled, PLATFORM_ADMIN, [disabled]).canDisable).toBe(true);
  });
});

describe('updatePatch', () => {
  const bob = { ...user('bob', 'tenant-viewer'), displayName: 'Bob' };
  const unchanged = {
    displayName: 'Bob',
    role: 'tenant-viewer' as const,
    disabled: false,
    password: '',
  };

  it('sends nothing when nothing changed', () => {
    expect(updatePatch(bob, unchanged, ALL_ALLOWED)).toEqual({});
  });

  it('sends only the changed fields, trimmed', () => {
    const patch = updatePatch(
      bob,
      { ...unchanged, displayName: '  Robert ', role: 'tenant-editor', disabled: true },
      ALL_ALLOWED,
    );
    expect(patch).toEqual({ displayName: 'Robert', role: 'tenant-editor', disabled: true });
  });

  it('drops role and disable changes that the actor may not make', () => {
    const locked: UserRights = { ...ALL_ALLOWED, canChangeRole: false, canDisable: false };
    const patch = updatePatch(
      bob,
      { ...unchanged, role: 'tenant-admin', disabled: true, displayName: 'Robert' },
      locked,
    );
    expect(patch).toEqual({ displayName: 'Robert' });
  });

  it('sends a password reset only when allowed and filled in', () => {
    const values = { ...unchanged, password: 'new-password-123' };
    expect(updatePatch(bob, values, ALL_ALLOWED)).toEqual({ password: 'new-password-123' });
    expect(updatePatch(bob, values, { ...ALL_ALLOWED, canResetPassword: false })).toEqual({});
  });
});

describe('userErrorKey', () => {
  it.each([
    [0, 'users.error.offline'],
    [403, 'users.error.forbidden'],
    [404, 'users.error.notFound'],
    [409, 'users.error.conflict'],
    [422, 'users.error.invalid'],
    [500, 'users.error.unexpected'],
  ])('maps status %i', (status, key) => {
    expect(userErrorKey(new HttpErrorResponse({ status }))).toBe(key);
  });

  it('maps non-HTTP errors to the generic message', () => {
    expect(userErrorKey(new Error('boom'))).toBe('users.error.unexpected');
  });
});
