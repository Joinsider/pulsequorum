// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import {
  MAT_DIALOG_DATA,
  MatDialog,
  MatDialogModule,
  MatDialogRef,
} from '@angular/material/dialog';
import { MatButtonModule } from '@angular/material/button';
import { TranslocoPipe } from '@jsverse/transloco';
import { firstValueFrom, map } from 'rxjs';

/** Translation keys and style of a confirmation. */
export interface ConfirmOptions {
  titleKey: string;
  messageKey: string;
  /** Label of the confirming button; defaults to `common.confirm`. */
  confirmKey?: string;
  /** Marks irreversible actions: the confirm button uses the warning colour. */
  danger?: boolean;
}

export interface ConfirmData extends Required<Omit<ConfirmOptions, 'danger'>> {
  danger: boolean;
}

/** Dialog body used by {@link confirm}. Closes with `true` only on confirmation. */
@Component({
  selector: 'app-confirm-dialog',
  imports: [MatDialogModule, MatButtonModule, TranslocoPipe],
  template: `
    <h2 mat-dialog-title>{{ data.titleKey | transloco }}</h2>
    <mat-dialog-content>
      <p>{{ data.messageKey | transloco }}</p>
    </mat-dialog-content>
    <mat-dialog-actions align="end">
      <button mat-button type="button" [mat-dialog-close]="false">
        {{ 'common.cancel' | transloco }}
      </button>
      <button
        mat-flat-button
        type="button"
        cdkFocusInitial
        [color]="data.danger ? 'warn' : 'primary'"
        [mat-dialog-close]="true"
      >
        {{ data.confirmKey | transloco }}
      </button>
    </mat-dialog-actions>
  `,
  styles: `
    p {
      margin: 0;
      white-space: pre-line;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ConfirmDialog {
  protected readonly data = inject<ConfirmData>(MAT_DIALOG_DATA);
}

/**
 * Asks the user to confirm an action in a modal dialog.
 *
 * @example
 * if (await confirm(this.dialog, { titleKey: 'monitors.deleteTitle', messageKey: 'monitors.deleteText', confirmKey: 'common.delete', danger: true })) { ... }
 *
 * @returns true only if the user confirmed; dismissing the dialog returns false.
 */
export function confirm(dialog: MatDialog, options: ConfirmOptions): Promise<boolean> {
  const data: ConfirmData = {
    titleKey: options.titleKey,
    messageKey: options.messageKey,
    confirmKey: options.confirmKey ?? 'common.confirm',
    danger: options.danger ?? false,
  };
  const ref: MatDialogRef<ConfirmDialog, boolean> = dialog.open(ConfirmDialog, {
    data,
    role: 'alertdialog',
    autoFocus: 'dialog',
    restoreFocus: true,
  });
  return firstValueFrom(ref.afterClosed().pipe(map((result) => result === true)), {
    defaultValue: false,
  });
}
