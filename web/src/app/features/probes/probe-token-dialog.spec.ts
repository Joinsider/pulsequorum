// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialogRef } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { firstValueFrom, of, throwError } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ProbesService } from '../../api/gen/endpoints/probes/probes.service';
import { SystemService } from '../../api/gen/endpoints/system/system.service';
import { ProbeTokenDialog, probeImageTagFor } from './probe-token-dialog';

describe('ProbeTokenDialog', () => {
  let fixture: ComponentFixture<ProbeTokenDialog> | undefined;
  let api: { createPrivateProbeJoinToken: ReturnType<typeof vi.fn> };
  let ref: { close: ReturnType<typeof vi.fn> };
  let system: { getHealth: ReturnType<typeof vi.fn> };

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
    system = { getHealth: vi.fn(() => of({ version: '0.0.5' })) };
    TestBed.configureTestingModule({
      imports: [ProbeTokenDialog, TranslocoTestingModule.forRoot({ langs: { en: {} } })],
      providers: [
        { provide: ProbesService, useValue: api },
        { provide: SystemService, useValue: system },
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

  it('pins the quick-start snippet to the probe image of the server version', async () => {
    fixture = TestBed.createComponent(ProbeTokenDialog);
    fixture.detectChanges();
    await fixture.componentInstance['create']();
    fixture.detectChanges();

    const snippet: string = fixture.nativeElement.querySelector(
      '[data-testid="probe-compose-snippet"]',
    ).textContent;
    expect(snippet).toContain('image: ghcr.io/joinsider/pulsequorum-probe:0.0.5');
    expect(snippet).not.toContain(':latest');
    expect(snippet).toContain('PROBE_ALLOW_TARGETS: "<allowed-targets');
  });

  it('falls back to the version placeholder for a development server', async () => {
    system.getHealth.mockReturnValue(of({ version: 'dev' }));
    fixture = TestBed.createComponent(ProbeTokenDialog);
    fixture.detectChanges();
    await fixture.componentInstance['create']();
    fixture.detectChanges();

    const snippet: string = fixture.nativeElement.querySelector(
      '[data-testid="probe-compose-snippet"]',
    ).textContent;
    expect(snippet).toContain('image: ghcr.io/joinsider/pulsequorum-probe:<version>');
  });
});

describe('probeImageTagFor', () => {
  it.each([
    ['0.0.5', '0.0.5'],
    ['v0.0.5', '0.0.5'],
    ['1.2.3-rc.1', '1.2.3-rc.1'],
    ['dev', '<version>'],
    ['', '<version>'],
    [null, '<version>'],
    [undefined, '<version>'],
    ['0.0.5; rm -rf /', '<version>'],
  ])('maps %j to %j', (version, tag) => {
    expect(probeImageTagFor(version)).toBe(tag);
  });
});
