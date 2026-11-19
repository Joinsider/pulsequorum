// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { ApiTokensService } from '../../api/gen/endpoints/api-tokens/api-tokens.service';
import { ApiToken } from '../../api/gen/model';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { RelativeTimePipe } from '../../shared/relative-time.pipe';
import { openApiTokenDialog } from './api-token-dialog';
import { apiTokenErrorKey, sortTokens } from './api-token-rules';

/** API token management (FA-AUTH-10, ADR 0031). Only reachable for admins (adminGuard). */
@Component({
  selector: 'app-api-tokens-page',
  imports: [DatePipe, MatButtonModule, Icon, PageHeader, RelativeTimePipe, TranslocoDirective],
  templateUrl: './api-tokens-page.html',
  styleUrl: './api-tokens-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ApiTokensPage {
  private readonly api = inject(ApiTokensService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  protected readonly tokens = signal<ApiToken[]>([]);
  protected readonly loading = signal(true);
  protected readonly loadError = signal<string | null>(null);
  protected readonly rows = computed(() => sortTokens(this.tokens()));

  constructor() {
    void this.load();
  }

  /** Reloads the tenant's tokens. Called on start, after changes and by the retry button. */
  protected async load(): Promise<void> {
    try {
      this.tokens.set(await firstValueFrom(this.api.listApiTokens()));
      this.loadError.set(null);
    } catch (err) {
      this.loadError.set(apiTokenErrorKey(err));
    } finally {
      this.loading.set(false);
    }
  }

  /** Opens the create dialog; the secret is shown there only, never on this page. */
  protected async create(): Promise<void> {
    if (await openApiTokenDialog(this.dialog)) {
      await this.load();
    }
  }

  protected async revoke(token: ApiToken): Promise<void> {
    const confirmed = await confirm(this.dialog, {
      titleKey: 'apiTokens.revokeTitle',
      messageKey: 'apiTokens.revokeText',
      confirmKey: 'apiTokens.revoke',
      danger: true,
    });
    if (!confirmed) {
      return;
    }
    try {
      await firstValueFrom(this.api.revokeApiToken(token.id));
      this.notify('apiTokens.revoked');
      await this.load();
    } catch (err) {
      this.notify(apiTokenErrorKey(err));
    }
  }

  private notify(key: string): void {
    this.snack.open(this.transloco.translate(key), undefined, { duration: 4000 });
  }
}
