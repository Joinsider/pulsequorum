// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpClient, provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { Router, provideRouter } from '@angular/router';
import { firstValueFrom } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiBaseUrlInterceptor } from '../api/api-base-url.interceptor';
import { ConfigService } from '../config/config.service';
import { authInterceptor } from './auth.interceptor';
import { AuthService } from './auth.service';

const ME = {
  id: 'u1',
  username: 'alice',
  tenantId: 't1',
  roles: ['tenant-admin'],
  authMode: 'local',
};
const later = () => new Date(Date.now() + 15 * 60_000).toISOString();

describe('authInterceptor', () => {
  let http: HttpClient;
  let ctl: HttpTestingController;
  let auth: AuthService;

  beforeEach(async () => {
    TestBed.configureTestingModule({
      providers: [
        provideRouter([]),
        provideHttpClient(withInterceptors([authInterceptor, apiBaseUrlInterceptor])),
        provideHttpClientTesting(),
        { provide: ConfigService, useValue: { apiBaseUrl: () => 'https://node-a.example' } },
      ],
    });
    http = TestBed.inject(HttpClient);
    ctl = TestBed.inject(HttpTestingController);
    auth = TestBed.inject(AuthService);

    const login = auth.login('alice', 'pw');
    ctl
      .expectOne('https://node-a.example/api/v1/auth/login')
      .flush({ accessToken: 'a1', expiresAt: later() });
    await Promise.resolve();
    ctl.expectOne('https://node-a.example/api/v1/me').flush(ME);
    await login;
  });

  afterEach(() => ctl.verify());

  it('adds the bearer token to own API calls', async () => {
    const res = firstValueFrom(http.get('/api/v1/monitors'));
    const req = ctl.expectOne('https://node-a.example/api/v1/monitors');
    expect(req.request.headers.get('Authorization')).toBe('Bearer a1');
    req.flush({});
    await res;
  });

  it('never sends the token to other hosts or non-API paths', async () => {
    const a = firstValueFrom(http.get('https://evil.example/api/v1/monitors'));
    const b = firstValueFrom(http.get('/i18n/en.json'));
    const ra = ctl.expectOne('https://evil.example/api/v1/monitors');
    const rb = ctl.expectOne('/i18n/en.json');
    expect(ra.request.headers.has('Authorization')).toBe(false);
    expect(rb.request.headers.has('Authorization')).toBe(false);
    ra.flush({});
    rb.flush({});
    await Promise.all([a, b]);
  });

  it('refreshes once on 401 and retries with the new token', async () => {
    const res = firstValueFrom(http.get('/api/v1/monitors'));
    ctl
      .expectOne('https://node-a.example/api/v1/monitors')
      .flush({}, { status: 401, statusText: 'Unauthorized' });
    await Promise.resolve();
    ctl
      .expectOne('https://node-a.example/api/v1/auth/refresh')
      .flush({ accessToken: 'a2', expiresAt: later() });
    await vi
      .waitFor(() => ctl.expectOne('https://node-a.example/api/v1/monitors'), { timeout: 500 })
      .then((retry) => {
        expect(retry.request.headers.get('Authorization')).toBe('Bearer a2');
        retry.flush({ ok: true });
      });
    expect(await res).toEqual({ ok: true });
  });

  it('does not retry a second time and ends the session if refresh fails', async () => {
    const nav = vi.spyOn(TestBed.inject(Router), 'navigate').mockResolvedValue(true);
    const res = firstValueFrom(http.get('/api/v1/monitors'));
    ctl
      .expectOne('https://node-a.example/api/v1/monitors')
      .flush({}, { status: 401, statusText: 'Unauthorized' });
    await Promise.resolve();
    ctl
      .expectOne('https://node-a.example/api/v1/auth/refresh')
      .flush({}, { status: 401, statusText: 'Unauthorized' });
    await expect(res).rejects.toBeTruthy();
    expect(auth.authenticated()).toBe(false);
    await vi.waitFor(() => expect(nav).toHaveBeenCalledWith(['/login'], expect.anything()));
  });
});
