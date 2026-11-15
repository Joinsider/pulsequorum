// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject, OnDestroy, signal } from '@angular/core';
import { CdkCopyToClipboard } from '@angular/cdk/clipboard';
import { DatePipe } from '@angular/common';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import {
  MAT_DIALOG_DATA,
  MatDialog,
  MatDialogModule,
  MatDialogRef,
} from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { ClusterService } from '../../api/gen/endpoints/cluster/cluster.service';
import { Icon } from '../../shared/icon/icon';
import { problemOf } from '../../shared/problem';

/** Limits of `JoinTokenRequest.ttlMinutes` in api/openapi.yaml. */
export const JOIN_TOKEN_TTL_MIN = 1;
export const JOIN_TOKEN_TTL_MAX = 1440;
export const JOIN_TOKEN_TTL_DEFAULT = 60;

/** Input of the dialog. */
export interface JoinTokenDialogData {
  /** Cluster address of a voter the new node joins through (CLUSTER_JOIN), if known. */
  joinAddress?: string;
  /** Extra hint above the form, e.g. the missing failure reserve while a node is replaced. */
  hintKey?: string;
}

/**
 * Creates a one-time join token for a new voter and shows it exactly once
 * (FA-PKI-02). The token is a secret: it lives only in this dialog's signal,
 * is never logged, cached or put into storage or the URL, and is dropped when
 * the dialog closes. The server sends it with Cache-Control no-store.
 */
@Component({
  selector: 'app-join-token-dialog',
  imports: [
    ReactiveFormsModule,
    DatePipe,
    CdkCopyToClipboard,
    MatDialogModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    Icon,
    TranslocoDirective,
  ],
  templateUrl: './join-token-dialog.html',
  styleUrl: './join-token-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class JoinTokenDialog implements OnDestroy {
  private readonly api = inject(ClusterService);
  private readonly ref = inject<MatDialogRef<JoinTokenDialog, boolean>>(MatDialogRef);
  protected readonly data = inject<JoinTokenDialogData>(MAT_DIALOG_DATA, { optional: true }) ?? {};

  protected readonly form = inject(NonNullableFormBuilder).group({
    ttlMinutes: [
      JOIN_TOKEN_TTL_DEFAULT,
      [Validators.required, Validators.min(JOIN_TOKEN_TTL_MIN), Validators.max(JOIN_TOKEN_TTL_MAX)],
    ],
  });
  protected readonly ttlMin = JOIN_TOKEN_TTL_MIN;
  protected readonly ttlMax = JOIN_TOKEN_TTL_MAX;

  protected readonly busy = signal(false);
  protected readonly errorKey = signal<string | null>(null);
  protected readonly errorDetail = signal<string | null>(null);
  /** The issued token; null until created and again after the dialog closed. */
  protected readonly token = signal<string | null>(null);
  protected readonly expiresAt = signal<string | null>(null);
  protected readonly copied = signal(false);

  protected async create(): Promise<void> {
    if (this.form.invalid || this.busy() || this.token()) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    this.errorKey.set(null);
    this.errorDetail.set(null);
    try {
      const res = await firstValueFrom(
        this.api.createJoinToken({ role: 'voter', ttlMinutes: this.form.getRawValue().ttlMinutes }),
      );
      this.token.set(res.token);
      this.expiresAt.set(res.expiresAt);
      this.form.disable();
    } catch (err) {
      const p = problemOf(err);
      this.errorKey.set(joinTokenErrorKey(p?.status));
      this.errorDetail.set(p?.detail ?? null);
    } finally {
      this.busy.set(false);
    }
  }

  protected onCopied(ok: boolean): void {
    this.copied.set(ok);
  }

  /** Closes with true once a token was issued, so the caller can reload the node list. */
  protected close(): void {
    this.ref.close(this.token() !== null);
  }

  ngOnDestroy(): void {
    this.token.set(null);
    this.expiresAt.set(null);
  }
}

/** Translation key for a failed token request. */
export function joinTokenErrorKey(status: number | undefined): string {
  switch (status) {
    case 403:
      return 'admin.error.forbidden';
    case 422:
      return 'admin.joinToken.error.invalid';
    case 503:
      return 'admin.error.unavailable';
    default:
      return 'admin.error.generic';
  }
}

/**
 * Opens the dialog. Backdrop clicks and Escape do not close it, so a token
 * that was just shown cannot be lost by accident. Resolves to true when a
 * token was issued.
 */
export async function openJoinTokenDialog(
  dialog: MatDialog,
  data: JoinTokenDialogData = {},
): Promise<boolean> {
  const ref = dialog.open<JoinTokenDialog, JoinTokenDialogData, boolean>(JoinTokenDialog, {
    data,
    width: '560px',
    maxWidth: 'calc(100vw - 32px)',
    disableClose: true,
    autoFocus: 'first-tabbable',
  });
  return (await firstValueFrom(ref.afterClosed())) === true;
}
