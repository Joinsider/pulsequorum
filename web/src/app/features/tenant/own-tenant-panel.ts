// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { TranslocoDirective } from '@jsverse/transloco';
import { TenantService } from '../../api/gen/endpoints/tenant/tenant.service';
import { OwnTenant } from '../../api/gen/model';
import { hintKey, usageRows, UsageRow } from './tenant-usage';

/**
 * Plan, limits and usage of the signed-in user's own tenant (FA-TEN-04). Shown on
 * the monitor list, where a reached limit matters. Hidden if the call fails: the
 * page works without it.
 */
@Component({
  selector: 'app-own-tenant-panel',
  imports: [TranslocoDirective],
  templateUrl: './own-tenant-panel.html',
  styleUrl: './own-tenant-panel.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class OwnTenantPanel {
  private readonly api = inject(TenantService);

  protected readonly tenant = signal<OwnTenant | null>(null);
  protected readonly rows = computed<UsageRow[]>(() => {
    const current = this.tenant();
    return current ? usageRows(current) : [];
  });
  protected readonly hintKey = hintKey;

  constructor() {
    this.api
      .getOwnTenant()
      .pipe(takeUntilDestroyed())
      .subscribe({
        next: (tenant) => this.tenant.set(tenant),
        error: () => this.tenant.set(null),
      });
  }
}
