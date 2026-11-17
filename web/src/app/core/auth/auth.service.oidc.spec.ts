// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  HttpClient,
  HttpErrorResponse,
  provideHttpClient,
  withInterceptors,
} from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { Component, signal } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { Router, provideRouter } from '@angular/router';
import {
  AbstractSecurityStorage,
  EventTypes,
  LoginResponse,
  OidcSecurityService,
  PublicEventsService,
} from 'angular-auth-oidc-client';
import { Subject, firstValueFrom, of } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfigService } from '../config/config.service';
import { authInterceptor } from './auth.interceptor';
import { AuthService, jwtExpiryMs } from './auth.service';
import { OIDC_CONFIG_ID } from './oidc-config';
import { SplitSecurityStorage } from './split-security-storage';

@Component({ template: '' })
class Blank {}

const ME = {
  id: 'u1',
  username: 'alice',
  tenantId: 't1',
  roles: ['tenant-viewer'],
  authMode: 'oidc',
};

/** Unsigned JWT with the given claims; the SPA reads `exp` only. */
function jwt(claims: Record<string, unknown>): string {
  const b64 = (o: object) =>
    btoa(JSON.stringify(o)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  return `${b64({ alg: 'RS256', kid: 'k1' })}.${b64(claims)}.signature`;
}
const inMinutes = (m: number) => Math.floor(Date.now() / 1000) + m * 60;
const SUBJECT = 'd3c1f0a2-5b7e-4c59-9a7e-0f2c4b8e6a10';
const ACCESS = jwt({ sub: 'u1', exp: inMinutes(15) });
const RENEWED = jwt({ sub: 'u1', exp: inMinutes(30) });

const NOT_AUTHENTICATED: LoginResponse = {
  isAuthenticated: false,
  accessToken: '',
  idToken: '',
  userData: undefined,
  errorMessage: 'no code in url',
};
const AUTHENTICATED: LoginResponse = {
  isAuthenticated: true,
  accessToken: ACCESS,
  idToken: 'id-token',
  userData: { sub: 'u1' },
  configId: OIDC_CONFIG_ID,
};

function mockOidc() {
  return {
    preloadAuthWellKnownDocument: vi.fn(() => of({})),
    checkAuth: vi.fn(() => of(NOT_AUTHENTICATED)),
    authorize: vi.fn(),
    logoff: vi.fn(() => of(undefined)),
    logoffLocal: vi.fn(),
    getAccessToken: vi.fn(() => of('')),
    forceRefreshSession: vi.fn(() => of(NOT_AUTHENTICATED)),
  };
}

/** Everything the browser storages hold, as one string. */
function persisted(): string {
  const out: string[] = [];
  for (const s of [localStorage, sessionStorage]) {
    for (let i = 0; i < s.length; i++) {
      const k = s.key(i)!;
      out.push(k, s.getItem(k) ?? '');
    }
  }
  return out.join('\n');
}

describe('AuthService in OIDC mode', () => {
  let oidc: ReturnType<typeof mockOidc>;
  let events: Subject<{ type: EventTypes }>;

  /** Builds a fresh "page load": new services, same browser storage. */
  function setup(mode: 'local' | 'oidc' = 'oidc') {
    TestBed.resetTestingModule();
    oidc = mockOidc();
    events = new Subject();
    TestBed.configureTestingModule({
      providers: [
        provideRouter([
          { path: 'login', component: Blank },
          { path: 'monitors/1', component: Blank },
        ]),
        provideHttpClient(withInterceptors([authInterceptor])),
        provideHttpClientTesting(),
        {
          provide: ConfigService,
          useValue: {
            config: signal({
              nodes: [],
              auth: {
                mode,
                issuer: 'https://id.example.test',
                clientId: 'uptime',
                scopes: ['openid', 'profile', 'offline_access'],
              },
            }),
            apiBaseUrl: () => '',
          },
        },
        { provide: OidcSecurityService, useValue: oidc },
        {
          provide: PublicEventsService,
          useValue: { registerForEvents: () => events.asObservable() },
        },
        { provide: AbstractSecurityStorage, useClass: SplitSecurityStorage },
      ],
    });
    return {
      auth: TestBed.inject(AuthService),
      http: TestBed.inject(HttpTestingController),
      router: TestBed.inject(Router),
      storage: TestBed.inject(AbstractSecurityStorage) as SplitSecurityStorage,
    };
  }

  /** Answers the profile call that follows a successful sign-in. */
  async function flushMe(http: HttpTestingController) {
    const req = await vi.waitFor(() => http.expectOne('/api/v1/me'), { timeout: 500 });
    expect(req.request.headers.get('Authorization')).toBe(`Bearer ${ACCESS}`);
    req.flush(ME);
  }

  /** Restores a session from a successful identity provider callback. */
  async function signIn(auth: AuthService, http: HttpTestingController) {
    oidc.checkAuth.mockReturnValue(of(AUTHENTICATED));
    const done = auth.restore();
    await flushMe(http);
    await done;
  }

  beforeEach(() => {
    sessionStorage.clear();
    localStorage.clear();
  });

  afterEach(() => {
    sessionStorage.clear();
    localStorage.clear();
    history.replaceState(null, '', '/');
  });

  it('caches the token, loads the profile and clears the redirect state after the callback', async () => {
    const { auth, http, storage } = setup();
    oidc.checkAuth.mockReturnValue(of(AUTHENTICATED));
    const clear = vi.spyOn(storage, 'clearRedirectState');

    const done = auth.restore();
    await flushMe(http);
    await done;

    expect(auth.status()).toBe('authenticated');
    expect(auth.accessToken()).toBe(ACCESS);
    expect(auth.me()?.username).toBe('alice');
    expect(clear).toHaveBeenCalledWith(OIDC_CONFIG_ID);
    expect(oidc.preloadAuthWellKnownDocument.mock.invocationCallOrder[0]).toBeLessThan(
      oidc.checkAuth.mock.invocationCallOrder[0],
    );
  });

  it('keeps the access token out of localStorage and sessionStorage (NFA-SEC-04)', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);

    const stored = persisted();
    expect(stored).not.toContain(ACCESS);
    expect(stored).not.toContain('id-token');
  });

  it('treats a callback with error=login_required as anonymous, without another authorize', async () => {
    history.replaceState(null, '', '/auth/callback?error=login_required&state=abc');
    const { auth, http } = setup();

    await auth.restore();

    expect(auth.status()).toBe('anonymous');
    expect(oidc.checkAuth).not.toHaveBeenCalled();
    expect(oidc.authorize).not.toHaveBeenCalled();
    expect(location.search).toBe('');
    http.verify();
  });

  it('starts one silent login per tab and never a second one', async () => {
    const first = setup();
    await first.auth.restore();

    expect(first.auth.status()).toBe('anonymous');
    expect(oidc.authorize).toHaveBeenCalledTimes(1);
    expect(oidc.authorize).toHaveBeenCalledWith(OIDC_CONFIG_ID, {
      customParams: { prompt: 'none' },
    });
    expect(sessionStorage.getItem('uc.oidc-silent-tried')).toBe('1');

    // Next page load in the same tab: the flag from sessionStorage holds it back.
    const second = setup();
    await second.auth.restore();
    expect(second.auth.status()).toBe('anonymous');
    expect(oidc.authorize).not.toHaveBeenCalled();
  });

  it('re-arms the silent login after a successful sign-in, so every reload can restore the session', async () => {
    sessionStorage.setItem('uc.oidc-silent-tried', '1');
    const { auth, http } = setup();
    await signIn(auth, http);
    expect(sessionStorage.getItem('uc.oidc-silent-tried')).toBeNull();

    // Reload: tokens were in memory only, so the next load tries prompt=none again.
    const next = setup();
    await next.auth.restore();
    expect(oidc.authorize).toHaveBeenCalledWith(OIDC_CONFIG_ID, {
      customParams: { prompt: 'none' },
    });
  });

  it('does not start a silent login on the callback page itself', async () => {
    history.replaceState(null, '', '/auth/callback');
    const { auth } = setup();

    await auth.restore();

    expect(auth.status()).toBe('anonymous');
    expect(oidc.authorize).not.toHaveBeenCalled();
  });

  it('login stores the return URL, clears the silent flag and redirects', async () => {
    const { auth } = setup();
    sessionStorage.setItem('uc.oidc-silent-tried', '1');

    await auth.login('/monitors/1');

    expect(sessionStorage.getItem('uc.oidc-silent-tried')).toBeNull();
    expect(sessionStorage.getItem('uc.oidc-return')).toBe('/monitors/1');
    expect(oidc.authorize).toHaveBeenCalledWith(OIDC_CONFIG_ID);
  });

  it('login does not store an unsafe return URL', async () => {
    const { auth } = setup();

    await auth.login('https://evil.example/steal');

    expect(sessionStorage.getItem('uc.oidc-return')).toBe('/');
  });

  it('takeReturnUrl returns the stored path once and then falls back to /', () => {
    const { auth } = setup();
    sessionStorage.setItem('uc.oidc-return', '/monitors/1');

    expect(auth.takeReturnUrl()).toBe('/monitors/1');
    expect(auth.takeReturnUrl()).toBe('/');
  });

  it('logout clears the local session and calls the provider logoff', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);

    await auth.logout();

    expect(oidc.logoff).toHaveBeenCalledWith(OIDC_CONFIG_ID);
    expect(auth.status()).toBe('anonymous');
    expect(auth.accessToken()).toBeNull();
    expect(auth.me()).toBeNull();
    expect(sessionStorage.getItem('uc.oidc-silent-tried')).toBe('1');
  });

  it('sessionExpired calls logoffLocal and sends the user to the login with the expired notice', async () => {
    const { auth, router } = setup();
    const navigate = vi.spyOn(router, 'navigate');

    await auth.sessionExpired();

    expect(oidc.logoffLocal).toHaveBeenCalledWith(OIDC_CONFIG_ID);
    expect(navigate).toHaveBeenCalledWith(
      ['/login'],
      expect.objectContaining({ queryParams: expect.objectContaining({ expired: 1 }) }),
    );
  });

  it('ends the session when the provider reports a failed silent renew', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);

    events.next({ type: EventTypes.SilentRenewFailed });

    await vi.waitFor(() => expect(oidc.logoffLocal).toHaveBeenCalledWith(OIDC_CONFIG_ID));
  });

  it('caches the renewed token on NewAuthenticationResult', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);
    oidc.getAccessToken.mockReturnValue(of(RENEWED));

    events.next({ type: EventTypes.NewAuthenticationResult });

    await vi.waitFor(() => expect(auth.accessToken()).toBe(RENEWED));
  });

  it('refresh uses the library refresh and never the backend refresh endpoint', async () => {
    const { auth, http } = setup();
    oidc.forceRefreshSession.mockReturnValue(of({ ...AUTHENTICATED, accessToken: RENEWED }));

    expect(await auth.refresh()).toBe(true);

    expect(auth.accessToken()).toBe(RENEWED);
    http.expectNone('/api/v1/auth/refresh');
  });

  it('refresh fails and ends the local session when the provider refuses', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);
    oidc.forceRefreshSession.mockReturnValue(of(NOT_AUTHENTICATED));

    expect(await auth.refresh()).toBe(false);

    expect(auth.status()).toBe('anonymous');
    expect(auth.accessToken()).toBeNull();
  });

  it('keeps the access token out of browser storage after a refresh as well', async () => {
    const { auth, http } = setup();
    await signIn(auth, http);
    oidc.forceRefreshSession.mockReturnValue(of({ ...AUTHENTICATED, accessToken: RENEWED }));

    await auth.refresh();

    expect(persisted()).not.toContain(RENEWED);
    expect(persisted()).not.toContain(ACCESS);
  });

  /** Signs in with an ID token whose claims are `claims` (plus the subject). */
  async function signInWithClaims(claims: Record<string, unknown>): Promise<AuthService> {
    const { auth, http } = setup();
    oidc.checkAuth.mockReturnValue(of({ ...AUTHENTICATED, userData: { sub: SUBJECT, ...claims } }));
    const done = auth.restore();
    await flushMe(http);
    await done;
    return auth;
  }

  it.each([
    [
      'name',
      { name: 'Alice Example', preferred_username: 'alice', email: 'a@example.test' },
      'Alice Example',
    ],
    ['preferred_username', { preferred_username: 'alice', email: 'a@example.test' }, 'alice'],
    ['email', { email: 'a@example.test' }, 'a@example.test'],
    ['name, trimmed', { name: '  Alice Example ' }, 'Alice Example'],
    [
      'blank claims skipped',
      { name: ' ', preferred_username: '', email: 'a@example.test' },
      'a@example.test',
    ],
    ['non-string claims skipped', { name: 42, preferred_username: 'alice' }, 'alice'],
  ])('shows the ID token claim by preference: %s', async (_case, claims, want) => {
    const auth = await signInWithClaims(claims);
    expect(auth.displayName()).toBe(want);
  });

  it('never shows the subject ID when no name claim exists; falls back to the profile', async () => {
    const auth = await signInWithClaims({});
    expect(auth.displayName()).toBe(ME.username);
    expect(auth.displayName()).not.toBe(SUBJECT);
  });

  it('forgets the ID token claims on logout', async () => {
    const auth = await signInWithClaims({ name: 'Alice Example' });
    expect(auth.displayName()).toBe('Alice Example');

    await auth.logout();

    expect(auth.displayName()).toBe('');
  });

  it('keeps the ID token claims out of browser storage', async () => {
    await signInWithClaims({ name: 'Alice Example', email: 'a@example.test' });
    expect(persisted()).not.toContain('Alice Example');
    expect(persisted()).not.toContain('a@example.test');
  });
});

