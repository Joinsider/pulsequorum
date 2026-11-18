// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { HttpErrorResponse } from '@angular/common/http';
import { ChangeDetectionStrategy, Component, DestroyRef, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { ReactiveFormsModule } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatExpansionModule } from '@angular/material/expansion';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { MatSlideToggleModule } from '@angular/material/slide-toggle';
import { ActivatedRoute, ActivatedRouteSnapshot, Router } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { firstValueFrom, forkJoin, of } from 'rxjs';
import { MonitorsService } from '../../api/gen/endpoints/monitors/monitors.service';
import { NotificationsService } from '../../api/gen/endpoints/notifications/notifications.service';
import {
  AssignmentMode,
  ConsensusPolicy,
  Monitor,
  MonitorInput,
  MonitorType,
  NotificationChannel,
} from '../../api/gen/model';
import { AuthService } from '../../core/auth/auth.service';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import { PageHeader } from '../../shared/page-header';
import { SchemaField } from './schema-field';
import {
  createMonitorForm,
  monitorFormValue,
  monitorInputOf,
  newRequestKey,
  replaceConfig,
} from './monitor-form';
import { JsonSchema, asSchema, validationMessage } from './schema-form';

/** Summary shown above the form after a failed save. */
interface Summary {
  key: string;
  messages: string[];
}

