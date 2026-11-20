// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MAT_DIALOG_DATA, MatDialogModule } from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { TranslocoDirective } from '@jsverse/transloco';

/** Input of the dialog: the tenant name the user has to type. */
export interface TenantDeleteData {
  readonly name: string;
}

/**
 * Permanent deletion of a suspended tenant (ADR 0029). The user types the exact
 * name; only then the button is enabled. Closes with `true` on confirmation.
 */
@Component({
  selector: 'app-tenant-delete-dialog',
  imports: [
    MatDialogModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    TranslocoDirective,
  ],
  template: `
    <ng-container *transloco="let t">
      <h2 mat-dialog-title>{{ t('admin.tenants.delete.title', { name: data.name }) }}</h2>
      <mat-dialog-content class="content">
        <p class="warn" role="note">{{ t('admin.tenants.delete.text') }}</p>
        <p class="hint">{{ t('admin.tenants.delete.copies') }}</p>
        <mat-form-field appearance="outline">
          <mat-label>{{ t('admin.tenants.delete.typeLabel', { name: data.name }) }}</mat-label>
          <input
            matInput
            autocomplete="off"
            autocapitalize="none"
            [value]="typed()"
            (input)="onInput($event)"
          />
        </mat-form-field>
      </mat-dialog-content>
      <mat-dialog-actions align="end">
        <button mat-button type="button" [mat-dialog-close]="false">
          {{ t('common.cancel') }}
        </button>
        <button
          mat-flat-button
          color="warn"
          type="button"
          [disabled]="!matches()"
          [mat-dialog-close]="true"
        >
          {{ t('admin.tenants.delete.confirm') }}
        </button>
      </mat-dialog-actions>
    </ng-container>
  `,
  styles: `
    .content {
      display: flex;
      flex-direction: column;
    }
    mat-form-field {
      width: 100%;
    }
    .warn {
      margin: 0 0 0.5rem;
      color: var(--danger);
    }
    .hint {
      margin: 0 0 0.75rem;
      font-size: 0.875rem;
      color: var(--muted);
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class TenantDeleteDialog {
  protected readonly data = inject<TenantDeleteData>(MAT_DIALOG_DATA);
  protected readonly typed = signal('');
  /** Exact match: the server compares the same string. */
  protected readonly matches = computed(() => this.typed() === this.data.name);

  protected onInput(event: Event): void {
    this.typed.set((event.target as HTMLInputElement).value);
  }
}
