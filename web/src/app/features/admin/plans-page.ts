// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  OnInit,
  computed,
  inject,
  signal,
} from '@angular/core';
import { MatButtonModule } from '@angular/material/button';
import { MatDialog } from '@angular/material/dialog';
import { MatSnackBar } from '@angular/material/snack-bar';
import { MatTooltipModule } from '@angular/material/tooltip';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom, Observable } from 'rxjs';
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { Plan } from '../../api/gen/model';
import { confirm } from '../../shared/confirm-dialog';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { failureDetail, failureKey } from './failure-rules';
import { LIMIT_KEYS, limitLabelKey } from './limit-rules';
import { openPlanDialog } from './plan-dialog';
import { PlanRights, planRights } from './plan-rules';

/** One list row: the plan and what the actor may do with it. */
export interface PlanRow {
  readonly plan: Plan;
  readonly rights: PlanRights;
}

/** An error of the last action, shown above the list. */
export interface PlanActionError {
  readonly key: string;
  readonly detail: string | null;
}

/**
 * Platform admin area "Plans" (ADR 0029): list with limits and tenant count;
 * create, edit and delete. A plan in use or `default` cannot be deleted.
 */
@Component({
  selector: 'app-plans-page',
  imports: [MatButtonModule, MatTooltipModule, Icon, PageHeader, TranslocoDirective],
  templateUrl: './plans-page.html',
  styleUrl: './plans-page.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class PlansPage implements OnInit {
  private readonly api = inject(TenantAdminService);
  private readonly dialog = inject(MatDialog);
  private readonly snack = inject(MatSnackBar);
  private readonly transloco = inject(TranslocoService);

  protected readonly limitKeys = LIMIT_KEYS;
  protected readonly labelKey = limitLabelKey;
  protected readonly plans = signal<Plan[]>([]);
  protected readonly loading = signal(true);
  /** Translation key of a failed list call, or null. */
  protected readonly loadError = signal<string | null>(null);
  protected readonly actionError = signal<PlanActionError | null>(null);
  /** IDs of plans with an action in flight. */
  protected readonly pending = signal<ReadonlySet<string>>(new Set());

  protected readonly rows = computed<PlanRow[]>(() =>
    this.plans().map((plan) => ({ plan, rights: planRights(plan) })),
  );

  ngOnInit(): void {
    void this.load();
  }

  /** Reloads the plans. Called on start, after changes and by the retry button. */
  protected async load(): Promise<void> {
    try {
      this.plans.set(await firstValueFrom(this.api.listPlans()));
      this.loadError.set(null);
    } catch (err) {
      this.loadError.set(failureKey(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    const saved = await openPlanDialog(this.dialog, {
      mode: 'create',
      takenIds: this.plans().map((plan) => plan.id),
    });
    if (saved) {
      this.notify('admin.plans.created');
      await this.load();
    }
  }

  protected async edit(row: PlanRow): Promise<void> {
    const saved = await openPlanDialog(this.dialog, {
      mode: 'edit',
      takenIds: this.plans().map((plan) => plan.id),
      plan: row.plan,
    });
    if (saved) {
      this.notify('admin.plans.updated');
      await this.load();
    }
  }

  protected async remove(row: PlanRow): Promise<void> {
    if (!row.rights.canDelete) {
      return;
    }
    const confirmed = await confirm(this.dialog, {
      titleKey: 'admin.plans.delete.title',
      messageKey: 'admin.plans.delete.text',
      confirmKey: 'admin.plans.delete.confirm',
      danger: true,
    });
    if (confirmed) {
      await this.run(row.plan.id, this.api.deletePlan(row.plan.id), 'admin.plans.deleted');
    }
  }

  protected isPending(id: string): boolean {
    return this.pending().has(id);
  }

  /** Runs the deletion, then reloads. A refusal (plan still in use) stays visible above the list. */
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
