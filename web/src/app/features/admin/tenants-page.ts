// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom, forkJoin, Observable } from 'rxjs';
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { Plan, Tenant } from '../../api/gen/model';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { failureDetail, failureKey } from './failure-rules';
import { openTenantDialog } from './tenant-dialog';
import { TenantDeleteData, TenantDeleteDialog } from './tenant-delete-dialog';
import { TenantRights, statusKey, tenantRights } from './tenant-rules';

/** One list row: the tenant, its plan's name and what the actor may do with it. */
export interface TenantRow {
  readonly tenant: Tenant;
  readonly planName: string;
  readonly rights: TenantRights;
}

/** An error of the last action, shown above the list. */
export interface TenantActionError {
  readonly key: string;
  readonly detail: string | null;
}

/**
 * Platform admin area "Tenants" (FA-TEN-03, ADR 0029): list with plan, status,
 * usage against the effective limits and exceeded limits; create, edit, suspend,
 * resume and delete (only when suspended, after typing the name).
 */
@Component({
  selector: 'app-tenants-page',
  imports: [MatButtonModule, MatTooltipModule, Icon, PageHeader, TranslocoDirective],
  templateUrl: './tenants-page.html',
  styleUrl: './tenants-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class TenantsPage {
  private readonly api = inject(TenantAdminService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  protected readonly statusKey = statusKey;
  protected readonly tenants = signal<Tenant[]>([]);
  protected readonly plans = signal<Plan[]>([]);
  protected readonly loading = signal(true);
  /** Translation key of a failed list call, or null. */
  protected readonly loadError = signal<string | null>(null);
  protected readonly actionError = signal<TenantActionError | null>(null);
  /** IDs of tenants with an action in flight; their actions are disabled. */
  protected readonly pending = signal<ReadonlySet<string>>(new Set());

  protected readonly rows = computed<TenantRow[]>(() => {
    const planNames = new Map(this.plans().map((plan) => [plan.id, plan.name]));
    return this.tenants().map((tenant) => ({
      tenant,
      planName: planNames.get(tenant.plan) ?? tenant.plan,
      rights: tenantRights(tenant),
    }));
  });

  constructor() {
    void this.load();
  }

  /** Reloads tenants and plans. Called on start, after changes and by the retry button. */
  protected async load(): Promise<void> {
    try {
      const result = await firstValueFrom(
        forkJoin({ tenants: this.api.listTenants(), plans: this.api.listPlans() }),
      );
      this.tenants.set(result.tenants);
      this.plans.set(result.plans);
      this.loadError.set(null);
    } catch (err) {
      this.loadError.set(failureKey(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    const saved = await openTenantDialog(this.dialog, { mode: 'create', plans: this.plans() });
    if (saved) {
      this.notify('admin.tenants.created');
      await this.load();
    }
  }

  protected async edit(row: TenantRow): Promise<void> {
    const saved = await openTenantDialog(this.dialog, {
      mode: 'edit',
      plans: this.plans(),
      tenant: row.tenant,
    });
    if (saved) {
      this.notify('admin.tenants.updated');
      await this.load();
    }
  }

  protected async toggleSuspended(row: TenantRow): Promise<void> {
    if (row.tenant.suspended) {
      await this.run(row.tenant.id, this.api.resumeTenant(row.tenant.id), 'admin.tenants.resumed');
      return;
    }
    const confirmed = await confirm(this.dialog, {
      titleKey: 'admin.tenants.suspendDialog.title',
      messageKey: 'admin.tenants.suspendDialog.text',
      confirmKey: 'admin.tenants.suspendDialog.confirm',
      danger: true,
    });
    if (confirmed) {
      await this.run(
        row.tenant.id,
        this.api.suspendTenant(row.tenant.id),
        'admin.tenants.suspended',
      );
    }
  }

  /** Permanent deletion: the user must type the exact tenant name first (sent as confirmName). */
  protected async remove(row: TenantRow): Promise<void> {
    if (!row.rights.canDelete) {
      return;
    }
    const data: TenantDeleteData = { name: row.tenant.name };
    const confirmed = await firstValueFrom(
      this.dialog
        .open<TenantDeleteDialog, TenantDeleteData, boolean>(TenantDeleteDialog, {
          data,
          width: 'min(32rem, calc(100vw - 2rem))',
          role: 'alertdialog',
          autoFocus: 'dialog',
          restoreFocus: true,
        })
        .afterClosed(),
      { defaultValue: false },
    );
    if (confirmed === true) {
      await this.run(
        row.tenant.id,
        this.api.deleteTenant(row.tenant.id, { confirmName: row.tenant.name }),
        'admin.tenants.deleted',
      );
    }
  }

  protected isPending(id: string): boolean {
    return this.pending().has(id);
  }

  /** Runs one action for a tenant, then reloads the list. Failures stay visible above the list. */
  private async run(id: string, request: Observable<unknown>, doneKey: string): Promise<void> {
    this.actionError.set(null);
    this.setPending(id, true);
    try {
      await firstValueFrom(request);
      this.notify(doneKey);
      await this.load();
    } catch (err) {
      this.actionError.set({ key: failureKey(err), detail: failureDetail(err) });
    } finally {
      this.setPending(id, false);
    }
  }

  private setPending(id: string, active: boolean): void {
    this.pending.update((ids) => {
      const next = new Set(ids);
      if (active) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  }

  private notify(key: string): void {
    this.snack.open(this.transloco.translate(key), undefined, { duration: 4000 });
  }
}
