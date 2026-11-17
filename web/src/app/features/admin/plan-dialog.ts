// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
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
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { Plan } from '../../api/gen/model';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import { failureDetail, failureKey } from './failure-rules';
import {
  LIMIT_BOUNDS,
  LIMIT_KEYS,
  LimitKey,
  limitLabelKey,
  limitTextsOf,
  limitValidator,
} from './limit-rules';
import { PLAN_NAME_MAX_LENGTH, planChanged, planIdValidator, planInput } from './plan-rules';

/** Input of the dialog: create a plan (with the IDs already taken), or edit one. */
export interface PlanDialogData {
  mode: 'create' | 'edit';
  /** IDs of existing plans; a new plan must not reuse one of them. */
  takenIds: readonly string[];
  plan?: Plan;
}

/**
 * Create or edit a plan. An empty limit means unlimited. Closes with `true` if
 * the server saved a change.
 */
@Component({
  selector: 'app-plan-dialog',
  imports: [
    ReactiveFormsModule,
    MatDialogModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    TranslocoDirective,
  ],
  templateUrl: './plan-dialog.html',
  styleUrl: './plan-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class PlanDialog {
  private readonly api = inject(TenantAdminService);
  private readonly ref = inject<MatDialogRef<PlanDialog, boolean>>(MatDialogRef);
  private readonly fb = inject(NonNullableFormBuilder);
  protected readonly data = inject<PlanDialogData>(MAT_DIALOG_DATA);

  protected readonly create = this.data.mode === 'create';
  protected readonly plan = this.data.plan;
  protected readonly limitKeys = LIMIT_KEYS;
  protected readonly bounds = LIMIT_BOUNDS;
  protected readonly labelKey = limitLabelKey;
  protected readonly maxName = PLAN_NAME_MAX_LENGTH;

  protected readonly busy = signal(false);
  /** Translation key of the failure, or null. */
  protected readonly error = signal<string | null>(null);
  protected readonly errorDetail = signal<string | null>(null);

  private readonly initialLimits = limitTextsOf(this.plan?.limits);

  protected readonly form = this.fb.group({
    id: this.fb.control({ value: this.plan?.id ?? '', disabled: !this.create }, [
      planIdValidator(this.data.takenIds),
    ]),
    name: this.fb.control(this.plan?.name ?? '', [
      Validators.required,
      Validators.maxLength(PLAN_NAME_MAX_LENGTH),
    ]),
    limits: this.fb.group({
      maxMonitors: this.fb.control(this.initialLimits.maxMonitors, [limitValidator('maxMonitors')]),
      minIntervalSeconds: this.fb.control(this.initialLimits.minIntervalSeconds, [
        limitValidator('minIntervalSeconds'),
      ]),
      maxChannels: this.fb.control(this.initialLimits.maxChannels, [limitValidator('maxChannels')]),
      maxStatusPages: this.fb.control(this.initialLimits.maxStatusPages, [
        limitValidator('maxStatusPages'),
      ]),
      maxPrivateProbes: this.fb.control(this.initialLimits.maxPrivateProbes, [
        limitValidator('maxPrivateProbes'),
      ]),
    }),
  });

  protected limitControlError(key: LimitKey): boolean {
    const control = this.form.controls.limits.controls[key];
    return control.hasError('integer') || control.hasError('range');
  }

  protected async submit(): Promise<void> {
    if (this.busy()) {
      return;
    }
    this.error.set(null);
    this.errorDetail.set(null);
    if (this.form.invalid) {
      this.form.markAllAsTouched();
      return;
    }
    this.busy.set(true);
    const raw = this.form.getRawValue();
    const input = planInput({ id: raw.id, name: raw.name, limits: raw.limits });
    try {
      if (this.create) {
        await firstValueFrom(this.api.putPlan(raw.id.trim(), input));
        this.ref.close(true);
        return;
      }
      if (!this.plan || !planChanged(this.plan, input)) {
        this.ref.close(false);
        return;
      }
      await firstValueFrom(this.api.putPlan(this.plan.id, input));
      this.ref.close(true);
    } catch (err) {
      this.error.set(failureKey(err));
      this.errorDetail.set(failureDetail(err));
      const problem = problemOf(err);
      if (problem?.errors?.length) {
        applyFieldErrors(this.form, problem);
      }
    } finally {
      this.busy.set(false);
    }
  }
}

/** Opens the plan dialog. Resolves to `true` if the server saved a change. */
export async function openPlanDialog(dialog: MatDialog, data: PlanDialogData): Promise<boolean> {
  const saved = await firstValueFrom(
    dialog
      .open<PlanDialog, PlanDialogData, boolean>(PlanDialog, {
        data,
        width: 'min(36rem, calc(100vw - 2rem))',
        autoFocus: 'first-tabbable',
        restoreFocus: true,
      })
      .afterClosed(),
    { defaultValue: false },
  );
  return saved === true;
}
