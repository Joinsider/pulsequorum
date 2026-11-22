// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ProbesService } from '../../api/gen/endpoints/probes/probes.service';
import { TenantService } from '../../api/gen/endpoints/tenant/tenant.service';
import { OwnTenant, PrivateProbe } from '../../api/gen/model';
import { ProbesPage } from './probes-page';

const PROBES: PrivateProbe[] = [
  {
    id: 'p1',
    name: 'edge-corp-1',
    region: 'eu-west',
    capabilities: ['http', 'tcp'],
    version: '1.5.0',
    healthy: false,
    healthChangedAt: '2026-11-22T10:00:00Z',
    offlineAlert: true,
    lastContactAt: '2026-11-22T09:59:00Z',
    probeBuffer: { bytes: 2048, unacked: 4, droppedTotal: 1, oldestUnackedMs: 12_000 },
  },
];

const TENANT: OwnTenant = {
  id: 'acme',
  name: 'Acme',
  plan: 'pro',
  effectiveLimits: { maxPrivateProbes: 2 },
  usage: { monitors: 1, channels: 1, privateProbes: 2 },
  exceeded: [],
};

describe('ProbesPage', () => {
  let fixture: ComponentFixture<ProbesPage> | undefined;
  let api: {
    listPrivateProbes: ReturnType<typeof vi.fn>;
    removePrivateProbe: ReturnType<typeof vi.fn>;
  };
  let tenantApi: { getOwnTenant: ReturnType<typeof vi.fn> };
  let dialogResult: boolean;
  let dialog: { open: ReturnType<typeof vi.fn> };
  let snack: { open: ReturnType<typeof vi.fn> };

  beforeEach(() => {
    api = {
      listPrivateProbes: vi.fn(() => of(PROBES)),
      removePrivateProbe: vi.fn(() => of(undefined)),
    };
    tenantApi = { getOwnTenant: vi.fn(() => of(TENANT)) };
    dialogResult = true;
    dialog = { open: vi.fn(() => ({ afterClosed: () => of(dialogResult) })) };
    snack = { open: vi.fn() };

    TestBed.configureTestingModule({
      imports: [ProbesPage, TranslocoTestingModule.forRoot({ langs: { en: {} } })],
      providers: [
        { provide: ProbesService, useValue: api },
        { provide: TenantService, useValue: tenantApi },
        { provide: MatDialog, useValue: dialog },
        { provide: MatSnackBar, useValue: snack },
      ],
    });
  });

  afterEach(() => {
    fixture?.destroy();
    fixture = undefined;
  });

  it('loads private probes, displays offline alert & buffer, and shows reached quota', async () => {
    fixture = TestBed.createComponent(ProbesPage);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();

    const rows = fixture.nativeElement.querySelectorAll('[data-testid="private-probe-row"]');
    expect(rows.length).toBe(1);
    expect(rows[0].textContent).toContain('edge-corp-1');
    expect(
      fixture.nativeElement.querySelector('[data-testid="probe-offline-badge"]'),
    ).not.toBeNull();
    expect(fixture.nativeElement.querySelector('[data-testid="probes-quota"]')).not.toBeNull();
    expect(fixture.componentInstance['quota']()).toEqual({ used: 2, max: 2, reached: true });
  });

  it('confirms removal before deleting a private probe and reloads list', async () => {
    fixture = TestBed.createComponent(ProbesPage);
    fixture.detectChanges();
    await fixture.whenStable();

    dialogResult = false;
    await fixture.componentInstance['remove'](PROBES[0]);
    expect(api.removePrivateProbe).not.toHaveBeenCalled();

    dialogResult = true;
    await fixture.componentInstance['remove'](PROBES[0]);
    expect(api.removePrivateProbe).toHaveBeenCalledWith('p1');
    expect(snack.open).toHaveBeenCalled();
    expect(api.listPrivateProbes).toHaveBeenCalledTimes(2);
  });

  it('opens the token dialog and reloads when a token is created', async () => {
    fixture = TestBed.createComponent(ProbesPage);
    fixture.detectChanges();
    await fixture.whenStable();

    dialogResult = true;
    await fixture.componentInstance['createToken']();
    expect(dialog.open).toHaveBeenCalledOnce();
    expect(api.listPrivateProbes).toHaveBeenCalledTimes(2);
  });
});
