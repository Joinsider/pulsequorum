// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, describe, expect, it } from 'vitest';
import { Me } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { AccountPage } from './account-page';

const LOCAL: Me = {
  id: 'u1',
  username: 'alice',
  displayName: 'Alice',
  tenantId: 't1',
  tenantName: 'Acme',
  roles: ['tenant-admin'],
  authMode: 'local',
};

const CURRENT = 'old-password-123';
const NEW = 'new-password-456';

describe('AccountPage', () => {
  let fixture: ComponentFixture<AccountPage>;
  let http: HttpTestingController;

  function setup(
    profile: Me,
    mode: 'local' | 'oidc' = 'local',
    displayName = profile.displayName ?? profile.username,
  ) {
    TestBed.configureTestingModule({
      imports: [
        AccountPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: AuthService,
          useValue: {
            me: signal(profile).asReadonly(),
            mode: signal(mode).asReadonly(),
            displayName: signal(displayName).asReadonly(),
          },
        },
      ],
    });
    fixture = TestBed.createComponent(AccountPage);
    http = TestBed.inject(HttpTestingController);
    fixture.detectChanges();
  }

  function text(): string {
    return (fixture.nativeElement as HTMLElement).textContent ?? '';
  }

  function type(selector: string, value: string) {
    const input = (fixture.nativeElement as HTMLElement).querySelector<HTMLInputElement>(selector);
    if (!input) {
      throw new Error(`no input ${selector}`);
    }
    input.value = value;
    input.dispatchEvent(new Event('input'));
  }

  function fillForm(repeat = NEW) {
    type('input[formcontrolname="currentPassword"]', CURRENT);
    type('input[formcontrolname="newPassword"]', NEW);
    type('input[formcontrolname="repeatPassword"]', repeat);
    fixture.detectChanges();
  }

  function submit() {
    const form = (fixture.nativeElement as HTMLElement).querySelector('form');
    form?.dispatchEvent(new Event('submit'));
    fixture.detectChanges();
  }

  afterEach(() => http.verify());

  it('shows the username in local mode', () => {
    setup(LOCAL);
    expect(text()).toContain('alice');
  });

  it('shows the identity provider name in OIDC mode, not the subject ID', () => {
    setup({ ...LOCAL, username: 'd3c1f0a2-5b7e-4c59', authMode: 'oidc' }, 'oidc', 'Alice Example');
    expect(text()).toContain('Alice Example');
    expect(text()).not.toContain('d3c1f0a2-5b7e-4c59');
  });

  it('hides the password form in OIDC mode, even for a local account', () => {
    setup(LOCAL, 'oidc');
    expect(text()).toContain('account.password.external');
    expect(
      (fixture.nativeElement as HTMLElement).querySelector(
        'input[formcontrolname="currentPassword"]',
      ),
    ).toBeNull();
  });

  it('hides the password form for an identity provider account', () => {
    setup({ ...LOCAL, authMode: 'oidc' });

    expect(
      (fixture.nativeElement as HTMLElement).querySelector('input[type="password"]'),
    ).toBeNull();
    expect(text()).toContain('account.password.external');
  });

  it('shows the profile and the password form for a local account', () => {
    setup(LOCAL);

    expect(text()).toContain('alice');
    expect(text()).toContain('Acme');
    expect(text()).toContain('account.roles.tenant-admin');
    expect(
      (fixture.nativeElement as HTMLElement).querySelectorAll('input[type="password"]'),
    ).toHaveLength(3);
  });

  it('does not send a request when the repetition does not match', () => {
    setup(LOCAL);
    fillForm('different-789');
    submit();

    expect(text()).toContain('account.password.mismatch');
  });

  it('changes the password with credentials and resets the form on success', async () => {
    setup(LOCAL);
    fillForm();
    submit();

    const req = http.expectOne({ method: 'PUT', url: '/api/v1/me/password' });
    expect(req.request.withCredentials).toBe(true);
    expect(req.request.body).toEqual({ currentPassword: CURRENT, newPassword: NEW });
    req.flush(null, { status: 204, statusText: 'No Content' });
    await fixture.whenStable();
    fixture.detectChanges();

    expect(text()).toContain('account.password.changed');
    expect(
      (fixture.nativeElement as HTMLElement).querySelector<HTMLInputElement>(
        'input[formcontrolname="newPassword"]',
      )?.value,
    ).toBe('');
  });

  it('shows the translated message for a wrong current password', async () => {
    setup(LOCAL);
    fillForm();
    submit();

    http
      .expectOne({ method: 'PUT', url: '/api/v1/me/password' })
      .flush(
        { title: 'Validation Failed', status: 422, detail: 'current password is wrong' },
        { status: 422, statusText: 'Unprocessable Entity' },
      );
    await fixture.whenStable();
    fixture.detectChanges();

    expect(text()).toContain('account.password.error.currentWrong');
  });
});
