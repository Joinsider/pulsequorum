// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AuthService } from '../../core/auth/auth.service';
import { LoginPage } from './login-page';

describe('LoginPage', () => {
  let fixture: ComponentFixture<LoginPage>;
  let login: ReturnType<typeof vi.fn>;

  function setup(mode: 'local' | 'oidc') {
    login = vi.fn().mockResolvedValue(undefined);
    TestBed.configureTestingModule({
      imports: [
        LoginPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideRouter([]),
        { provide: AuthService, useValue: { mode: signal(mode).asReadonly(), login } },
      ],
    });
    fixture = TestBed.createComponent(LoginPage);
    fixture.componentRef.setInput('returnUrl', '/monitors/1');
    fixture.detectChanges();
  }

  function el(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  afterEach(() => TestBed.resetTestingModule());

  it('renders only the sign-in button in OIDC mode', () => {
    setup('oidc');

    expect(el().querySelectorAll('input')).toHaveLength(0);
    expect(el().querySelector('a[href="/setup"]')).toBeNull();
    const buttons = el().querySelectorAll('button');
    expect(buttons).toHaveLength(1);
    expect(buttons[0].textContent?.trim()).toBe('auth.login.submit');
  });

  it('starts the identity provider login with the return URL on click', () => {
    setup('oidc');

    el().querySelector('button')!.click();

    expect(login).toHaveBeenCalledWith('/monitors/1');
  });

  it('keeps the password form in local mode', () => {
    setup('local');

    expect(el().querySelectorAll('input')).toHaveLength(2);
    expect(el().querySelector('a[href="/setup"]')).not.toBeNull();
  });
});
