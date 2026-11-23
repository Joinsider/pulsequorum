// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { TestBed } from '@angular/core/testing';
import { MAT_DIALOG_DATA } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { TenantDeleteDialog } from './tenant-delete-dialog';

describe('TenantDeleteDialog', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('enables confirmation only when the typed name matches exactly', () => {
    TestBed.configureTestingModule({
      imports: [
        TenantDeleteDialog,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [{ provide: MAT_DIALOG_DATA, useValue: { name: 'Acme Corp' } }],
    });
    const fixture = TestBed.createComponent(TenantDeleteDialog);
    fixture.detectChanges();
    const instance = fixture.componentInstance as unknown as {
      typed: () => string;
      matches: () => boolean;
      onInput: (event: Event) => void;
    };

    expect(instance.typed()).toBe('');
    expect(instance.matches()).toBe(false);

    instance.onInput({ target: { value: 'acme corp' } } as unknown as Event);
    expect(instance.matches()).toBe(false);

    instance.onInput({ target: { value: 'Acme Corp' } } as unknown as Event);
    expect(instance.matches()).toBe(true);
  });
});
