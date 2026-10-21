// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { signal } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { Me, User } from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { UsersPage } from './users-page';

const ME: Me = {
  id: 'me',
  username: 'admin',
  tenantId: 't1',
  roles: ['tenant-admin'],
  authMode: 'local',
};

const USERS: User[] = [
  {
    id: 'me',
    username: 'admin',
    role: 'tenant-admin',
    disabled: false,
    createdAt: '2026-01-01T00:00:00Z',
  },
  {
    id: 'p1',
    username: 'root',
    role: 'platform-admin',
    disabled: false,
    createdAt: '2026-01-02T00:00:00Z',
  },
  {
    id: 'bob',
    username: 'bob',
    displayName: 'Bob',
    role: 'tenant-viewer',
    disabled: true,
    createdAt: '2026-01-03T00:00:00Z',
  },
];

describe('UsersPage', () => {
  let fixture: ComponentFixture<UsersPage>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        UsersPage,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        { provide: AuthService, useValue: { me: signal(ME).asReadonly() } },
      ],
    });
    fixture = TestBed.createComponent(UsersPage);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function button(label: string): HTMLButtonElement | null {
    return root().querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
  }

  async function loadWith(users: User[]) {
    fixture.detectChanges();
    http.expectOne({ method: 'GET', url: '/api/v1/users' }).flush(users);
    await fixture.whenStable();
    fixture.detectChanges();
  }

  it('lists the users sorted by name', async () => {
    await loadWith(USERS);

    const names = [...root().querySelectorAll('.row__name')].map((el) => el.textContent?.trim());
    expect(names).toEqual(['admin', 'Bob', 'root']);
  });

  it('disables the actions the server would refuse', async () => {
    await loadWith(USERS);

    expect(button('common.edit: root')?.disabled).toBe(true);
    expect(button('common.delete: root')?.disabled).toBe(true);
    expect(button('common.delete: admin')?.disabled).toBe(true);
    expect(button('common.edit: admin')?.disabled).toBe(false);
    expect(button('common.edit: bob')?.disabled).toBe(false);
    expect(button('common.delete: bob')?.disabled).toBe(false);
  });

  it('shows a translated error when the list cannot be loaded', async () => {
    fixture.detectChanges();
    http
      .expectOne({ method: 'GET', url: '/api/v1/users' })
      .flush(
        { title: 'Forbidden', status: 403, detail: 'nope' },
        { status: 403, statusText: 'Forbidden' },
      );
    await fixture.whenStable();
    fixture.detectChanges();

    expect(root().querySelector('[role="alert"]')?.textContent).toContain('users.error.forbidden');
  });
});
