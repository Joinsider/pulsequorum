// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import {
  AbstractSecurityStorage,
  EventTypes,
  OidcSecurityService,
  PublicEventsService,
} from 'angular-auth-oidc-client';
import { Subject, of } from 'rxjs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SystemService } from '../api/gen/endpoints/system/system.service';
import { Me, Role } from '../api/gen/model';
import { authInterceptor } from '../core/auth/auth.interceptor';
import { AuthService } from '../core/auth/auth.service';
import { OIDC_CONFIG_ID } from '../core/auth/oidc-config';
import { SplitSecurityStorage } from '../core/auth/split-security-storage';
import { ConfigService } from '../core/config/config.service';
import { LiveStreamService } from '../core/stream/live-stream.service';
import { Shell } from './shell';

const SUBJECT = 'd3c1f0a2-5b7e-4c59-9a7e-0f2c4b8e6a10';

function jwt(claims: Record<string, unknown>): string {
  const b64 = (o: object) =>
    btoa(JSON.stringify(o)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  return `${b64({ alg: 'RS256' })}.${b64(claims)}.signature`;
}
const ACCESS = jwt({ sub: SUBJECT, exp: Math.floor(Date.now() / 1000) + 15 * 60 });

function profile(mode: 'local' | 'oidc', roles: Role[]): Me {
  return {
    id: 'u1',
    username: mode === 'oidc' ? SUBJECT : 'alice',
    tenantId: 't1',
    roles,
    authMode: mode,
  };
}

describe('Shell', () => {
  let fixture: ComponentFixture<Shell>;
  let http: HttpTestingController;
  let auth: AuthService;
  let oidc: {
    preloadAuthWellKnownDocument: ReturnType<typeof vi.fn>;
    checkAuth: ReturnType<typeof vi.fn>;
    logoffLocal: ReturnType<typeof vi.fn>;
  };
  let events: Subject<{ type: EventTypes }>;

  function setup(mode: 'local' | 'oidc') {
    oidc = {
      preloadAuthWellKnownDocument: vi.fn(() => of({})),
      checkAuth: vi.fn(),
      logoffLocal: vi.fn(),
    };
    events = new Subject();
    TestBed.configureTestingModule({
      imports: [
        Shell,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideRouter([]),
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
                scopes: ['openid', 'profile'],
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
        { provide: SystemService, useValue: { getHealth: () => of({ version: '1.2.3' }) } },
        { provide: LiveStreamService, useValue: { state: signal('open').asReadonly() } },
      ],
    });
    auth = TestBed.inject(AuthService);
    http = TestBed.inject(HttpTestingController);
  }

  async function signInLocal(roles: Role[]) {
    const done = auth.login('alice', 'secret-secret');
    http
      .expectOne('/api/v1/auth/login')
      .flush({ accessToken: ACCESS, expiresAt: new Date(Date.now() + 900_000).toISOString() });
    const me = await vi.waitFor(() => http.expectOne('/api/v1/me'), { timeout: 500 });
    me.flush(profile('local', roles));
    await done;
  }

  async function signInOidc(roles: Role[], userData: Record<string, unknown>) {
    oidc.checkAuth.mockReturnValue(
      of({
        isAuthenticated: true,
        accessToken: ACCESS,
        idToken: 'id',
        userData,
        configId: OIDC_CONFIG_ID,
      }),
    );
    const done = auth.restore();
    const me = await vi.waitFor(() => http.expectOne('/api/v1/me'), { timeout: 500 });
    me.flush(profile('oidc', roles));
    await done;
  }

  function render(): HTMLElement {
    fixture = TestBed.createComponent(Shell);
    fixture.detectChanges();
    return fixture.nativeElement as HTMLElement;
  }

  function headerName(el: HTMLElement): string {
    return el.querySelector('.user-name')?.textContent?.trim() ?? '';
  }

  function hasNavLink(el: HTMLElement, href: string): boolean {
    return el.querySelector(`.nav a[href="${href}"]`) !== null;
  }

  afterEach(() => {
    http?.verify();
    TestBed.resetTestingModule();
  });

  it('shows the username in the header in local mode', async () => {
    setup('local');
    await signInLocal(['tenant-viewer']);
    fixture = TestBed.createComponent(Shell);
    fixture.detectChanges();
    expect(headerName(fixture.nativeElement)).toBe('alice');
  });

  it('shows the ID token name in the header in OIDC mode, never the subject', async () => {
    setup('oidc');
    await signInOidc(['tenant-viewer'], { sub: SUBJECT, name: 'Alice Example' });
    const el = render();
    expect(headerName(el)).toBe('Alice Example');
    expect(el.textContent).not.toContain(SUBJECT);
  });

  it('lists the Users entry for an admin in local mode', async () => {
    setup('local');
    await signInLocal(['tenant-admin']);
    expect(hasNavLink(render(), '/users')).toBe(true);
  });

  it('hides the Users entry for an admin in OIDC mode', async () => {
    setup('oidc');
    await signInOidc(['tenant-admin'], { sub: SUBJECT, name: 'Alice Example' });
    const el = render();
    expect(hasNavLink(el, '/users')).toBe(false);
    expect(hasNavLink(el, '/incidents')).toBe(true);
  });
});
