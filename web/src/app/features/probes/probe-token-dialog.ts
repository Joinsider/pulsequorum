// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  OnDestroy,
  signal,
} from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { CdkCopyToClipboard } from '@angular/cdk/clipboard';
import { DatePipe } from '@angular/common';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog, MatDialogModule, MatDialogRef } from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { TranslocoDirective } from '@jsverse/transloco';
import { catchError, firstValueFrom, map, of } from 'rxjs';
import { ProbesService } from '../../api/gen/endpoints/probes/probes.service';
import { SystemService } from '../../api/gen/endpoints/system/system.service';
import { Icon } from '../../shared/icon/icon';
import { problemOf } from '../../shared/problem';

export const PROBE_TOKEN_TTL_MIN = 1;
export const PROBE_TOKEN_TTL_MAX = 1440;
export const PROBE_TOKEN_TTL_DEFAULT = 60;

/** Placeholder shown in the quick-start snippet when the server version is unknown. */
export const PROBE_IMAGE_TAG_PLACEHOLDER = '<version>';

const RELEASE_VERSION = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;

/**
 * Returns the tag of the probe image that matches the server version.
 * Development builds ("dev", empty or unknown) yield the placeholder, because
 * no image is published for them.
 */
export function probeImageTagFor(version: string | null | undefined): string {
  const v = (version ?? '').trim().replace(/^v/, '');
  return RELEASE_VERSION.test(v) ? v : PROBE_IMAGE_TAG_PLACEHOLDER;
}

/**
 * Issues a single-use private probe join token bound to the caller's tenant
 * (FA-PRB-05, ADR 0035). The token lives only in memory in this component's
 * signal and is cleared as soon as the dialog closes.
 */
@Component({
  selector: 'app-probe-token-dialog',
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
  templateUrl: './probe-token-dialog.html',
  styleUrl: './probe-token-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ProbeTokenDialog implements OnDestroy {
  private readonly api = inject(ProbesService);
  private readonly system = inject(SystemService);
  private readonly ref = inject<MatDialogRef<ProbeTokenDialog, boolean>>(MatDialogRef);

  /** Server version from the public health endpoint; null when it cannot be read. */
  private readonly serverVersion = toSignal(
    this.system.getHealth().pipe(
      map((health) => health.version),
      catchError(() => of(null)),
    ),
    { initialValue: null },
  );
  /** Tag of the probe image in the quick-start snippet, matching the server version. */
  protected readonly probeImageTag = computed(() => probeImageTagFor(this.serverVersion()));

  protected readonly form = inject(NonNullableFormBuilder).group({
    ttlMinutes: [
      PROBE_TOKEN_TTL_DEFAULT,
      [
        Validators.required,
        Validators.min(PROBE_TOKEN_TTL_MIN),
        Validators.max(PROBE_TOKEN_TTL_MAX),
      ],
    ],
  });
  protected readonly ttlMin = PROBE_TOKEN_TTL_MIN;
  protected readonly ttlMax = PROBE_TOKEN_TTL_MAX;

  protected readonly busy = signal(false);
  protected readonly errorKey = signal<string | null>(null);
  protected readonly errorDetail = signal<string | null>(null);
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
        this.api.createPrivateProbeJoinToken({ ttlMinutes: this.form.getRawValue().ttlMinutes }),
      );
      this.token.set(res.token);
      this.expiresAt.set(res.expiresAt);
      this.form.disable();
    } catch (err) {
      const p = problemOf(err);
      this.errorKey.set(probeTokenErrorKey(p?.status));
      this.errorDetail.set(p?.detail ?? null);
    } finally {
      this.busy.set(false);
    }
  }

  protected onCopied(ok: boolean): void {
    this.copied.set(ok);
  }

  protected close(): void {
    this.ref.close(this.token() !== null);
  }

  ngOnDestroy(): void {
    this.token.set(null);
    this.expiresAt.set(null);
  }
}

export function probeTokenErrorKey(status: number | undefined): string {
  switch (status) {
    case 403:
      return 'probes.tokenDialog.error.forbidden';
    case 422:
      return 'probes.tokenDialog.error.invalid';
    case 503:
      return 'probes.tokenDialog.error.unavailable';
    default:
      return 'probes.tokenDialog.error.generic';
  }
}

export async function openProbeTokenDialog(dialog: MatDialog): Promise<boolean> {
  const ref = dialog.open<ProbeTokenDialog, void, boolean>(ProbeTokenDialog, {
    width: '600px',
    maxWidth: 'calc(100vw - 32px)',
    disableClose: true,
    autoFocus: 'first-tabbable',
  });
  return (await firstValueFrom(ref.afterClosed())) === true;
}
