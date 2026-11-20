// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import {
  MAT_DIALOG_DATA,
  MatDialog,
  MatDialogModule,
  MatDialogRef,
} from '@angular/material/dialog';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom, map } from 'rxjs';

/** Input of the dialog: which node, and what its removal does to the quorum. */
export interface RemoveNodeDialogData {
  /** `remove` drops the node; `replace` drops it and then asks for a new voter. */
  mode: 'remove' | 'replace';
  /** Display name of the node (a name or the short id). */
  nodeName: string;
  votersLeft: number;
  toleratesAfter: number;
  /** Translation key of a quorum risk, or null. */
  riskKey: string | null;
}

/** Confirmation with the quorum impact of a removal. Closes with `true` only on confirmation. */
@Component({
  selector: 'app-remove-node-dialog',
  imports: [MatDialogModule, MatButtonModule, TranslocoDirective],
  template: `
    <ng-container *transloco="let t">
      <h2 mat-dialog-title>{{ t('admin.cluster.' + data.mode + '.title') }}</h2>
      <mat-dialog-content class="form-stack">
        <p>{{ t('admin.cluster.' + data.mode + '.text', { name: data.nodeName }) }}</p>
        @if (data.mode === 'replace') {
          <p>{{ t('admin.cluster.replace.warnReserve') }}</p>
          <p>{{ t('admin.cluster.replace.warnData') }}</p>
        }
        <ul class="impact">
          <li>{{ t('admin.cluster.impact.votersLeft', { count: data.votersLeft }) }}</li>
          <li>{{ t('admin.cluster.impact.tolerates', { count: data.toleratesAfter }) }}</li>
        </ul>
        @if (data.riskKey; as key) {
          <p class="risk" role="note">{{ t(key) }}</p>
        }
      </mat-dialog-content>
      <mat-dialog-actions align="end">
        <button mat-button type="button" [mat-dialog-close]="false">
          {{ t('common.cancel') }}
        </button>
        <button
          mat-flat-button
          type="button"
          color="warn"
          cdkFocusInitial
          [mat-dialog-close]="true"
        >
          {{ t('admin.cluster.' + data.mode + '.confirm') }}
        </button>
      </mat-dialog-actions>
    </ng-container>
  `,
  styles: `
    p {
      margin: 0;
      overflow-wrap: anywhere;
    }
    .impact {
      margin: 0;
      padding-left: 1.25rem;
    }
    .risk {
      color: var(--status-degraded);
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class RemoveNodeDialog {
  protected readonly data = inject<RemoveNodeDialogData>(MAT_DIALOG_DATA);
}

/**
 * Asks for confirmation of a node removal or replacement.
 *
 * @returns true only if the user confirmed; dismissing the dialog returns false.
 */
export function openRemoveNodeDialog(
  dialog: MatDialog,
  data: RemoveNodeDialogData,
): Promise<boolean> {
  const ref: MatDialogRef<RemoveNodeDialog, boolean> = dialog.open(RemoveNodeDialog, {
    data,
    role: 'alertdialog',
    width: 'min(32rem, calc(100vw - 2rem))',
    autoFocus: 'dialog',
    restoreFocus: true,
  });
  return firstValueFrom(ref.afterClosed().pipe(map((result) => result === true)), {
    defaultValue: false,
  });
}
