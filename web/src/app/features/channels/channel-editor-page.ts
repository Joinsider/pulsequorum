// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  ChangeDetectionStrategy,
  Component,
  computed,
  inject,
  input,
  OnInit,
  signal,
} from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import {
  FormArray,
  FormControl,
  FormGroup,
  NonNullableFormBuilder,
  ReactiveFormsModule,
  Validators,
} from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatCheckboxModule } from '@angular/material/checkbox';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatProgressSpinnerModule } from '@angular/material/progress-spinner';
import { MatSelectModule } from '@angular/material/select';
import { MatSlideToggleModule } from '@angular/material/slide-toggle';
import { Router, RouterLink } from '@angular/router';
import { TranslocoDirective, TranslocoService } from '@jsverse/transloco';
import { AuthService } from '../../core/auth/auth.service';
import { applyFieldErrors, problemOf } from '../../shared/problem';
import { Icon } from '../../shared/icon/icon';
import { PageHeader } from '../../shared/page-header';
import { ChannelApi, ChannelScope, channelListPath } from './channel-api';
import {
  NotificationChannel,
  NotificationChannelInput,
  NotificationEvent,
  NotificationKind,
  WebhookConfigMethod,
} from '../../api/gen/model';
import { buildHeaders, eventFlags, HeaderRow, selectedEvents } from './channel-payload';
import { failureText } from './error-text';
import {
  ALL_SERVICE_FIELD_NAMES,
  buildExpertUrl,
  buildShoutrrrUrl,
  SERVICE_FIELDS,
  SHOUTRRR_SERVICES,
  ShoutrrrChoice,
} from './shoutrrr-url';

type HeaderGroup = FormGroup<{
  key: FormControl<string>;
  value: FormControl<string>;
  serverKey: FormControl<string>;
  serverValue: FormControl<string>;
}>;