describe('authInterceptor in OIDC mode', () => {
  let oidc: ReturnType<typeof mockOidc>;
  let events: Subject<{ type: EventTypes }>;
  let http: HttpClient;
  let ctl: HttpTestingController;
  let auth: AuthService;

  beforeEach(async () => {
    sessionStorage.clear();
    oidc = mockOidc();
    events = new Subject();
    TestBed.configureTestingModule({
      providers: [
        provideRouter([{ path: 'login', component: Blank }]),
        provideHttpClient(withInterceptors([authInterceptor])),
        provideHttpClientTesting(),
        {
          provide: ConfigService,
          useValue: {
            config: signal({ nodes: [], auth: { mode: 'oidc' } }),
            apiBaseUrl: () => '',
          },
        },
        { provide: OidcSecurityService, useValue: oidc },
        {
          provide: PublicEventsService,
          useValue: { registerForEvents: () => events.asObservable() },
        },
        { provide: AbstractSecurityStorage, useClass: SplitSecurityStorage },
      ],
    });
    http = TestBed.inject(HttpClient);
    ctl = TestBed.inject(HttpTestingController);
    auth = TestBed.inject(AuthService);

    oidc.checkAuth.mockReturnValue(of(AUTHENTICATED));
    const done = auth.restore();
    const me = await vi.waitFor(() => ctl.expectOne('/api/v1/me'), { timeout: 500 });
    me.flush(ME);
    await done;
  });

  afterEach(() => {
    ctl.verify();
    sessionStorage.clear();
  });

  it('refreshes once on 401 and retries with the renewed token', async () => {
    oidc.forceRefreshSession.mockReturnValue(of({ ...AUTHENTICATED, accessToken: RENEWED }));
    const res = firstValueFrom(http.get<{ ok: boolean }>('/api/v1/monitors'));
    ctl.expectOne('/api/v1/monitors').flush({}, { status: 401, statusText: 'Unauthorized' });

    const retry = await vi.waitFor(() => ctl.expectOne('/api/v1/monitors'), { timeout: 500 });
    expect(retry.request.headers.get('Authorization')).toBe(`Bearer ${RENEWED}`);
    retry.flush({ ok: true });

    expect(await res).toEqual({ ok: true });
    expect(oidc.forceRefreshSession).toHaveBeenCalledTimes(1);
  });

  it('ends the session when the retried call is rejected as well', async () => {
    oidc.forceRefreshSession.mockReturnValue(of({ ...AUTHENTICATED, accessToken: RENEWED }));
    const res = firstValueFrom(http.get('/api/v1/monitors')).catch((e: unknown) => e);
    ctl.expectOne('/api/v1/monitors').flush({}, { status: 401, statusText: 'Unauthorized' });

    const retry = await vi.waitFor(() => ctl.expectOne('/api/v1/monitors'), { timeout: 500 });
    retry.flush({}, { status: 401, statusText: 'Unauthorized' });

    expect(await res).toBeInstanceOf(HttpErrorResponse);
    await vi.waitFor(() => expect(oidc.logoffLocal).toHaveBeenCalledWith(OIDC_CONFIG_ID));
    expect(auth.authenticated()).toBe(false);
  });

  it('passes 403 through unchanged without a refresh', async () => {
    const res = firstValueFrom(http.get('/api/v1/monitors')).catch((e: unknown) => e);
    ctl.expectOne('/api/v1/monitors').flush({}, { status: 403, statusText: 'Forbidden' });

    const error = await res;
    expect(error).toBeInstanceOf(HttpErrorResponse);
    expect((error as HttpErrorResponse).status).toBe(403);
    expect(oidc.forceRefreshSession).not.toHaveBeenCalled();
  });
});

describe('jwtExpiryMs', () => {
  it.each([
    ['a valid exp', jwt({ exp: 1_000 }), 1_000_000],
    ['no exp claim', jwt({ sub: 'u1' }), 0],
    ['not a JWT', 'opaque-token', 0],
    ['undecodable payload', 'a.!!!.c', 0],
    ['non-JSON payload', `a.${btoa('not json')}.c`, 0],
  ])('%s', (_name, token, want) => {
    expect(jwtExpiryMs(token)).toBe(want);
  });
});
