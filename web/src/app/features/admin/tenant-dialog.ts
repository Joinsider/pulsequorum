// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import {
  AbstractControl,
  NonNullableFormBuilder,
  ReactiveFormsModule,
  Validators,
} from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatCheckboxModule } from '@angular/material/checkbox';
import {
  MAT_DIALOG_DATA,
  MatDialog,
  MatDialogModule,
  MatDialogRef,
} from '@angular/material/dialog';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { TranslocoDirective } from '@jsverse/transloco';
import { firstValueFrom } from 'rxjs';
import { TenantAdminService } from '../../api/gen/endpoints/tenant-admin/tenant-admin.service';
import { Plan, Tenant } from '../../api/gen/model';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import { newRequestKey } from '../monitor-editor/monitor-form';
import { failureDetail, failureKey } from './failure-rules';
import { DEFAULT_PLAN_ID } from './plan-rules';
import {
  LIMIT_BOUNDS,
  LIMIT_KEYS,
  LimitKey,
  LimitTexts,
  NOT_ENFORCED_YET,
  limitLabelKey,
  limitTextsOf,
  limitValidator,
  unlimitedOf,
} from './limit-rules';
import {
  ORG_ID_MAX_LENGTH,
  TENANT_ID_PATTERN,
  TENANT_NAME_MAX_LENGTH,
  TenantFormValues,
  isOrgEditable,
  tenantCreatePayload,
  tenantPatch,
} from './tenant-rules';

/** Input of the dialog: create a tenant, or edit one. */
export interface TenantDialogData {
  mode: 'create' | 'edit';
  /** Plans to choose from. */
  plans: readonly Plan[];
  tenant?: Tenant;
}

/**
 * Create or edit a tenant (FA-TEN-03, ADR 0029). An empty limit inherits the plan,
 * "Unlimited" sets null. Closes with `true` if the server saved a change.
 */
@Component({
  selector: 'app-tenant-dialog',
  imports: [
    ReactiveFormsModule,
    MatDialogModule,
    MatButtonModule,
    MatCheckboxModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    TranslocoDirective,
  ],
  templateUrl: './tenant-dialog.html',
  styleUrl: './tenant-dialog.scss',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class TenantDialog {
  private readonly api = inject(TenantAdminService);
  private readonly ref = inject<MatDialogRef<TenantDialog, boolean>>(MatDialogRef);
  private readonly fb = inject(NonNullableFormBuilder);
  protected readonly data = inject<TenantDialogData>(MAT_DIALOG_DATA);

  protected readonly create = this.data.mode === 'create';
  protected readonly tenant = this.data.tenant;
  protected readonly plans = this.data.plans;
  protected readonly limitKeys = LIMIT_KEYS;
  protected readonly bounds = LIMIT_BOUNDS;
  protected readonly labelKey = limitLabelKey;
  protected readonly notEnforced = NOT_ENFORCED_YET;
  /** The organization of `default` is set by the server configuration. */
  protected readonly orgEditable = this.tenant ? isOrgEditable(this.tenant.id) : true;
  protected readonly maxName = TENANT_NAME_MAX_LENGTH;
  protected readonly maxOrgId = ORG_ID_MAX_LENGTH;

  protected readonly busy = signal(false);
  /** Translation key of the failure, or null. */
  protected readonly error = signal<string | null>(null);
  protected readonly errorDetail = signal<string | null>(null);
  /** Limits that are unlimited (null) for this tenant. Their fields are disabled. */
  protected readonly unlimited = signal<Partial<Record<LimitKey, boolean>>>(
    unlimitedOf(this.tenant?.limits),
  );

  private readonly initialLimits: LimitTexts = limitTextsOf(this.tenant?.limits);

  protected readonly form = this.fb.group({
    id: this.fb.control({ value: '', disabled: !this.create }, [
      Validators.pattern(TENANT_ID_PATTERN),
    ]),
    name: this.fb.control(this.tenant?.name ?? '', [
      Validators.required,
      Validators.maxLength(TENANT_NAME_MAX_LENGTH),
    ]),
    plan: this.fb.control(this.tenant?.plan ?? DEFAULT_PLAN_ID, [Validators.required]),
    orgId: this.fb.control({ value: this.tenant?.orgId ?? '', disabled: !this.orgEditable }, [
      Validators.maxLength(ORG_ID_MAX_LENGTH),
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

  /** The plan selected in the form; drives the "Plan: …" hints of the limits. */
  private readonly selectedPlan = toSignal(this.form.controls.plan.valueChanges, {
    initialValue: this.form.controls.plan.value,
  });
  private readonly selectedPlanLimits = computed(
    () => this.plans.find((plan) => plan.id === this.selectedPlan())?.limits ?? null,
  );

  private idempotencyKey: string | null = null;
  private idempotencyPayload: string | null = null;

  constructor() {
    for (const key of LIMIT_KEYS) {
      if (this.unlimited()[key]) {
        this.limitControl(key).disable();
      }
    }
  }

  protected limitControl(key: LimitKey): AbstractControl {
    return this.form.controls.limits.controls[key];
  }

  /** The selected plan's own value for a limit: a number, null (unlimited) or undefined. */
  protected planLimit(key: LimitKey): number | null | undefined {
    return this.selectedPlanLimits()?.[key];
  }

  /** Checking "Unlimited" empties the field and disables it; the override then becomes null. */
  protected setUnlimited(key: LimitKey, checked: boolean): void {
    this.unlimited.update((flags) => ({ ...flags, [key]: checked }));
    const control = this.limitControl(key);
    if (checked) {
      control.setValue('');
      control.disable();
    } else {
      control.enable();
    }
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
    const values = this.values();
    try {
      if (this.create) {
        const body = tenantCreatePayload(values);
        await firstValueFrom(
          this.api.createTenant(body, {
            headers: { 'Idempotency-Key': this.keyFor(JSON.stringify(body)) },
          }),
        );
        this.ref.close(true);
        return;
      }
      if (!this.tenant) {
        this.ref.close(false);
        return;
      }
      const patch = tenantPatch(this.tenant, values);
      if (Object.keys(patch).length === 0) {
        this.ref.close(false);
        return;
      }
      await firstValueFrom(this.api.updateTenant(this.tenant.id, patch));
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

  private values(): TenantFormValues {
    const raw = this.form.getRawValue();
    return {
      id: raw.id,
      name: raw.name,
      plan: raw.plan,
      orgId: raw.orgId,
      limits: raw.limits,
      unlimited: this.unlimited(),
    };
  }

  /** Same key for retries of the same payload; a changed payload gets a new key (as in the monitor editor). */
  private keyFor(payload: string): string {
    if (this.idempotencyKey === null || payload !== this.idempotencyPayload) {
      this.idempotencyKey = newRequestKey();
      this.idempotencyPayload = payload;
    }
    return this.idempotencyKey;
  }
}

/** Opens the tenant dialog. Resolves to `true` if the server saved a change. */
export async function openTenantDialog(
  dialog: MatDialog,
  data: TenantDialogData,
): Promise<boolean> {
  const saved = await firstValueFrom(
    dialog
      .open<TenantDialog, TenantDialogData, boolean>(TenantDialog, {
        data,
        width: 'min(40rem, calc(100vw - 2rem))',
        autoFocus: 'first-tabbable',
        restoreFocus: true,
      })
      .afterClosed(),
    { defaultValue: false },
  );
  return saved === true;
}