/** Creates or edits a notification channel (FA-UI-04, FA-UI-07). */
@Component({
  selector: 'app-channel-editor-page',
  imports: [
    ReactiveFormsModule,
    RouterLink,
    MatButtonModule,
    MatCheckboxModule,
    MatFormFieldModule,
    MatInputModule,
    MatProgressSpinnerModule,
    MatSelectModule,
    MatSlideToggleModule,
    Icon,
    PageHeader,
    TranslocoDirective,
  ],
  template: `
    <ng-container *transloco="let t">
      <app-page-header [title]="t(titleKey())" />

      @if (loading()) {
        <div class="state" role="status">
          <mat-spinner diameter="28" />
          <span>{{ t('common.loading') }}</span>
        </div>
      } @else if (loadError()) {
        <div class="state" role="alert">
          <p>{{ t('common.error') }}</p>
          <button mat-stroked-button type="button" (click)="load()">{{ t('common.retry') }}</button>
        </div>
      } @else {
        @if (!auth.canAdmin()) {
          <p class="notice" role="status">{{ t('channels.editor.readOnly') }}</p>
        }
        <form class="editor" [formGroup]="form" (ngSubmit)="submit()" novalidate>
          <mat-form-field appearance="outline">
            <mat-label>{{ t('channels.editor.name') }}</mat-label>
            <input matInput formControlName="name" required maxlength="200" autocomplete="off" />
            @if (form.controls.name.hasError('server')) {
              <mat-error>{{ form.controls.name.getError('server') }}</mat-error>
            } @else if (form.controls.name.hasError('required') && form.controls.name.touched) {
              <mat-error>{{ t('channels.editor.errors.name') }}</mat-error>
            }
          </mat-form-field>

          <mat-form-field appearance="outline">
            <mat-label>{{ t('channels.editor.kind') }}</mat-label>
            <mat-select formControlName="kind" required>
              <mat-option value="shoutrrr">{{ t('channels.kinds.shoutrrr') }}</mat-option>
              <mat-option value="webhook">{{ t('channels.kinds.webhook') }}</mat-option>
            </mat-select>
          </mat-form-field>

          @if (kind() === 'shoutrrr') {
            <section class="section" [attr.aria-label]="t('channels.editor.target')">
              <h2 class="section__title">{{ t('channels.editor.target') }}</h2>

              @if (existing() && !replacing()) {
                <p class="muted">{{ t('channels.editor.shoutrrr.keepHint') }}</p>
                <p class="masked">
                  <code>{{ existing()?.maskedUrl ?? '-' }}</code>
                </p>
                @if (auth.canAdmin()) {
                  <button mat-stroked-button type="button" (click)="startReplace()">
                    {{ t('channels.editor.shoutrrr.replace') }}
                  </button>
                }
              } @else {
                @if (existing()) {
                  <p class="muted">{{ t('channels.editor.shoutrrr.replaceHint') }}</p>
                }
                <mat-form-field appearance="outline">
                  <mat-label>{{ t('channels.editor.shoutrrr.service') }}</mat-label>
                  <mat-select [formControl]="shoutrrrChoice">
                    @for (s of services; track s) {
                      <mat-option [value]="s">{{ t('channels.editor.services.' + s) }}</mat-option>
                    }
                    <mat-option value="expert">{{
                      t('channels.editor.services.expert')
                    }}</mat-option>
                  </mat-select>
                </mat-form-field>

                @if (choice() === 'expert') {
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('channels.editor.fields.url') }}</mat-label>
                    <input
                      matInput
                      [formControl]="expertUrl"
                      required
                      autocomplete="off"
                      autocapitalize="none"
                      spellcheck="false"
                    />
                    <mat-hint>{{ t('channels.editor.shoutrrr.expertHint') }}</mat-hint>
                  </mat-form-field>
                } @else {
                  <div class="form-row">
                    @for (f of currentFields(); track f.name) {
                      @if (f.kind === 'select') {
                        <mat-form-field appearance="outline">
                          <mat-label>{{ t('channels.editor.fields.' + f.name) }}</mat-label>
                          <mat-select [formControl]="fieldControl(f.name)" [required]="f.required">
                            @for (o of f.options ?? []; track o) {
                              <mat-option [value]="o">{{
                                t('channels.editor.modes.' + o)
                              }}</mat-option>
                            }
                          </mat-select>
                        </mat-form-field>
                      } @else {
                        <mat-form-field appearance="outline">
                          <mat-label>{{ t('channels.editor.fields.' + f.name) }}</mat-label>
                          <input
                            matInput
                            [type]="f.kind === 'secret' ? 'password' : 'text'"
                            [formControl]="fieldControl(f.name)"
                            [required]="f.required"
                            autocomplete="off"
                            autocapitalize="none"
                            spellcheck="false"
                          />
                        </mat-form-field>
                      }
                    }
                  </div>
                }
                @if (shoutrrrError(); as e) {
                  <p class="error" role="alert">{{ e }}</p>
                }
              }
            </section>
          } @else {
            <section class="section" [attr.aria-label]="t('channels.editor.webhook.title')">
              <h2 class="section__title">{{ t('channels.editor.webhook.title') }}</h2>
              <div formGroupName="webhook" class="webhook">
                <div class="form-row">
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('channels.editor.webhook.url') }}</mat-label>
                    <input
                      matInput
                      type="url"
                      formControlName="url"
                      required
                      autocomplete="off"
                      autocapitalize="none"
                      spellcheck="false"
                    />
                    @if (form.controls.webhook.controls.url.hasError('server')) {
                      <mat-error>{{
                        form.controls.webhook.controls.url.getError('server')
                      }}</mat-error>
                    } @else if (
                      form.controls.webhook.controls.url.invalid &&
                      form.controls.webhook.controls.url.touched
                    ) {
                      <mat-error>{{ t('channels.editor.errors.url') }}</mat-error>
                    }
                  </mat-form-field>
                  <mat-form-field appearance="outline">
                    <mat-label>{{ t('channels.editor.webhook.method') }}</mat-label>
                    <mat-select formControlName="method">
                      <mat-option value="POST">POST</mat-option>
                      <mat-option value="PUT">PUT</mat-option>
                    </mat-select>
                  </mat-form-field>
                </div>

                <mat-form-field appearance="outline" class="wide">
                  <mat-label>{{ t('channels.editor.webhook.body') }}</mat-label>
                  <textarea
                    matInput
                    formControlName="bodyTemplate"
                    rows="6"
                    spellcheck="false"
                  ></textarea>
                  <mat-hint>{{ t('channels.editor.webhook.bodyHint') }}</mat-hint>
                </mat-form-field>

                <h3 class="subtitle">{{ t('channels.editor.webhook.headers') }}</h3>
                <p class="muted">{{ t('channels.editor.webhook.headersHint') }}</p>
                <div formArrayName="headers" class="headers">
                  @for (row of headers.controls; track row; let i = $index) {
                    <div class="header-row" [formGroupName]="i">
                      <mat-form-field appearance="outline">
                        <mat-label>{{ t('channels.editor.webhook.headerKey') }}</mat-label>
                        <input
                          matInput
                          formControlName="key"
                          autocomplete="off"
                          spellcheck="false"
                        />
                      </mat-form-field>
                      <mat-form-field appearance="outline">
                        <mat-label>{{ t('channels.editor.webhook.headerValue') }}</mat-label>
                        <input
                          matInput
                          type="password"
                          formControlName="value"
                          autocomplete="off"
                        />
                      </mat-form-field>
                      <button
                        mat-icon-button
                        type="button"
                        [attr.aria-label]="t('channels.editor.webhook.removeHeader')"
                        (click)="removeHeader(i)"
                      >
                        <app-icon name="close" />
                      </button>
                    </div>
                  }
                </div>
                <button mat-stroked-button type="button" (click)="addHeader()">
                  <app-icon name="add" size="1.125rem" />
                  {{ t('channels.editor.webhook.addHeader') }}
                </button>
              </div>
            </section>
          }

          @if (isSystem()) {
            <p class="muted">{{ t('channels.system.eventsHint') }}</p>
          } @else {
            <section class="section" [attr.aria-label]="t('channels.editor.events')">
              <h2 class="section__title">{{ t('channels.editor.events') }}</h2>
              <div class="events" formGroupName="events">
                @for (e of events; track e) {
                  <mat-checkbox [formControlName]="e">{{ t('channels.events.' + e) }}</mat-checkbox>
                }
              </div>
              <p class="muted">{{ t('channels.editor.eventsHint') }}</p>
            </section>

            <mat-slide-toggle formControlName="isDefault">{{
              t('channels.editor.isDefault')
            }}</mat-slide-toggle>
            <p class="muted">{{ t('channels.editor.isDefaultHint') }}</p>
          }

          @if (formError(); as e) {
            <p class="error" role="alert">{{ e }}</p>
          }

          <div class="actions">
            @if (auth.canAdmin()) {
              <button mat-flat-button type="submit" [disabled]="saving()">
                {{ t('common.save') }}
              </button>
            }
            <a mat-button [routerLink]="listPath()">{{ t('common.cancel') }}</a>
          </div>
        </form>
      }
    </ng-container>
  `,
  styles: `
    .state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.75rem;
      padding: 2rem 1rem;
      text-align: center;
    }
    .state p {
      margin: 0;
    }
    .notice {
      margin: 0;
      padding: 0.625rem 0.875rem;
      border-radius: 6px;
      background: var(--background);
      border: 1px solid var(--border);
    }
    .editor {
      display: flex;
      flex-direction: column;
      gap: 1rem;
      max-width: 44rem;
    }
    .editor mat-form-field {
      width: 100%;
    }
    .section {
      display: flex;
      flex-direction: column;
      gap: 1rem;
      padding-top: 1rem;
      border-top: 1px solid var(--border);
    }
    .section__title {
      margin: 0;
      font-size: 1.0625rem;
      font-weight: 650;
    }
    .subtitle {
      margin: 0;
      font-size: 0.9375rem;
      font-weight: 600;
    }
    .header-row {
      display: grid;
      gap: 1rem 0.5rem;
      grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)) auto;
      align-items: start;
    }
    .events {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem 1.25rem;
    }
    .muted {
      margin: 0;
      color: var(--muted);
      font-size: 0.875rem;
    }
    .masked code {
      display: block;
      padding: 0.5rem 0.625rem;
      border-radius: 4px;
      background: var(--background);
      overflow-wrap: anywhere;
    }
    .error {
      margin: 0;
      color: var(--danger);
    }
    .actions {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class ChannelEditorPage implements OnInit {
  /** Route parameter; absent when creating a channel. */
  readonly channelId = input<string>();
  /** Tenant channels, or the platform's system channels for platform admins (ADR 0025). */
  readonly scope = input<ChannelScope>('tenant');
  protected readonly isSystem = computed(() => this.scope() === 'system');
  protected readonly listPath = computed(() => channelListPath(this.scope()));
  protected readonly titleKey = computed(() => {
    if (this.channelId()) {
      return this.isSystem() ? 'channels.system.titleEdit' : 'channels.editor.titleEdit';
    }
    return this.isSystem() ? 'channels.system.titleNew' : 'channels.editor.titleNew';
  });

  protected readonly auth = inject(AuthService);
  private readonly api = inject(ChannelApi);
  private readonly router = inject(Router);
  private readonly transloco = inject(TranslocoService);
  private readonly fb = inject(NonNullableFormBuilder);

  protected readonly services = SHOUTRRR_SERVICES;
  protected readonly events: readonly NotificationEvent[] = [
    NotificationEvent.down,
    NotificationEvent.up,
    NotificationEvent.degraded,
    NotificationEvent.partial,
    NotificationEvent.flapping,
    NotificationEvent.channel_failed,
    NotificationEvent.probe_offline,
  ];

  protected readonly loading = signal(false);
  protected readonly loadError = signal(false);
  protected readonly saving = signal(false);
  protected readonly formError = signal<string | null>(null);
  protected readonly shoutrrrError = signal<string | null>(null);
  protected readonly existing = signal<NotificationChannel | null>(null);
  /** Existing Shoutrrr channels show the masked URL until the user asks to replace it. */
  protected readonly replacing = signal(false);

  protected readonly form = this.fb.group({
    name: this.fb.control('', [Validators.required, Validators.maxLength(200)]),
    kind: this.fb.control<NotificationKind>(NotificationKind.shoutrrr),
    events: this.fb.group({
      down: this.fb.control(true),
      up: this.fb.control(true),
      degraded: this.fb.control(false),
      partial: this.fb.control(false),
      flapping: this.fb.control(true),
      channel_failed: this.fb.control(true),
      probe_offline: this.fb.control(true),
    }),
    isDefault: this.fb.control(false),
    webhook: this.fb.group({
      url: this.fb.control('', [Validators.required, Validators.pattern(/^https?:\/\/\S+$/i)]),
      method: this.fb.control<string>(WebhookConfigMethod.POST),
      bodyTemplate: this.fb.control(''),
      headers: this.fb.array<HeaderGroup>([]),
    }),
  });

  protected readonly kindControl = this.form.controls.kind;
  protected readonly kind = toSignal(this.kindControl.valueChanges, {
    initialValue: this.kindControl.value,
  });
  protected readonly headers = this.form.controls.webhook.controls.headers;

  /** Guided Shoutrrr form state; the URL is built from these values on save only. */
  protected readonly shoutrrrChoice = this.fb.control<ShoutrrrChoice>('email');
  protected readonly expertUrl = this.fb.control('');
  protected readonly fieldValues = new FormGroup(
    Object.fromEntries(
      ALL_SERVICE_FIELD_NAMES.map((name) => [
        name,
        new FormControl<string>(name === 'mode' ? 'hook' : '', { nonNullable: true }),
      ]),
    ) as Record<string, FormControl<string>>,
  );
  protected readonly choice = toSignal(this.shoutrrrChoice.valueChanges, {
    initialValue: this.shoutrrrChoice.value,
  });
  protected readonly currentFields = computed(() => {
    const c = this.choice();
    return c === 'expert' ? [] : SERVICE_FIELDS[c];
  });

  constructor() {
    if (!this.auth.canAdmin()) {
      this.form.disable();
    }
  }

  /** Route inputs are set only after the constructor ran, so loading starts here. */
  ngOnInit(): void {
    this.load();
  }

  protected fieldControl(name: string): FormControl<string> {
    return this.fieldValues.controls[name];
  }

  protected load(): void {
    const id = this.channelId();
    if (!id) {
      return;
    }
    this.loading.set(true);
    this.loadError.set(false);
    this.api.get(this.scope(), id).subscribe({
      next: (channel) => {
        this.populate(channel);
        this.loading.set(false);
      },
      error: () => {
        this.loading.set(false);
        this.loadError.set(true);
      },
    });
  }

  protected startReplace(): void {
    this.replacing.set(true);
  }

  protected addHeader(): void {
    this.headers.push(this.headerGroup({ key: '', value: '', serverKey: '', serverValue: '' }));
  }

  protected removeHeader(index: number): void {
    this.headers.removeAt(index);
  }

  protected submit(): void {
    if (this.saving()) {
      return;
    }
    this.formError.set(null);
    this.shoutrrrError.set(null);
    this.form.markAllAsTouched();
    const raw = this.form.getRawValue();
    // System channels subscribe to the system event only (ADR 0025).
    const events = this.isSystem() ? [NotificationEvent.system] : selectedEvents(raw.events);
    if (this.form.controls.name.invalid) {
      this.form.controls.name.markAsTouched();
      return;
    }
    if (events.length === 0) {
      this.formError.set(this.transloco.translate('channels.editor.errors.noEvent'));
      return;
    }

    const input: NotificationChannelInput = {
      name: raw.name.trim(),
      kind: raw.kind,
      events,
      // System channels have no monitors to attach to, so they are never a default.
      isDefault: this.isSystem() ? undefined : raw.isDefault,
    };

    const ok =
      raw.kind === NotificationKind.shoutrrr
        ? this.applyShoutrrrTarget(input)
        : this.applyWebhookTarget(input);
    if (!ok) {
      return;
    }

    this.save(input);
  }

  private applyShoutrrrTarget(input: NotificationChannelInput): boolean {
    // Stored secrets are kept by omitting `url` on update; a new URL is needed on create.
    if (this.existing() && !this.replacing()) {
      return true;
    }
    const built = this.buildUrl();
    if (!built.ok) {
      this.shoutrrrError.set(built.message);
      return false;
    }
    input.url = built.url;
    return true;
  }

  private applyWebhookTarget(input: NotificationChannelInput): boolean {
    const headers = buildHeaders(this.headerRows());
    if (!headers.ok) {
      this.formError.set(this.transloco.translate(HEADER_ERROR_KEYS[headers.error]));
      return false;
    }
    const wh = this.form.controls.webhook.getRawValue();
    if (this.form.controls.webhook.invalid) {
      this.form.controls.webhook.markAllAsTouched();
      this.formError.set(this.transloco.translate('channels.editor.errors.url'));
      return false;
    }
    input.webhook = {
      url: wh.url.trim(),
      method:
        wh.method === WebhookConfigMethod.PUT ? WebhookConfigMethod.PUT : WebhookConfigMethod.POST,
      headers: headers.headers,
      bodyTemplate: wh.bodyTemplate.trim() || undefined,
    };
    return true;
  }

  private save(input: NotificationChannelInput): void {
    this.saving.set(true);
    const id = this.channelId();
    const request = id
      ? this.api.update(this.scope(), id, input)
      : this.api.create(this.scope(), input);
    request.subscribe({
      next: () => {
        this.saving.set(false);
        void this.router.navigateByUrl(this.listPath());
      },
      error: (err: unknown) => {
        this.saving.set(false);
        const problem = problemOf(err);
        const applied = problem ? applyFieldErrors(this.form, problem) : 0;
        if (applied === 0) {
          this.formError.set(failureText(err, this.transloco.translate('common.error')));
        }
      },
    });
  }

  /** Builds the Shoutrrr URL from the guided form or the expert field. */
  private buildUrl(): { ok: true; url: string } | { ok: false; message: string } {
    const choice = this.choice();
    const result =
      choice === 'expert'
        ? buildExpertUrl(this.expertUrl.value)
        : buildShoutrrrUrl(choice, this.fieldPayload());
    if (result.ok) {
      return { ok: true, url: result.url };
    }
    const label = this.transloco.translate('channels.editor.fields.' + result.field);
    const key =
      result.error === 'missing'
        ? 'channels.editor.errors.missing'
        : 'channels.editor.errors.invalid';
    return { ok: false, message: this.transloco.translate(key, { field: label }) };
  }

  private fieldPayload(): Record<string, string> {
    return this.fieldValues.getRawValue();
  }

  private headerRows(): HeaderRow[] {
    return this.headers.controls.map((g) => g.getRawValue());
  }

  private headerGroup(row: HeaderRow): HeaderGroup {
    return this.fb.group({
      key: this.fb.control(row.key),
      value: this.fb.control(row.value),
      serverKey: this.fb.control(row.serverKey),
      serverValue: this.fb.control(row.serverValue),
    });
  }

  /** Fills the form from the server's channel. Secrets arrive masked and stay masked. */
  private populate(channel: NotificationChannel): void {
    this.existing.set(channel);
    this.replacing.set(false);
    this.form.controls.kind.setValue(channel.kind);
    this.form.controls.kind.disable();
    this.form.controls.name.setValue(channel.name);
    this.form.controls.events.setValue(eventFlags(channel.events));
    this.form.controls.isDefault.setValue(channel.isDefault);
    if (channel.webhook) {
      const wh = this.form.controls.webhook;
      wh.controls.url.setValue(channel.webhook.url);
      wh.controls.method.setValue(channel.webhook.method ?? WebhookConfigMethod.POST);
      wh.controls.bodyTemplate.setValue(channel.webhook.bodyTemplate ?? '');
      this.headers.clear();
      for (const [key, masked] of Object.entries(channel.webhook.headers ?? {})) {
        this.headers.push(
          this.headerGroup({ key, value: masked, serverKey: key, serverValue: masked }),
        );
      }
    }
    if (!this.auth.canAdmin()) {
      this.form.disable();
    }
  }
}

const HEADER_ERROR_KEYS: Record<'emptyKey' | 'emptyValue' | 'duplicateKey', string> = {
  emptyKey: 'channels.editor.errors.headerKey',
  emptyValue: 'channels.editor.errors.headerValue',
  duplicateKey: 'channels.editor.errors.headerDuplicate',
};
