// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AuditEntry, AuditPage as AuditPageResponse } from '../../api/gen/model';
import { AuditPage as AuditPageComponent } from './audit-page';

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    at: '2026-11-19T10:00:00Z',
    tenantId: 't1',
    actor: { kind: 'user', id: 'u1', name: 'Alice' },
    action: 'monitor.update',
    objectKind: 'monitor',
    objectId: '6f1c7a2e-0000-4000-8000-000000000001',
    requestId: 'req-0001-aaaa-bbbb',
    ...overrides,
  };
}

const FIRST_PAGE: AuditPageResponse = {
  items: [
    entry(),
    entry({ actor: { kind: 'api_token', id: 'tok1', name: 'ci' }, action: 'monitor.pause' }),
  ],
  nextCursor: 'c2',
};

describe('AuditPage', () => {
  let fixture: ComponentFixture<AuditPageComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        AuditPageComponent,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(AuditPageComponent);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  function root(): HTMLElement {
    return fixture.nativeElement as HTMLElement;
  }

  function buttonWith(text: string): HTMLButtonElement | undefined {
    return [...root().querySelectorAll('button')].find((b) => b.textContent?.includes(text));
  }

  async function settle(): Promise<void> {
    await fixture.whenStable();
    fixture.detectChanges();
  }

  it('tenant scope lists the entries of /audit without a tenant column', async () => {
    fixture.detectChanges();
    const req = http.expectOne((r) => r.url === '/api/v1/audit');
    expect(req.request.method).toBe('GET');
    expect(req.request.params.get('limit')).toBe('50');
    expect(req.request.params.has('tenant')).toBe(false);
    req.flush({ items: FIRST_PAGE.items, nextCursor: null });
    await settle();

    const rows = root().querySelectorAll('tbody tr');
    expect(rows).toHaveLength(2);
    expect(rows[0].querySelector('.actor')?.textContent?.trim()).toBe('Alice');
    expect(rows[1].querySelector('.kind')?.textContent?.trim()).toBe('audit.actorKind.apiToken');
    expect(root().querySelectorAll('th')).toHaveLength(5);
    expect(buttonWith('audit.loadMore')).toBeUndefined();
  });

  it('load more appends the next page behind the cursor', async () => {
    fixture.detectChanges();
    http.expectOne((r) => r.url === '/api/v1/audit').flush(FIRST_PAGE);
    await settle();

    buttonWith('audit.loadMore')?.click();
    const req = http.expectOne((r) => r.url === '/api/v1/audit');
    expect(req.request.params.get('cursor')).toBe('c2');
    req.flush({ items: [entry({ action: 'monitor.delete' })], nextCursor: null });
    await settle();

    const actions = [...root().querySelectorAll('tbody tr code:not(.request)')]
      .map((el) => el.textContent?.trim())
      .filter((text) => text?.startsWith('monitor.'));
    expect(actions).toEqual(['monitor.update', 'monitor.pause', 'monitor.delete']);
    expect(buttonWith('audit.loadMore')).toBeUndefined();
  });

  it('platform scope calls listAdminAudit with the tenant filter and shows the tenant column', async () => {
    fixture.componentRef.setInput('scope', 'platform');
    fixture.detectChanges();
    http.expectOne((r) => r.url === '/api/v1/admin/audit').flush({ items: [], nextCursor: null });
    await settle();
    expect(root().textContent).toContain('audit.empty');

    const tenant = root().querySelector<HTMLInputElement>('input[formcontrolname="tenant"]');
    expect(tenant).not.toBeNull();
    tenant!.value = ' _system ';
    tenant!.dispatchEvent(new Event('input'));
    root().querySelector('form')!.dispatchEvent(new Event('submit'));

    const req = http.expectOne((r) => r.url === '/api/v1/admin/audit');
    expect(req.request.params.get('tenant')).toBe('_system');
    req.flush({ items: [entry({ tenantId: '_system' })], nextCursor: null });
    await settle();

    const headers = [...root().querySelectorAll('th')].map((th) => th.textContent?.trim());
    expect(headers).toContain('audit.columns.tenant');
    expect(root().querySelector('tbody tr td:nth-child(5) code')?.textContent?.trim()).toBe(
      '_system',
    );
  });

  it('shows a translated error with retry and recovers on retry', async () => {
    fixture.detectChanges();
    http
      .expectOne((r) => r.url === '/api/v1/audit')
      .flush(
        { type: 'about:blank', title: 'Unavailable', status: 503 },
        { status: 503, statusText: 'Service Unavailable' },
      );
    await settle();

    const alert = root().querySelector('[role="alert"]');
    expect(alert?.textContent).toContain('audit.error.unavailable');

    buttonWith('common.retry')?.click();
    http.expectOne((r) => r.url === '/api/v1/audit').flush({ items: [entry()], nextCursor: null });
    await settle();

    expect(root().querySelectorAll('tbody tr')).toHaveLength(1);
    expect(root().querySelector('[role="alert"]')).toBeNull();
  });
});
