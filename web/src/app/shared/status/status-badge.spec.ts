// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { MonitorStatus } from '../../api/gen/model';
import { StatusBadge } from './status-badge';
import { statusColorVar } from './status-colors';

describe('statusColorVar', () => {
  it.each(Object.values(MonitorStatus))('maps %s to its CSS variable', (status) => {
    expect(statusColorVar(status)).toBe(`var(--status-${status.toLowerCase()})`);
  });
});

describe('StatusBadge', () => {
  let fixture: ComponentFixture<StatusBadge>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        StatusBadge,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
    });
    fixture = TestBed.createComponent(StatusBadge);
  });

  it('renders the translation key of the status', () => {
    fixture.componentRef.setInput('status', MonitorStatus.DOWN);
    fixture.detectChanges();

    expect((fixture.nativeElement as HTMLElement).textContent?.trim()).toBe('status.DOWN');
  });

  it('colours the pill with the status variable', () => {
    fixture.componentRef.setInput('status', MonitorStatus.DEGRADED);
    fixture.detectChanges();

    expect((fixture.nativeElement as HTMLElement).style.getPropertyValue('--badge-color')).toBe(
      'var(--status-degraded)',
    );
  });
});
