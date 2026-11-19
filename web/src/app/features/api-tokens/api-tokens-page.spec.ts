// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiToken } from '../../api/gen/model';
import { ApiTokensPage } from './api-tokens-page';

const TOKENS: ApiToken[] = [
  {
    id: 'tok_aaaaaaaaaaaaaaaa',
    name: 'ci-old',
    role: 'tenant-viewer',
    createdAt: '2026-01-01T00:00:00Z',
    expiresAt: '2026-06-01T00:00:00Z',
    expired: true,
    createdBy: { kind: 'user', id: 'me', name: 'admin' },
  },
  {
    id: 'tok_bbbbbbbbbbbbbbbb',
    name: 'ci',
    role: 'tenant-editor',
    createdAt: '2026-03-01T00:00:00Z',
    expiresAt: '2027-03-01T00:00:00Z',
    expired: false,
    createdBy: { kind: 'user', id: 'me', name: 'admin' },
  },
];

describe('ApiTokensPage', () => {
  let fixture: ComponentFixture<ApiTokensPage>;
  let http: HttpTestingController;
  let dialog: { open: ReturnType<typeof vi.fn> };
  let snack: { open: ReturnType<typeof vi.fn> };

  beforeEach(() => {
    dialog = { open: vi.fn(() => ({ afterClosed: () => of(true) })) };
    snack = { open: vi.fn() };
    TestBed.configureTestingModule({
      imports: [
        ApiTokensPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        { provide: MatDialog, useValue: dialog },
        { provide: MatSnackBar, useValue: snack },
      ],
    });
    fixture = TestBed.createComponent(ApiTokensPage);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function button(label: string): HTMLButtonElement | null {
    return root().querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
  }

  async function loadWith(tokens: ApiToken[]) {
    fixture.detectChanges();
    http.expectOne({ method: 'GET', url: '/api/v1/api-tokens' }).flush(tokens);
    await fixture.whenStable();
    fixture.detectChanges();
  }

  it('lists the active tokens first and marks expired ones', async () => {
    await loadWith(TOKENS);

    const names = [...root().querySelectorAll('.row__name')].map((el) => el.textContent?.trim());
    expect(names).toEqual(['ci', 'ci-old']);
    expect(root().querySelectorAll('.badge')).toHaveLength(1);
    expect(root().querySelector('.badge')?.textContent).toContain('apiTokens.expiredBadge');
  });

  it('shows the empty state when there are no tokens', async () => {
    await loadWith([]);

    expect(root().querySelector('.state')?.textContent).toContain('apiTokens.empty');
    expect(root().querySelector('.list')).toBeNull();
  });

  it('revokes a token after confirmation and reloads the list', async () => {
    await loadWith(TOKENS);

    button('apiTokens.revoke: ci')?.click();
    await fixture.whenStable();

    expect(dialog.open).toHaveBeenCalled();
    http
      .expectOne({ method: 'DELETE', url: '/api/v1/api-tokens/tok_bbbbbbbbbbbbbbbb' })
      .flush(null, { status: 204, statusText: 'No Content' });
    await fixture.whenStable();

    http.expectOne({ method: 'GET', url: '/api/v1/api-tokens' }).flush([TOKENS[0]]);
    await fixture.whenStable();
    fixture.detectChanges();

    expect(snack.open).toHaveBeenCalledWith('apiTokens.revoked', undefined, expect.any(Object));
    expect(root().querySelectorAll('.row__name')).toHaveLength(1);
  });

  it('does not call the API when the revocation is cancelled', async () => {
    dialog.open.mockReturnValueOnce({ afterClosed: () => of(false) });
    await loadWith(TOKENS);

    button('apiTokens.revoke: ci')?.click();
    await fixture.whenStable();

    expect(dialog.open).toHaveBeenCalled();
    expect(snack.open).not.toHaveBeenCalled();
    http.expectNone({ method: 'DELETE', url: '/api/v1/api-tokens/tok_bbbbbbbbbbbbbbbb' });
  });

  it('shows a translated error with retry when the list cannot be loaded', async () => {
    fixture.detectChanges();
    http
      .expectOne({ method: 'GET', url: '/api/v1/api-tokens' })
      .flush(
        { type: 'about:blank', title: 'Forbidden', status: 403, detail: 'nope' },
        { status: 403, statusText: 'Forbidden' },
      );
    await fixture.whenStable();
    fixture.detectChanges();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain(
      'apiTokens.error.forbidden',
    );

    root().querySelector<HTMLButtonElement>('[role="alert"] button')?.click();
    await fixture.whenStable();
    http.expectOne({ method: 'GET', url: '/api/v1/api-tokens' }).flush(TOKENS);
    await fixture.whenStable();
    fixture.detectChanges();

    expect(root().querySelector('[role="alert"]')).toBeNull();
    expect(root().querySelectorAll('.row')).toHaveLength(2);
  });
});
