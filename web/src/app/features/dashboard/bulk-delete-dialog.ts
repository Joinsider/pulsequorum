// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { MAT_DIALOG_DATA, MatDialogModule } from '@angular/material/dialog';
import { MatButtonModule } from '@angular/material/button';
import { TranslocoPipe } from '@jsverse/transloco';

/** Input of {@link BulkDeleteDialog}: how many monitors are about to be deleted. */
export interface BulkDeleteData {
  readonly count: number;
}

/**
 * Confirmation for the bulk delete on the dashboard. Closes with `true` only on
 * confirmation. Unlike the shared confirm dialog it shows the number of monitors.
 */
@Component({
  selector: 'app-bulk-delete-dialog',
  imports: [MatDialogModule, MatButtonModule, TranslocoPipe],
  template: `
    <h2 mat-dialog-title>{{ 'dashboard.bulk.deleteTitle' | transloco }}</h2>
    <mat-dialog-content class="form-stack">
      <p>{{ 'dashboard.bulk.deleteText' | transloco: { count: data.count } }}</p>
    </mat-dialog-content>
    <mat-dialog-actions align="end">
      <button mat-button type="button" [mat-dialog-close]="false">
        {{ 'common.cancel' | transloco }}
      </button>
      <button mat-flat-button type="button" color="warn" cdkFocusInitial [mat-dialog-close]="true">
        {{ 'common.delete' | transloco }}
      </button>
    </mat-dialog-actions>
  `,
  styles: `
    p {
      margin: 0;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class BulkDeleteDialog {
  protected readonly data = inject<BulkDeleteData>(MAT_DIALOG_DATA);
}
