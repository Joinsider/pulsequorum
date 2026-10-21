// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  OnInit,
  signal,
} from '@angular/core';
import { takeUntilDestroyed, toSignal } from '@angular/core/rxjs-interop';
import { NonNullableFormBuilder, ReactiveFormsModule, Validators } from '@angular/forms';
import { MatAutocompleteModule } from '@angular/material/autocomplete';
import { MatButtonModule } from '@angular/material/button';
import { MatButtonToggleModule } from '@angular/material/button-toggle';
import { MatCheckboxModule } from '@angular/material/checkbox';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { MatSlideToggleModule } from '@angular/material/slide-toggle';
import { Router, RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom, merge } from 'rxjs';
import { MaintenanceService } from '../../api/gen/endpoints/maintenance/maintenance.service';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { Monitor, MaintenanceSchedule, MaintenanceWindow } from '../../api/gen/model';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { loadAllMonitors, MONITOR_PAGE_SIZE } from './monitor-options';
import { browserTimeZone, describeCron, listTimeZones, weekdayName } from './schedule';
import {
  cronValidator,
  durationValidator,
  flattenFieldPointers,
  fromMaintenanceWindow,
  onceEndValidator,
  onceStartValidator,
  PresetChoice,
  presetCron,
  presetDayValidator,
  presetTimeValidator,
  timeZoneValidator,
  toMaintenanceInput,
  WindowFormValue,
  WindowKind,
} from './window-form';

type FormKey = keyof WindowFormValue;

/** Validator error keys and their translation key under maintenance.errors. */
const ERROR_TEXTS: ReadonlyArray<readonly [string, string]> = [
  ['required', 'required'],
  ['datetime', 'datetime'],
  ['order', 'endBeforeStart'],
  ['cron', 'cron'],
  ['range', 'range'],
  ['time', 'time'],
  ['timezone', 'timezone'],
  ['maxlength', 'tooLong'],
];

/** Monday first, as most schedules are read that way. */
const WEEKDAY_ORDER = [1, 2, 3, 4, 5, 6, 0] as const;

/**
 * Creates or edits a maintenance window. One-time windows take start and end
 * in the chosen time zone; recurring windows take a cron expression, a duration
 * and a time zone. Server field errors (422) are shown on the matching fields.
 */
