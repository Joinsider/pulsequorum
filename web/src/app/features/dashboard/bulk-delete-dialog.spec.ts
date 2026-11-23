// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { TestBed } from '@angular/core/testing';
import { MAT_DIALOG_DATA } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { BulkDeleteDialog } from './bulk-delete-dialog';

describe('BulkDeleteDialog', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('renders the confirmation dialog with the monitor count', () => {
    TestBed.configureTestingModule({
      imports: [
        BulkDeleteDialog,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [{ provide: MAT_DIALOG_DATA, useValue: { count: 5 } }],
    });
    const fixture = TestBed.createComponent(BulkDeleteDialog);
    fixture.detectChanges();
    const el = fixture.nativeElement as HTMLElement;
    expect(el.textContent).toContain('dashboard.bulk.deleteTitle');
    expect(el.querySelectorAll('button').length).toBe(2);
  });
});
