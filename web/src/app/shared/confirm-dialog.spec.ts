// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { TestBed } from '@angular/core/testing';
import { MAT_DIALOG_DATA, MatDialog, MatDialogRef } from '@angular/material/dialog';
import { TranslocoTestingModule } from '@jsverse/transloco';
import { of } from 'rxjs';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ConfirmData, ConfirmDialog, confirm } from './confirm-dialog';

describe('ConfirmDialog and confirm()', () => {
  beforeEach(() => {
    TestBed.resetTestingModule();
  });

  it('renders title, message and buttons for ConfirmDialog', () => {
    const data: ConfirmData = {
      titleKey: 'dialog.title',
      messageKey: 'dialog.message',
      confirmKey: 'common.delete',
      danger: true,
    };
    TestBed.configureTestingModule({
      imports: [
        ConfirmDialog,
        TranslocoTestingModule.forRoot({
          langs: { en: {} },
          translocoConfig: { availableLangs: ['en'], defaultLang: 'en' },
        }),
      ],
      providers: [{ provide: MAT_DIALOG_DATA, useValue: data }],
    });
    const fixture = TestBed.createComponent(ConfirmDialog);
    fixture.detectChanges();
    const text = (fixture.nativeElement as HTMLElement).textContent ?? '';
    expect(text).toContain('dialog.title');
    expect(text).toContain('dialog.message');
  });

  it('opens ConfirmDialog with defaults and resolves true on confirmation', async () => {
    const dialog = {
      open: vi.fn(
        () =>
          ({
            afterClosed: () => of(true),
          }) as unknown as MatDialogRef<ConfirmDialog, boolean>,
      ),
    } as unknown as MatDialog;

    const ok = await confirm(dialog, { titleKey: 't', messageKey: 'm' });
    expect(ok).toBe(true);
    expect(dialog.open).toHaveBeenCalledWith(ConfirmDialog, {
      data: {
        titleKey: 't',
        messageKey: 'm',
        confirmKey: 'common.confirm',
        danger: false,
      },
      role: 'alertdialog',
      autoFocus: 'dialog',
      restoreFocus: true,
    });
  });

  it('resolves false when dismissed or cancelled', async () => {
    const dialog = {
      open: vi.fn(
        () =>
          ({
            afterClosed: () => of(undefined),
          }) as unknown as MatDialogRef<ConfirmDialog, boolean>,
      ),
    } as unknown as MatDialog;

    const ok = await confirm(dialog, {
      titleKey: 't',
      messageKey: 'm',
      confirmKey: 'common.delete',
      danger: true,
    });
    expect(ok).toBe(false);
  });
});