@Component({
  selector: 'app-maintenance-editor',
  imports: [
    ReactiveFormsModule,
    RouterLink,
    MatAutocompleteModule,
    MatButtonModule,
    MatButtonToggleModule,
    MatCheckboxModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    MatSlideToggleModule,
    PageHeader,
    Icon,
    TranslocoDirective,
  ],
  template: `
    <ng-container *transloco="let t">
      <app-page-header
        [title]="
          maintenanceId() ? t('maintenance.editor.editTitle') : t('maintenance.editor.newTitle')
        "
      >
        <a mat-button routerLink="/maintenance">
          <app-icon name="arrowBack" />
          {{ t('common.back') }}
        </a>
      </app-page-header>

      @if (loading()) {
        <p class="state" role="status">{{ t('common.loading') }}</p>
      } @else if (loadFailed()) {
        <div class="state" role="alert">
          <p>{{ t('maintenance.errors.load') }}</p>
          <button mat-stroked-button type="button" (click)="load()">{{ t('common.retry') }}</button>
        </div>
      } @else {
        <form class="form" [formGroup]="form" (ngSubmit)="save()" novalidate>
          @if (formError(); as message) {
            <p class="notice" role="alert">{{ message }}</p>
          }
          @if (droppedCount() > 0) {
            <p class="notice notice--info" role="status">
              {{ t('maintenance.editor.droppedMonitors', { count: droppedCount() }) }}
            </p>
          }

          <section class="section">
            <h2 class="section__title">{{ t('maintenance.editor.general') }}</h2>
            <mat-form-field appearance="outline">
              <mat-label>{{ t('maintenance.editor.name') }}</mat-label>
              <input matInput formControlName="name" maxlength="200" required />
              <mat-error>{{ errorText('name') }}</mat-error>
            </mat-form-field>
            <mat-form-field appearance="outline">
              <mat-label>{{ t('maintenance.editor.description') }}</mat-label>
              <textarea matInput formControlName="description" rows="3" maxlength="2000"></textarea>
              <mat-error>{{ errorText('description') }}</mat-error>
            </mat-form-field>
            <mat-slide-toggle formControlName="active">{{
              t('maintenance.editor.active')
            }}</mat-slide-toggle>
          </section>

          <section class="section">
            <h2 class="section__title">{{ t('maintenance.editor.monitors') }}</h2>
            <mat-checkbox formControlName="allMonitors">{{
              t('maintenance.editor.allMonitors')
            }}</mat-checkbox>
            @if (!allMonitors()) {
              <mat-form-field appearance="outline">
                <mat-label>{{ t('maintenance.editor.searchMonitors') }}</mat-label>
                <input
                  matInput
                  type="search"
                  autocomplete="off"
                  [value]="monitorQuery()"
                  (input)="onMonitorQuery($event)"
                />
              </mat-form-field>
              <p class="hint" role="status">
                {{ t('maintenance.editor.selectedCount', { count: selected().length }) }}
              </p>
              <div
                class="monitor-list"
                role="group"
                [attr.aria-label]="t('maintenance.editor.monitors')"
              >
                @for (m of visibleMonitors(); track m.id) {
                  <mat-checkbox
                    [checked]="isSelected(m.id)"
                    (change)="toggleMonitor(m.id, $event.checked)"
                  >
                    {{ m.name }}
                  </mat-checkbox>
                } @empty {
                  <p class="hint">{{ t('maintenance.editor.noMonitors') }}</p>
                }
              </div>
              @if (errorText('monitorIds'); as message) {
                <p class="field-error" role="alert">{{ message }}</p>
              }
            }
          </section>

          <section class="section">
            <h2 class="section__title">{{ t('maintenance.editor.schedule') }}</h2>
            <mat-button-toggle-group
              class="kind"
              formControlName="kind"
              [attr.aria-label]="t('maintenance.editor.kind')"
            >
              <mat-button-toggle value="once">{{ t('maintenance.editor.once') }}</mat-button-toggle>
              <mat-button-toggle value="recurring">{{
                t('maintenance.editor.recurring')
              }}</mat-button-toggle>
            </mat-button-toggle-group>

            <mat-form-field appearance="outline">
              <mat-label>{{ t('maintenance.editor.zone') }}</mat-label>
              <input
                matInput
                formControlName="timezone"
                autocomplete="off"
                autocapitalize="none"
                spellcheck="false"
                [matAutocomplete]="zoneAuto"
              />
              <mat-autocomplete #zoneAuto="matAutocomplete">
                @for (z of zoneOptions(); track z) {
                  <mat-option [value]="z">{{ z }}</mat-option>
                }
              </mat-autocomplete>
              <mat-hint>{{ t('maintenance.editor.zoneHint') }}</mat-hint>
              <mat-error>{{ errorText('timezone') }}</mat-error>
            </mat-form-field>

            @if (kind() === 'once') {
              <div class="pair">
                <mat-form-field appearance="outline">
                  <mat-label>{{ t('maintenance.editor.startsAt') }}</mat-label>
                  <input matInput type="datetime-local" formControlName="startsAt" />
                  <mat-error>{{ errorText('startsAt') }}</mat-error>
                </mat-form-field>
                <mat-form-field appearance="outline">
                  <mat-label>{{ t('maintenance.editor.endsAt') }}</mat-label>
                  <input matInput type="datetime-local" formControlName="endsAt" />
                  <mat-error>{{ errorText('endsAt') }}</mat-error>
                </mat-form-field>
              </div>
            } @else {
              <div class="pair">
                <mat-form-field appearance="outline">
                  <mat-label>{{ t('maintenance.editor.preset') }}</mat-label>
                  <mat-select formControlName="preset">
                    <mat-option value="custom">{{
                      t('maintenance.editor.presetCustom')
                    }}</mat-option>
                    <mat-option value="daily">{{ t('maintenance.editor.presetDaily') }}</mat-option>
                    <mat-option value="weekly">{{
                      t('maintenance.editor.presetWeekly')
                    }}</mat-option>
                    <mat-option value="monthly">{{
                      t('maintenance.editor.presetMonthly')
                    }}</mat-option>
                  </mat-select>
                </mat-form-field>
                @if (preset() !== 'custom') {
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('maintenance.editor.time') }}</mat-label>
                    <input matInput type="time" formControlName="time" />
                    <mat-error>{{ errorText('time') }}</mat-error>
                  </mat-form-field>
                }
                @if (preset() === 'weekly') {
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('maintenance.editor.weekday') }}</mat-label>
                    <mat-select formControlName="weekday">
                      @for (d of weekdays; track d) {
                        <mat-option [value]="d">{{ weekdayLabel(d) }}</mat-option>
                      }
                    </mat-select>
                  </mat-form-field>
                }
                @if (preset() === 'monthly') {
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('maintenance.editor.dayOfMonth') }}</mat-label>
                    <input
                      matInput
                      type="number"
                      min="1"
                      max="31"
                      step="1"
                      formControlName="dayOfMonth"
                    />
                    <mat-error>{{ errorText('dayOfMonth') }}</mat-error>
                  </mat-form-field>
                }
              </div>

              <mat-form-field appearance="outline">
                <mat-label>{{ t('maintenance.editor.cron') }}</mat-label>
                <input
                  matInput
                  formControlName="cron"
                  autocomplete="off"
                  autocapitalize="none"
                  spellcheck="false"
                />
                <mat-hint>{{ cronHint() ?? t('maintenance.editor.cronHint') }}</mat-hint>
                <mat-error>{{ errorText('cron') }}</mat-error>
              </mat-form-field>

              <mat-form-field appearance="outline">
                <mat-label>{{ t('maintenance.editor.duration') }}</mat-label>
                <input
                  matInput
                  type="number"
                  min="1"
                  max="10080"
                  step="1"
                  formControlName="durationMinutes"
                />
                <mat-hint>{{ t('maintenance.editor.durationHint') }}</mat-hint>
                <mat-error>{{ errorText('durationMinutes') }}</mat-error>
              </mat-form-field>
            }
          </section>

          <div class="actions">
            <a mat-button routerLink="/maintenance">{{ t('common.cancel') }}</a>
            <button mat-flat-button type="submit" [disabled]="saving()">
              {{ t('maintenance.editor.save') }}
            </button>
          </div>
        </form>
      }
    </ng-container>
  `,
  styles: `
    .state,
    .notice {
      margin: 1rem 0;
    }
    .state {
      display: flex;
      flex-direction: column;
      align-items: flex-start;
      gap: 0.75rem;
      color: var(--muted);
    }
    .notice {
      padding: 0.75rem 1rem;
      color: var(--danger);
      border: 1px solid var(--danger);
      border-radius: 8px;
    }
    .notice--info {
      color: var(--text);
      border-color: var(--border);
    }
    .form {
      display: flex;
      flex-direction: column;
      gap: 1.25rem;
      max-width: 48rem;
    }
    .section {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 1rem;
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: 12px;
    }
    .section__title {
      margin: 0 0 0.25rem;
      font-size: 1rem;
    }
    mat-form-field {
      width: 100%;
    }
    .kind {
      max-width: 100%;
    }
    .pair {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
      gap: 0 1rem;
    }
    .monitor-list {
      display: flex;
      flex-direction: column;
      max-height: 18rem;
      padding: 0.25rem 0.5rem;
      overflow: auto;
      border: 1px solid var(--border);
      border-radius: 8px;
    }
    .hint {
      margin: 0;
      font-size: 0.85rem;
      color: var(--muted);
    }
    .field-error {
      margin: 0;
      font-size: 0.85rem;
      color: var(--danger);
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 0.5rem;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class MaintenanceEditor implements OnInit {
  /** Route parameter; absent for a new window. */
  readonly maintenanceId = input<string>();

  private readonly api = inject(MaintenanceService);
  private readonly monitorsApi = inject(MonitorsService);
  private readonly router = inject(Router);
  private readonly transloco = inject(TranslocoService);
  private readonly fb = inject(NonNullableFormBuilder);

  protected readonly lang = toSignal(this.transloco.langChanges$, {
    initialValue: this.transloco.getActiveLang(),
  });
  private readonly browserZone = browserTimeZone();
  protected readonly zones = listTimeZones();
  protected readonly weekdays = WEEKDAY_ORDER;

  protected readonly loading = signal(true);
  protected readonly loadFailed = signal(false);
  protected readonly saving = signal(false);
  protected readonly formError = signal<string | null>(null);
  protected readonly droppedCount = signal(0);
  protected readonly monitors = signal<Monitor[]>([]);
  protected readonly monitorQuery = signal('');

  /** Schedule of the loaded window; its recurring bounds are kept on save. */
  private original: MaintenanceSchedule | null = null;

  protected readonly form = this.fb.group({
    name: ['', [Validators.required, Validators.maxLength(200)]],
    description: ['', [Validators.maxLength(2000)]],
    active: [true],
    allMonitors: [true],
    monitorIds: this.fb.control<string[]>([]),
    kind: this.fb.control<WindowKind>('once'),
    startsAt: ['', [onceStartValidator]],
    endsAt: ['', [onceEndValidator]],
    timezone: [this.browserZone, [timeZoneValidator]],
    cron: ['', [cronValidator]],
    durationMinutes: this.fb.control<number | null>(60, [durationValidator]),
    preset: this.fb.control<PresetChoice>('custom'),
    time: ['03:00', [presetTimeValidator]],
    weekday: [0],
    dayOfMonth: [1, [presetDayValidator]],
  });

  // Signals mirror form values that drive the template, so that OnPush views update.
  protected readonly allMonitors = toSignal(this.form.controls.allMonitors.valueChanges, {
    initialValue: this.form.controls.allMonitors.value,
  });
  protected readonly selected = toSignal(this.form.controls.monitorIds.valueChanges, {
    initialValue: this.form.controls.monitorIds.value,
  });
  protected readonly kind = toSignal(this.form.controls.kind.valueChanges, {
    initialValue: this.form.controls.kind.value,
  });
  protected readonly preset = toSignal(this.form.controls.preset.valueChanges, {
    initialValue: this.form.controls.preset.value,
  });
  private readonly cronValue = toSignal(this.form.controls.cron.valueChanges, {
    initialValue: this.form.controls.cron.value,
  });
  private readonly zoneQuery = toSignal(this.form.controls.timezone.valueChanges, {
    initialValue: this.form.controls.timezone.value,
  });

  /** Plain-language reading of the cron expression, if it is a known pattern. */
  protected readonly cronHint = computed(() => {
    const expr = this.cronValue();
    const lang = this.lang();
    return describeCron(
      expr,
      (key, params) => this.transloco.translate(key, params),
      (day) => weekdayName(lang, day),
    );
  });

  protected readonly zoneOptions = computed(() => {
    const query = (this.zoneQuery() ?? '').trim().toLowerCase();
    return this.zones.filter((z) => z.toLowerCase().includes(query)).slice(0, 100);
  });

  protected readonly visibleMonitors = computed(() => {
    const query = this.monitorQuery().trim().toLowerCase();
    const all = this.monitors();
    return query ? all.filter((m) => m.name.toLowerCase().includes(query)) : all;
  });

  constructor() {
    this.refreshValidity();
    const c = this.form.controls;

    c.kind.valueChanges.pipe(takeUntilDestroyed()).subscribe(() => this.refreshValidity());
    c.startsAt.valueChanges
      .pipe(takeUntilDestroyed())
      .subscribe(() => c.endsAt.updateValueAndValidity());
    c.timezone.valueChanges.pipe(takeUntilDestroyed()).subscribe(() => {
      c.startsAt.updateValueAndValidity();
      c.endsAt.updateValueAndValidity();
    });
    // A preset writes its expression into the cron field.
    merge(
      c.preset.valueChanges,
      c.time.valueChanges,
      c.weekday.valueChanges,
      c.dayOfMonth.valueChanges,
    )
      .pipe(takeUntilDestroyed())
      .subscribe(() => this.applyPreset());
    // Editing the expression by hand switches the preset to custom.
    c.cron.valueChanges.pipe(takeUntilDestroyed()).subscribe(() => this.detectCustom());
  }

  ngOnInit(): void {
    void this.load();
  }

  /** Loads all monitors and, when editing, the window. Retries after an error. */
  protected async load(): Promise<void> {
    this.loading.set(true);
    this.loadFailed.set(false);
    const id = this.maintenanceId();
    try {
      const [monitors, existing] = await Promise.all([
        loadAllMonitors((cursor) =>
          firstValueFrom(this.monitorsApi.listMonitors({ cursor, limit: MONITOR_PAGE_SIZE })),
        ),
        id ? firstValueFrom(this.api.getMaintenanceWindow(id)) : Promise.resolve(null),
      ]);
      this.monitors.set(monitors);
      if (existing) {
        this.populate(existing);
      }
    } catch {
      this.loadFailed.set(true);
    } finally {
      this.loading.set(false);
    }
  }

  protected isSelected(id: string): boolean {
    return this.selected().includes(id);
  }

  protected toggleMonitor(id: string, checked: boolean): void {
    const rest = this.form.controls.monitorIds.value.filter((x) => x !== id);
    this.form.controls.monitorIds.setValue(checked ? [...rest, id] : rest);
  }

  protected onMonitorQuery(event: Event): void {
    this.monitorQuery.set((event.target as HTMLInputElement).value);
  }

  protected weekdayLabel(day: number): string {
    return weekdayName(this.lang(), day);
  }

  /** The first failing validator of a control as text, or an empty string. */
  protected errorText(name: FormKey): string {
    const control = this.form.get(name);
    const errors = control?.errors;
    if (!errors) {
      return '';
    }
    if (typeof errors['server'] === 'string') {
      return errors['server'];
    }
    const hit = ERROR_TEXTS.find(([code]) => errors[code]);
    return hit ? this.transloco.translate(`maintenance.errors.${hit[1]}`) : '';
  }

  protected async save(): Promise<void> {
    if (this.saving()) {
      return;
    }
    this.formError.set(null);
    if (this.form.invalid) {
      this.form.markAllAsTouched();
      this.formError.set(this.transloco.translate('maintenance.errors.validation'));
      return;
    }
    this.saving.set(true);
    try {
      const input = toMaintenanceInput(this.form.getRawValue(), this.original);
      const id = this.maintenanceId();
      if (id) {
        await firstValueFrom(this.api.updateMaintenanceWindow(id, input));
      } else {
        await firstValueFrom(this.api.createMaintenanceWindow(input));
      }
      await this.router.navigateByUrl('/maintenance');
    } catch (err) {
      this.formError.set(this.saveErrorText(err));
    } finally {
      this.saving.set(false);
    }
  }

  private saveErrorText(err: unknown): string {
    const problem = problemOf(err);
    if (problem?.status === 422) {
      const applied = applyFieldErrors(this.form, flattenFieldPointers(problem));
      if (applied > 0) {
        return this.transloco.translate('maintenance.errors.validation');
      }
      return problem.detail ?? problem.title;
    }
    if (problem?.status === 403) {
      return this.transloco.translate('maintenance.errors.forbidden');
    }
    return this.transloco.translate('common.error');
  }

  /** Fills the form from a loaded window. Monitors that no longer exist are dropped and counted. */
  private populate(w: MaintenanceWindow): void {
    this.original = w.schedule;
    const value = fromMaintenanceWindow(w, this.browserZone);
    const known = new Set(this.monitors().map((m) => m.id));
    const monitorIds = value.monitorIds.filter((id) => known.has(id));
    this.droppedCount.set(value.monitorIds.length - monitorIds.length);
    // Without emitEvent the preset would overwrite the stored cron while patching.
    this.form.patchValue({ ...value, monitorIds }, { emitEvent: false });
    this.refreshValidity();
    // Notify subscribers once the whole value is in place.
    this.form.controls.timezone.updateValueAndValidity();
    this.form.controls.cron.updateValueAndValidity();
    this.form.controls.monitorIds.updateValueAndValidity();
  }

  /** Re-runs the cross-field validators, e.g. after the kind changes. */
  private refreshValidity(): void {
    const c = this.form.controls;
    for (const control of [c.startsAt, c.endsAt, c.cron, c.durationMinutes, c.time, c.dayOfMonth]) {
      control.updateValueAndValidity({ emitEvent: false });
    }
  }

  private applyPreset(): void {
    this.refreshValidity();
    const cron = presetCron(this.form.getRawValue());
    if (cron !== null) {
      this.form.controls.cron.setValue(cron, { emitEvent: false });
    }
  }

  private detectCustom(): void {
    const value = this.form.getRawValue();
    if (value.preset === 'custom') {
      return;
    }
    if (presetCron(value) !== this.form.controls.cron.value) {
      this.form.controls.preset.setValue('custom', { emitEvent: false });
    }
  }
}
