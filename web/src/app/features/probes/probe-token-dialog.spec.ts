// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialogRef } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { firstValueFrom, of, throwError } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ProbesService } from '../../api/gen/endpoints/probes/probes.service';
import { ProbeTokenDialog } from './probe-token-dialog';

describe('ProbeTokenDialog', () => {
  let fixture: ComponentFixture<ProbeTokenDialog> | undefined;
  let api: { createPrivateProbeJoinToken: ReturnType<typeof vi.fn> };
  let ref: { close: ReturnType<typeof vi.fn> };

  beforeEach(() => {
    api = {
      createPrivateProbeJoinToken: vi.fn(() =>
        of({
          token: 'secret-probe-join-token',
          expiresAt: '2030-01-01T00:00:00Z',
          role: 'probe',
          tenantId: 'default',
        }),
      ),
    };
    ref = { close: vi.fn() };
    TestBed.configureTestingModule({
      imports: [ProbeTokenDialog, TranslocoTestingModule.forRoot({ langs: { en: {} } })],
      providers: [
        { provide: ProbesService, useValue: api },
        { provide: MatDialogRef, useValue: ref },
      ],
    });
  });

  afterEach(() => {
    fixture?.destroy();
    fixture = undefined;
  });

  it('creates a token once, renders snippet, and clears the token on destroy', async () => {
    fixture = TestBed.createComponent(ProbeTokenDialog);
    fixture.detectChanges();
    const cmp = fixture.componentInstance;

    await cmp['create']();
    fixture.detectChanges();

    expect(api.createPrivateProbeJoinToken).toHaveBeenCalledWith({ ttlMinutes: 60 });
    expect(cmp['token']()).toBe('secret-probe-join-token');
    expect(fixture.nativeElement.querySelector('[data-testid="probe-join-token"]')?.value).toBe(
      'secret-probe-join-token',
    );
    expect(
      fixture.nativeElement.querySelector('[data-testid="probe-compose-snippet"]')?.textContent,
    ).toContain('NODE_ROLE: probe');

    cmp['close']();
    expect(ref.close).toHaveBeenCalledWith(true);

    fixture.destroy();
    expect(cmp['token']()).toBeNull();
    expect(cmp['expiresAt']()).toBeNull();
    fixture = undefined;
  });

  it('displays server detail when maxPrivateProbes limit returns 403', async () => {
    api.createPrivateProbeJoinToken.mockReturnValue(
      throwError(
        () =>
          new HttpErrorResponse({
            status: 403,
            error: {
              type: 'about:blank',
              title: 'Forbidden',
              status: 403,
              detail: 'private probe limit reached: plan "starter" allows 1 private probes',
            },
          }),
      ),
    );
    fixture = TestBed.createComponent(ProbeTokenDialog);
    fixture.detectChanges();
    const cmp = fixture.componentInstance;

    await cmp['create']();
    fixture.detectChanges();

    expect(cmp['errorKey']()).toBe('probes.tokenDialog.error.forbidden');
    expect(cmp['errorDetail']()).toContain('private probe limit reached');

    // Keep firstValueFrom reference valid if unused in assertions.
    void firstValueFrom;
  });
});
