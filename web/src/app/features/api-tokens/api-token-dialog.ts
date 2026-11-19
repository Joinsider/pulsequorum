// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { CdkCopyToClipboard } from '@angular/cdk/clipboard';
import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject, OnDestroy, signal } from '@angular/core';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog, MatDialogModule, MatDialogRef } from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { ApiTokensService } from '../../api/gen/endpoints/api-tokens/api-tokens.service';
import { ApiTokenRole } from '../../api/gen/model';
import { Icon } from '../../shared/icon/icon';
import { applyFieldErrors, problemOf } from '../../shared/problem';

/** Lifetimes offered in the dialog, in days. The server accepts 1 hour to 366 days (ADR 0031). */
export const API_TOKEN_LIFETIMES = [7, 30, 90, 180, 365] as const;
export const API_TOKEN_LIFETIME_DEFAULT = 90;

/** Roles a token may carry, weakest first (ADR 0031: tenant roles only). */
export const API_TOKEN_ROLES: readonly ApiTokenRole[] = [
  ApiTokenRole.ApiTokenViewer,
  ApiTokenRole.ApiTokenEditor,
  ApiTokenRole.ApiTokenAdmin,
];

/** Expiry timestamp `days` days after `now`, as sent in ApiTokenInput.expiresAt. */
export function tokenExpiry(now: Date, days: number): string {
  return new Date(now.getTime() + days * 24 * 60 * 60 * 1000).toISOString();
}

/** Translation key for a failed create request. */
export function apiTokenCreateErrorKey(status: number | undefined): string {
  switch (status) {
    case 403:
      return 'apiTokens.error.forbidden';
    case 409:
      return 'apiTokens.error.conflict';
    case 422:
      return 'apiTokens.error.invalid';
    case 503:
      return 'apiTokens.error.unavailable';
    default:
      return 'apiTokens.error.generic';
  }
}

/**
 * Creates an API token and shows its secret exactly once (FA-AUTH-10, ADR 0031).
 *
 * The secret (`uct_…`) lives only in this dialog's `secret` signal. It is never
 * logged, never put into storage, the URL, a snackbar or the token list, and the
 * signal is cleared when the dialog is destroyed. The server sends the response
 * with Cache-Control no-store and keeps only the SHA-256. The dialog does not
 * close on backdrop click or Escape, so the secret cannot be lost by accident.
 */
@Component({
  selector: 'app-api-token-dialog',
  imports: [
    ReactiveFormsModule,
    DatePipe,
    CdkCopyToClipboard,
    MatDialogModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    Icon,
    TranslocoDirective,
  ],
  templateUrl: './api-token-dialog.html',
  styleUrl: './api-token-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ApiTokenDialog implements OnDestroy {
  private readonly api = inject(ApiTokensService);
  private readonly ref = inject<MatDialogRef<ApiTokenDialog, boolean>>(MatDialogRef);

  protected readonly roles = API_TOKEN_ROLES;
  protected readonly lifetimes = API_TOKEN_LIFETIMES;

  protected readonly form = inject(NonNullableFormBuilder).group({
    name: ['', [Validators.required, Validators.maxLength(64)]],
    role: [ApiTokenRole.ApiTokenViewer as ApiTokenRole, Validators.required],
    lifetimeDays: [API_TOKEN_LIFETIME_DEFAULT, Validators.required],
  });

  protected readonly busy = signal(false);
  protected readonly errorKey = signal<string | null>(null);
  protected readonly errorDetail = signal<string | null>(null);
  /** The issued secret; null until created and again once the dialog is gone. */
  protected readonly secret = signal<string | null>(null);
  protected readonly expiresAt = signal<string | null>(null);
  protected readonly copied = signal(false);

  protected async create(): Promise<void> {
    if (this.form.invalid || this.busy() || this.secret()) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    this.errorKey.set(null);
    this.errorDetail.set(null);
    const v = this.form.getRawValue();
    try {
      const res = await firstValueFrom(
        this.api.createApiToken({
          name: v.name.trim(),
          role: v.role,
          expiresAt: tokenExpiry(new Date(), v.lifetimeDays),
        }),
      );
      this.secret.set(res.token);
      this.expiresAt.set(res.expiresAt);
      this.form.disable();
    } catch (err) {
      const p = problemOf(err);
      if (p) {
        applyFieldErrors(this.form, p);
      }
      this.errorKey.set(apiTokenCreateErrorKey(p?.status));
      this.errorDetail.set(p?.detail ?? null);
    } finally {
      this.busy.set(false);
    }
  }

  protected onCopied(ok: boolean): void {
    this.copied.set(ok);
  }

  /** Closes with true once a token was issued, so the caller reloads the list. */
  protected close(): void {
    this.ref.close(this.secret() !== null);
  }

  ngOnDestroy(): void {
    this.secret.set(null);
    this.expiresAt.set(null);
  }
}

/** Opens the dialog; resolves to true when a token was issued. */
export async function openApiTokenDialog(dialog: MatDialog): Promise<boolean> {
  const ref = dialog.open<ApiTokenDialog, void, boolean>(ApiTokenDialog, {
    width: '560px',
    maxWidth: 'calc(100vw - 32px)',
    disableClose: true,
    autoFocus: 'first-tabbable',
    restoreFocus: true,
  });
  return (await firstValueFrom(ref.afterClosed())) === true;
}