/** Routes: `data.mode` is `create` or `edit`; the monitor id comes from the parent route. */
@Component({
  selector: 'app-monitor-editor-page',
  imports: [
    ReactiveFormsModule,
    MatButtonModule,
    MatExpansionModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    MatSlideToggleModule,
    TranslocoDirective,
    PageHeader,
    SchemaField,
  ],
  templateUrl: './monitor-editor-page.html',
  styles: `
    :host {
      display: block;
      max-width: 60rem;
    }
    .state {
      display: grid;
      justify-items: start;
      gap: 0.75rem;
      padding: 1.5rem 0;
      color: var(--muted);
    }
    .error-text {
      color: var(--danger);
      margin: 0;
    }
    .card {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: 0.75rem;
      padding: 1rem 1.25rem 0.5rem;
      margin-bottom: 1rem;
      min-width: 0;
    }
    .card h2 {
      margin: 0 0 0.75rem;
      font-size: 1.0625rem;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
      gap: 0 1rem;
      min-width: 0;
    }
    .span-all {
      grid-column: 1 / -1;
    }
    .full {
      width: 100%;
    }
    .toggle {
      margin: 0.25rem 0 1rem;
    }
    .hint {
      margin: -0.5rem 0 1rem;
      color: var(--muted);
      font-size: 0.8125rem;
    }
    .summary {
      border: 1px solid var(--danger);
      border-radius: 0.5rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      color: var(--danger);
    }
    .summary p {
      margin: 0;
    }
    .summary ul {
      margin: 0.5rem 0 0;
      padding-left: 1.25rem;
      color: var(--text);
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 0.5rem;
      margin-top: 1rem;
    }
    .requires {
      margin: 0 0 1rem;
      color: var(--muted);
      font-size: 0.875rem;
    }
    mat-expansion-panel {
      margin-bottom: 1rem;
    }
    @media (max-width: 480px) {
      .card {
        padding: 0.875rem 0.875rem 0.25rem;
      }
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class MonitorEditorPage {
  private readonly route = inject(ActivatedRoute);
  private readonly router = inject(Router);
  private readonly destroyRef = inject(DestroyRef);
  private readonly monitors = inject(MonitorsService);
  private readonly notifications = inject(NotificationsService);
  private readonly auth = inject(AuthService);
  private readonly transloco = inject(TranslocoService);

  protected readonly isEdit = this.route.snapshot.data['mode'] === 'edit';
  /** Monitor id of the edit route; the `:id` parameter belongs to the parent route. */
  private readonly monitorId = this.isEdit ? findParam(this.route.snapshot, 'id') : null;

  protected readonly canEdit = this.auth.canEdit;
  protected readonly form = createMonitorForm();

  protected readonly loadState = signal<'loading' | 'ready' | 'error'>('loading');
  protected readonly types = signal<MonitorType[]>([]);
  protected readonly channels = signal<NotificationChannel[]>([]);
  /** Schema of the selected monitor type; null if the type is unknown. */
  protected readonly typeSchema = signal<JsonSchema | null>(null);
  protected readonly capability = signal<string | null>(null);
  protected readonly busy = signal(false);
  /** Opened automatically when a collapsed advanced field is invalid on submit. */
  protected readonly advancedOpen = signal(false);
  protected readonly consensusPolicies: ConsensusPolicy[] = ['majority', 'any', 'all'];
  protected readonly assignmentModes: AssignmentMode[] = ['all', 'count', 'nodes'];
  protected readonly summary = signal<Summary | null>(null);

  /** Idempotency key of the current payload, reused for retries of the same submission. */
  private idempotencyKey: string | null = null;
  private idempotencyPayload: string | null = null;

  constructor() {
    this.load();
  }

  protected load(): void {
    this.loadState.set('loading');
    forkJoin({
      types: this.monitors.listMonitorTypes(),
      channels: this.notifications.listNotificationChannels(),
      monitor: this.monitorId ? this.monitors.getMonitor(this.monitorId) : of(null),
    })
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: ({ types, channels, monitor }) => {
          this.types.set(types);
          this.channels.set(channels);
          if (monitor) {
            this.fill(monitor);
          } else {
            this.form.patchValue(
              monitorFormValue({
                notificationChannelIds: channels.filter((c) => c.isDefault).map((c) => c.id),
              }),
            );
            const initialType = types.find((t) => t.type === 'http') ?? types[0];
            this.applyType(initialType?.type ?? '');
          }
          this.loadState.set('ready');
        },
        error: () => this.loadState.set('error'),
      });
  }

  /** Called when the user picks another type: the config resets to that type's defaults. */
  protected selectType(type: string | null): void {
    this.applyType(type ?? '');
  }

  /** Translated message of a common field's first validation error, or '' if valid. */
  protected messageOf(name: string): string {
    const message = validationMessage(this.form.get(name)?.errors ?? null);
    if (!message) {
      return '';
    }
    return message.text ?? this.transloco.translate(message.key ?? '', message.params);
  }

  protected async submit(): Promise<void> {
    if (this.busy() || !this.canEdit()) {
      return;
    }
    this.form.markAllAsTouched();
    const schema = this.typeSchema();
    if (this.form.invalid || !schema) {
      const advancedInvalid = ADVANCED_FIELDS.some((name) => this.form.get(name)?.invalid);
      if (advancedInvalid) {
        this.advancedOpen.set(true);
      }
      this.summary.set({
        key: schema ? 'monitorEditor.errors.invalidForm' : 'monitorEditor.errors.unknownType',
        messages: [],
      });
      return;
    }

    const input = monitorInputOf(this.form, schema);
    this.busy.set(true);
    this.summary.set(null);
    try {
      const saved = this.monitorId
        ? await firstValueFrom(this.monitors.updateMonitor(this.monitorId, input))
        : await firstValueFrom(
            this.monitors.createMonitor(input, {
              headers: { 'Idempotency-Key': this.keyFor(input) },
            }),
          );
      await this.router.navigate(['/monitors', saved.id]);
    } catch (err) {
      this.handleError(err);
    } finally {
      this.busy.set(false);
    }
  }

  protected cancel(): void {
    void this.router.navigate(this.monitorId ? ['/monitors', this.monitorId] : ['/']);
  }

  /** Same payload, same key: a retried submission never creates a second monitor (FA-CLU-06). */
  private keyFor(input: MonitorInput): string {
    const payload = JSON.stringify(input);
    if (this.idempotencyKey === null || payload !== this.idempotencyPayload) {
      this.idempotencyKey = newRequestKey();
      this.idempotencyPayload = payload;
    }
    return this.idempotencyKey;
  }

  private fill(monitor: Monitor): void {
    this.form.patchValue(monitorFormValue(monitor));
    this.applyType(monitor.type, monitor.config);
  }

  private applyType(type: string, config?: unknown): void {
    const selected = this.types().find((t) => t.type === type);
    const schema = selected ? asSchema(selected.configSchema) : null;
    this.capability.set(selected?.requiresCapability ?? null);
    this.typeSchema.set(schema);
    this.form.controls['type'].setValue(type);
    if (schema) {
      replaceConfig(this.form, schema, config);
    } else {
      replaceConfig(this.form, { type: 'object' }, config);
    }
  }

  private handleError(err: unknown): void {
    const problem = problemOf(err);
    if (problem?.errors?.length) {
      const messages: string[] = [];
      for (const fieldError of problem.errors) {
        const applied = applyFieldErrors(this.form, { ...problem, errors: [fieldError] });
        if (applied === 0) {
          messages.push(fieldError.message);
        }
      }
      this.summary.set({ key: 'monitorEditor.errors.validation', messages });
      return;
    }
    const status = err instanceof HttpErrorResponse ? err.status : 0;
    // The problem detail says why the server refused, e.g. a reached plan limit (ADR 0029).
    const detail = problem?.detail;
    this.summary.set({ key: statusKey(status), messages: detail ? [detail] : [] });
  }
}

/** Fields inside the collapsed "Advanced" panel. */
const ADVANCED_FIELDS = [
  'recoveryThreshold',
  'resendIntervalMinutes',
  'tags',
  'consensusPolicy',
  'assignmentCount',
  'assignmentNodes',
];

function statusKey(status: number): string {
  switch (status) {
    case 0:
      return 'monitorEditor.errors.offline';
    case 403:
      return 'monitorEditor.errors.forbidden';
    case 404:
      return 'monitorEditor.errors.notFound';
    default:
      return 'monitorEditor.errors.save';
  }
}

/** Looks up a route parameter on the snapshot or any parent (empty-path routes may not inherit it). */
function findParam(snapshot: ActivatedRouteSnapshot, name: string): string | null {
  let current: ActivatedRouteSnapshot | null = snapshot;
  while (current) {
    const value = current.paramMap.get(name);
    if (value) {
      return value;
    }
    current = current.parent;
  }
  return null;
}
