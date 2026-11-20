// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { FlappingBadge } from './flapping-badge';

describe('FlappingBadge', () => {
  let fixture: ComponentFixture<FlappingBadge>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        FlappingBadge,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
    });
    fixture = TestBed.createComponent(FlappingBadge);
    fixture.detectChanges();
  });

  it('renders the translation key of the flapping label', () => {
    expect((fixture.nativeElement as HTMLElement).textContent?.trim()).toBe('status.flapping');
  });
});
