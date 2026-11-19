// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialogRef } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { Observable, of, throwError } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiTokensService } from '../../api/gen/endpoints/api-tokens/api-tokens.service';
import { ApiTokenCreated } from '../../api/gen/model';
import {
  ApiTokenDialog,
  API_TOKEN_LIFETIME_DEFAULT,
  apiTokenCreateErrorKey,
  tokenExpiry,
} from './api-token-dialog';

const CREATED: ApiTokenCreated = {
  id: 'tok_bbbbbbbbbbbbbbbb',
  name: 'ci',
  role: 'tenant-editor',
  createdAt: '2026-11-19T10:00:00Z',
  expiresAt: '2027-02-17T10:00:00Z',
  expired: false,
  createdBy: { kind: 'user', id: 'me', name: 'admin' },
  token: 'uct_secretvalue123',
};

describe('tokenExpiry', () => {
  it('returns the instant the given number of days after now, in UTC ISO format', () => {
    const now = new Date('2026-01-01T00:00:00Z');
    expect(tokenExpiry(now, 2)).toBe('2026-01-03T00:00:00.000Z');
    expect(tokenExpiry(now, 0)).toBe('2026-01-01T00:00:00.000Z');
  });

  it('crosses month and year boundaries', () => {
    expect(tokenExpiry(new Date('2026-12-31T12:00:00Z'), 1)).toBe('2027-01-01T12:00:00.000Z');
  });
});

describe('apiTokenCreateErrorKey', () => {
  it.each([
    [403, 'apiTokens.error.forbidden'],
    [409, 'apiTokens.error.conflict'],
    [422, 'apiTokens.error.invalid'],
    [503, 'apiTokens.error.unavailable'],
    [500, 'apiTokens.error.generic'],
    [undefined, 'apiTokens.error.generic'],
  ])('maps status %s', (status, key) => {
    expect(apiTokenCreateErrorKey(status)).toBe(key);
  });
});

describe('ApiTokenDialog', () => {
  let fixture: ComponentFixture<ApiTokenDialog>;
  let api: { createApiToken: ReturnType<typeof vi.fn> };
  let ref: { close: ReturnType<typeof vi.fn> };

  function setup(
    create: () => Observable<ApiTokenCreated> = () => of(CREATED),
  ): Record<string, any> {
    api = { createApiToken: vi.fn(create) };
    ref = { close: vi.fn() };
    TestBed.configureTestingModule({
      imports: [
        ApiTokenDialog,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        { provide: MatDialogRef, useValue: ref },
        { provide: ApiTokensService, useValue: api },
      ],
    });
    fixture = TestBed.createComponent(ApiTokenDialog);
    fixture.detectChanges();
    return fixture.componentInstance as unknown as Record<string, any>;
  }

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function secretElement(): HTMLTextAreaElement | null {
    return root().querySelector<HTMLTextAreaElement>('[data-testid=api-token-secret]');
  }

  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('creates a token with the trimmed name, the role and an expiry in the default lifetime', async () => {
    const before = Date.now();
    const p = setup();
    p['form'].controls.name.setValue('  ci  ');
    p['form'].controls.role.setValue('tenant-editor');

    await p['create']();

    expect(api.createApiToken).toHaveBeenCalledTimes(1);
    const [body] = api.createApiToken.mock.calls[0];
    expect(body.name).toBe('ci');
    expect(body.role).toBe('tenant-editor');
    const days = API_TOKEN_LIFETIME_DEFAULT * 24 * 60 * 60 * 1000;
    const expiry = Date.parse(body.expiresAt);
    expect(expiry).toBeGreaterThanOrEqual(before + days);
    expect(expiry).toBeLessThanOrEqual(Date.now() + days);
  });

  it('shows the secret once after a successful create', async () => {
    const p = setup();
    p['form'].controls.name.setValue('ci');

    await p['create']();
    fixture.detectChanges();

    expect(secretElement()?.value).toBe('uct_secretvalue123');
    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'apiTokens.dialog.onceOnly',
    );
    expect(p['form'].disabled).toBe(true);
  });

  it('shows no secret and an error when the server rejects the input', async () => {
    const p = setup(() =>
      throwError(
        () =>
          new HttpErrorResponse({
            status: 422,
            error: {
              type: 'about:blank',
              title: 'Unprocessable',
              status: 422,
              detail: 'name taken',
              errors: [{ field: '/name', message: 'already used' }],
            },
          }),
      ),
    );
    p['form'].controls.name.setValue('ci');

    await p['create']();
    fixture.detectChanges();

    expect(secretElement()).toBeNull();
    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'apiTokens.error.invalid',
    );
    expect(root().textContent).toContain('name taken');
    expect(p['form'].controls.name.errors).toEqual({ server: 'already used' });
    expect(p['form'].enabled).toBe(true);
  });

  it('does not send a request when the name is empty', async () => {
    const p = setup();

    await p['create']();

    expect(api.createApiToken).not.toHaveBeenCalled();
    expect(secretElement()).toBeNull();
  });

  it('closes with true once a token was issued', async () => {
    const p = setup();
    p['form'].controls.name.setValue('ci');
    await p['create']();

    p['close']();

    expect(ref.close).toHaveBeenCalledWith(true);
  });

  it('closes with false when cancelled before anything was created', () => {
    const p = setup();

    p['close']();

    expect(ref.close).toHaveBeenCalledWith(false);
  });
});
