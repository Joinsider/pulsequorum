// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { Component } from '@angular/core';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { authInterceptor } from './auth.interceptor';
import { Me } from '../../api/gen/model';
import { AuthService, displayNameFor, safeReturnUrl } from './auth.service';

@Component({ template: '' })
class Blank {}

const ME = {
  id: 'u1',
  username: 'alice',
  tenantId: 't1',
  roles: ['tenant-viewer'],
  authMode: 'local',
};

function token(name: string, inMs = 15 * 60_000) {
  return { accessToken: name, expiresAt: new Date(Date.now() + inMs).toISOString() };
}

describe('AuthService', () => {
  let auth: AuthService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideRouter([{ path: 'login', component: Blank }]),
        provideHttpClient(withInterceptors([authInterceptor])),
        provideHttpClientTesting(),
      ],
    });
    auth = TestBed.inject(AuthService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  async function flushMe() {
    await Promise.resolve();
    const me = http.expectOne('/api/v1/me');
    expect(me.request.headers.get('Authorization')).toMatch(/^Bearer /);
    me.flush(ME);
  }

  it('logs in, keeps the token in memory and loads the profile', async () => {
    const done = auth.login('alice', 'secret-secret');
    const req = http.expectOne('/api/v1/auth/login');
    expect(req.request.withCredentials).toBe(true);
    expect(req.request.headers.has('Authorization')).toBe(false);
    req.flush(token('a1'));
    await flushMe();
    await done;

    expect(auth.authenticated()).toBe(true);
    expect(auth.accessToken()).toBe('a1');
    expect(auth.me()?.username).toBe('alice');
    expect(auth.canEdit()).toBe(false);
  });

  it('sends the CSRF header and credentials on refresh', async () => {
    const done = auth.restore();
    const req = http.expectOne('/api/v1/auth/refresh');
    expect(req.request.headers.get('X-Requested-With')).toBe('uptimecluster');
    expect(req.request.withCredentials).toBe(true);
    req.flush(token('a1'));
    await flushMe();
    await done;
    expect(auth.status()).toBe('authenticated');
  });

  it('shares one refresh between concurrent callers (rotation must not be reused)', async () => {
    const a = auth.refresh();
    const b = auth.refresh();
    http.expectOne('/api/v1/auth/refresh').flush(token('a2'));
    await flushMe();
    expect(await a).toBe(true);
    expect(await b).toBe(true);
  });

  it('becomes anonymous when the refresh cookie is rejected', async () => {
    const done = auth.restore();
    http.expectOne('/api/v1/auth/refresh').flush({}, { status: 401, statusText: 'Unauthorized' });
    await done;
    expect(auth.status()).toBe('anonymous');
    expect(auth.accessToken()).toBeNull();
  });

  it('does not return an expired access token', async () => {
    const done = auth.login('alice', 'secret-secret');
    http.expectOne('/api/v1/auth/login').flush(token('old', -1000));
    await Promise.resolve();
    http.expectOne('/api/v1/me').flush(ME);
    await done.catch(() => undefined);
    expect(auth.accessToken()).toBeNull();
  });

  it('forgets the session on logout even if the server call fails', async () => {
    const login = auth.login('alice', 'secret-secret');
    http.expectOne('/api/v1/auth/login').flush(token('a1'));
    await flushMe();
    await login;

    const done = auth.logout();
    const req = http.expectOne('/api/v1/auth/logout');
    expect(req.request.headers.get('X-Requested-With')).toBe('uptimecluster');
    req.flush({}, { status: 500, statusText: 'Error' });
    await done;
    expect(auth.authenticated()).toBe(false);
    expect(auth.accessToken()).toBeNull();
    expect(auth.me()).toBeNull();
  });
});

describe('safeReturnUrl', () => {
  it.each([
    ['/monitors/1', '/monitors/1'],
    ['/incidents?x=1', '/incidents?x=1'],
    ['//evil.example', '/'],
    ['/\\evil.example', '/'],
    ['https://evil.example', '/'],
    ['javascript:alert(1)', '/'],
    ['', '/'],
    [undefined, '/'],
    ['/login', '/'],
    ['/setup?x', '/'],
  ])('%s -> %s', (input, want) => {
    expect(safeReturnUrl(input)).toBe(want);
  });
});

describe('displayNameFor', () => {
  const profile: Me = {
    id: 'u1',
    username: 'alice',
    displayName: 'Alice L.',
    tenantId: 't1',
    roles: ['tenant-viewer'],
    authMode: 'oidc',
  };
  const noDisplayName: Me = { ...profile, displayName: undefined };

  it.each([
    ['local', { name: 'Claim Name' }, profile, 'Alice L.'],
    ['local', null, noDisplayName, 'alice'],
    [
      'oidc',
      { name: 'Full Name', preferred_username: 'p', email: 'e@x.test' },
      profile,
      'Full Name',
    ],
    ['oidc', { preferred_username: 'p', email: 'e@x.test' }, profile, 'p'],
    ['oidc', { email: 'e@x.test' }, profile, 'e@x.test'],
    ['oidc', { name: ' ', preferred_username: '', email: ' ' }, profile, 'Alice L.'],
    ['oidc', { name: 7 }, profile, 'Alice L.'],
    ['oidc', { sub: '8f0c2e6a-1111' }, noDisplayName, 'alice'],
    ['oidc', null, profile, 'Alice L.'],
    ['oidc', null, null, ''],
  ])('%s, claims %j -> %s', (mode, claims, me, want) => {
    expect(displayNameFor(mode, claims, me)).toBe(want);
  });
});
