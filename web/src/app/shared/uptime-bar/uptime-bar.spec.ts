// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { describe, expect, it } from 'vitest';
import { UptimeBucket } from '../../api/gen/model';
import { BucketState, UptimeBar, bucketState, uptimePercent } from './uptime-bar';

function bucket(overrides: Partial<UptimeBucket> = {}): UptimeBucket {
  return {
    start: '2026-10-21T10:00:00Z',
    upSeconds: 0,
    downSeconds: 0,
    maintenanceSeconds: 0,
    unknownSeconds: 0,
    ...overrides,
  };
}

describe('bucketState', () => {
  it.each<[string, Partial<UptimeBucket>, BucketState]>([
    ['no observed time is unknown', {}, 'unknown'],
    ['only unknown time is unknown', { unknownSeconds: 60 }, 'unknown'],
    ['all up is up', { upSeconds: 60 }, 'up'],
    ['up with some unknown is up', { upSeconds: 50, unknownSeconds: 10 }, 'up'],
    ['maintenance only is maintenance', { maintenanceSeconds: 60 }, 'maintenance'],
    ['maintenance with up is up', { upSeconds: 30, maintenanceSeconds: 30 }, 'up'],
    ['down share of exactly 5 % is down', { upSeconds: 57, downSeconds: 3 }, 'down'],
    ['down share above 5 % is down', { upSeconds: 10, downSeconds: 50 }, 'down'],
    ['small down share is degraded', { upSeconds: 97, downSeconds: 3 }, 'degraded'],
    [
      'down share below 5 % with 1 s of down is degraded',
      { upSeconds: 9999, downSeconds: 1 },
      'degraded',
    ],
  ])('%s', (_name, values, expected) => {
    expect(bucketState(bucket(values))).toBe(expected);
  });

  it('counts a bucket that is only down as down', () => {
    expect(bucketState(bucket({ downSeconds: 60 }))).toBe('down');
  });
});

describe('uptimePercent', () => {
  it('ignores maintenance and unknown time', () => {
    expect(uptimePercent(bucket({ upSeconds: 90, downSeconds: 10, maintenanceSeconds: 50 }))).toBe(
      90,
    );
  });

  it('is null without up or down time', () => {
    expect(uptimePercent(bucket({ maintenanceSeconds: 60 }))).toBeNull();
  });
});

describe('UptimeBar', () => {
  let fixture: ComponentFixture<UptimeBar>;

  function render(buckets: UptimeBucket[]): HTMLElement {
    TestBed.configureTestingModule({
      imports: [
        UptimeBar,
        TranslocoTestingModule.forRoot({
          langs: {
            en: {
              uptimeBar: {
                label: 'Uptime history',
                tooltip: '{{ time }}: {{ detail }}',
                percent: '{{ value }}% uptime',
                noData: 'No data',
              },
              status: { MAINTENANCE: 'Maintenance' },
            },
          },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
    });
    fixture = TestBed.createComponent(UptimeBar);
    fixture.componentRef.setInput('buckets', buckets);
    fixture.detectChanges();
    return fixture.nativeElement as HTMLElement;
  }

  it('renders one bar per bucket with its state', () => {
    const el = render([
      bucket({ start: '2026-10-21T10:00:00Z', upSeconds: 60 }),
      bucket({ start: '2026-10-21T10:01:00Z', downSeconds: 60 }),
      bucket({ start: '2026-10-21T10:02:00Z', maintenanceSeconds: 60 }),
      bucket({ start: '2026-10-21T10:03:00Z' }),
    ]);
    const states = [...el.querySelectorAll('.bar')].map((b) => b.getAttribute('data-state'));

    expect(states).toEqual(['up', 'down', 'maintenance', 'unknown']);
  });

  it('gives every bar a tooltip with its uptime', () => {
    const el = render([bucket({ upSeconds: 75, downSeconds: 25 })]);
    const title = el.querySelector('.bar')?.getAttribute('title') ?? '';

    expect(title).toContain('75% uptime');
  });

  it('shows a placeholder without buckets', () => {
    const el = render([]);

    expect(el.querySelectorAll('.bar')).toHaveLength(0);
    expect(el.textContent).toContain('No data');
  });
});
