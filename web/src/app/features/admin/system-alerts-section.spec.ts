// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of, throwError } from 'rxjs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SystemAlert } from '../../api/gen/model';
import { SystemAlertsSection } from './system-alerts-section';
import { SystemAlertsService } from './system-alerts.service';

const ALERT: SystemAlert = {
  kind: 'voter_offline',
  subject: 'n-voter-1111111111',
  active: true,
  level: 0,
  since: '2026-11-17T09:00:00Z',
  updated: '2026-11-17T09:00:00Z',
  details: { node: 'n-voter-1111111111', lastSeenMs: 90000 },
};

describe('SystemAlertsSection', () => {
  let fixture: ComponentFixture<SystemAlertsSection> | undefined;

  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  afterEach(() => {
    fixture?.destroy();
    fixture = undefined;
  });

  function create(active: ReturnType<typeof vi.fn>): ComponentFixture<SystemAlertsSection> {
    TestBed.configureTestingModule({
      imports: [
        SystemAlertsSection,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [{ provide: SystemAlertsService, useValue: { active } }],
    });
    fixture = TestBed.createComponent(SystemAlertsSection);
    return fixture;
  }

  async function settle(f: ComponentFixture<SystemAlertsSection>): Promise<HTMLElement> {
    await f.whenStable();
    f.detectChanges();
    return f.nativeElement as HTMLElement;
  }

  it('lists the active alerts with their subject, level and details', async () => {
    const active = vi.fn(() => of([ALERT]));
    const f = create(active);
    f.detectChanges();
    const root = await settle(f);

    expect(active).toHaveBeenCalledOnce();
    const items = root.querySelectorAll('[data-testid="system-alert"]');
    expect(items).toHaveLength(1);
    const text = items[0]?.textContent ?? '';
    expect(text).toContain('voter_offline');
    expect(text).toContain('n-voter-1111111111');
    expect(text).toContain('lastSeenMs');
    expect(text).toContain('90000');
  });

  it('shows the empty state when no alert is active', async () => {
    const f = create(vi.fn(() => of([])));
    f.detectChanges();
    const root = await settle(f);

    expect(root.querySelector('[data-testid="system-alerts-empty"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="system-alert"]')).toBeNull();
  });

  it('shows an error with a retry when the alerts cannot be loaded', async () => {
    const f = create(vi.fn(() => throwError(() => new Error('down'))));
    f.detectChanges();
    const root = await settle(f);

    expect(root.querySelector('[role="alert"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="system-alert"]')).toBeNull();
  });
});
