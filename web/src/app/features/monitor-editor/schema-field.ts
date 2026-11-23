// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, inject, input } from '@angular/core';
import {
  AbstractControl,
  FormArray,
  FormControl,
  FormGroup,
  ReactiveFormsModule,
} from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { MatSelectModule } from '@angular/material/select';
import { MatSlideToggleModule } from '@angular/material/slide-toggle';
import { toObservable, toSignal } from '@angular/core/rxjs-interop';
import { TranslocoPipe, TranslocoService } from '@jsverse/transloco';
import { asapScheduler, observeOn, scan, switchMap } from 'rxjs';
import { Icon } from '../../shared/icon/icon';
import { PemField, isPemField } from './pem-field';
import {
  JsonSchema,
  SecretControl,
  asSchema,
  fieldKind,
  humanizeKey,
  isLockedKey,
  newArrayItem,
  newMapEntry,
  orderedProperties,
  validationMessage,
} from './schema-form';

/**
 * Renders one node of a JSON Schema bound to its control: a leaf input, a
 * fieldset for objects (recursive), a key/value list for maps and a repeatable
 * list for arrays.
 *
 * Texts, in order of precedence (`<prefix>` is `i18nPrefix`, `<path>` the dotted
 * property path, e.g. `auth.password`; both empty for the root):
 * - label: `<prefix>.<path>.label`, then `schema.title`, then the humanized key
 * - hint: `<prefix>.<path>.hint`, then `schema.description`
 * - enum option: `<prefix>.<path>.options.<value>`, then the raw value
 * Missing translations fall back silently; the labels follow the language at runtime.
 */
@Component({
  selector: 'app-schema-field',
  imports: [
    ReactiveFormsModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    MatSelectModule,
    MatSlideToggleModule,
    TranslocoPipe,
    Icon,
    PemField,
  ],
  templateUrl: './schema-field.html',
  styles: `
    :host {
      display: block;
      min-width: 0;
    }
    .full {
      width: 100%;
    }
    .mono {
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 0.875rem;
    }
    .group {
      border: 1px solid var(--border);
      border-radius: 0.5rem;
      padding: 0.75rem 1rem 0.5rem;
      margin: 0;
      min-width: 0;
      display: grid;
      gap: 0.25rem;
    }
    .group legend {
      padding: 0 0.375rem;
      font-weight: 600;
    }
    .hint {
      margin: 0.125rem 0 0.5rem;
      color: var(--muted);
      font-size: 0.8125rem;
      overflow-wrap: anywhere;
    }
    .list {
      display: grid;
      gap: 0.5rem;
      min-width: 0;
      margin: 0;
      padding: 0;
      border: 0;
    }
    .list-title {
      font-weight: 600;
      font-size: 0.9375rem;
    }
    .row {
      display: flex;
      flex-wrap: wrap;
      align-items: flex-start;
      gap: 0.5rem;
      min-width: 0;
    }
    .row > .fill {
      flex: 1 1 12rem;
      min-width: 0;
    }
    .row > .entry-key {
      flex: 0 1 9rem;
      min-width: 0;
    }
    .toggle {
      margin: 0.25rem 0;
    }
    .secret {
      display: grid;
      gap: 0.25rem;
      padding: 0.25rem 0 0.5rem;
      min-width: 0;
      margin: 0;
      border: 0;
    }
    .secret-title {
      color: var(--muted);
      font-size: 0.75rem;
    }
    .secret-state {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
    }
    .secret-actions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
    }
    .error-text {
      color: var(--danger);
      margin: 0;
      font-size: 0.8125rem;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class SchemaField {
  readonly schema = input.required<JsonSchema>();
  readonly control = input.required<AbstractControl>();
  /** Overrides the schema title, e.g. "#2" for list items. */
  readonly label = input<string>('');
  readonly required = input<boolean>(false);
  /** Translation key prefix of the monitor type's schema, e.g. `monitorEditor.schema.http`. */
  readonly i18nPrefix = input<string>('');
  /** Dotted property path below the prefix, e.g. `auth.password`; empty for the root. */
  readonly path = input<string>('');

  private readonly transloco = inject(TranslocoService);
  /**
   * Counts Transloco events (translation loads and language changes). The scope may
   * still be loading when this field is created, so the catalog is re-read on each event.
   */
  private readonly revision = toSignal(
    // Asynchronous: a pipe in the template can trigger a translation load while
    // rendering, and a synchronous emission would write this signal then (NG0600).
    this.transloco.events$.pipe(
      observeOn(asapScheduler),
      scan((n) => n + 1, 0),
    ),
    {
      initialValue: 0,
    },
  );

  /** Flat translations of the active language; re-read whenever a translation or the language changes. */
  private readonly catalog = computed<Record<string, unknown>>(() => {
    this.revision();
    return this.transloco.getTranslation(this.transloco.getActiveLang()) ?? {};
  });

  /** Translation key of this node without its suffix, or null without a prefix. */
  private readonly translationBase = computed(() => {
    const prefix = this.i18nPrefix();
    if (!prefix) {
      return null;
    }
    return this.path() ? `${prefix}.${this.path()}` : prefix;
  });

  /**
   * Looks a key up without going through Transloco's missing-key handler, so a
   * missing translation falls back silently instead of logging a warning.
   */
  private translated(key: string): string | null {
    const value = this.catalog()[key];
    // Read the flat catalog value directly: translate() may emit Transloco events,
    // which would write the revision signal while the template renders (NG0600).
    return typeof value === 'string' && value !== '' ? value : null;
  }

  protected readonly kind = computed(() => fieldKind(this.schema()));
  protected readonly title = computed(() => this.label() || this.labelText());
  protected readonly description = computed(() => {
    const base = this.translationBase();
    return (base && this.translated(`${base}.hint`)) || this.schema().description || '';
  });
  protected readonly leaf = computed(() => this.control() as FormControl);
  protected readonly group = computed(() => this.control() as FormGroup);
  protected readonly array = computed(() => this.control() as FormArray);
  /** Enum options with their labels; the raw value is shown without a translation. */
  protected readonly optionItems = computed(() => {
    const base = this.translationBase();
    return (this.schema().enum ?? []).map((value) => ({
      value,
      label: (base && this.translated(`${base}.options.${String(value)}`)) || String(value),
    }));
  });
  protected readonly isUri = computed(() => this.schema().format === 'uri');
  /** PEM text (certificate or key): edited with the file loader of {@link PemField}. */
  protected readonly pem = computed(() => isPemField(this.schema(), this.path()));

  /** Object properties in display order (x-order, else required first; composites last). */
  protected readonly properties = computed(() => orderedProperties(this.schema()));
  protected readonly itemSchema = computed(() => asSchema(this.schema().items));
  protected readonly valueSchema = computed(() => asSchema(this.schema().additionalProperties));

  /** Fallback label: the property key, humanized (`maxBodyBytes` -> "Max body bytes"). */
  private labelText(): string {
    const base = this.translationBase();
    const translated = base && this.translated(`${base}.label`);
    if (translated) {
      return translated;
    }
    if (this.schema().title) {
      return this.schema().title ?? '';
    }
    const segments = this.path().split('.');
    const last = segments[segments.length - 1] ?? '';
    return last ? humanizeKey(last) : '';
  }

  /** Path of an object property below this node. */
  protected childPath(key: string): string {
    return this.path() ? `${this.path()}.${key}` : key;
  }

  /** Re-reads the validation state whenever the control reports a change (OnPush-safe). */
  private readonly changes = toSignal(toObservable(this.control).pipe(switchMap((c) => c.events)), {
    initialValue: null,
  });

  protected readonly error = computed(() => {
    this.changes();
    return validationMessage(this.control().errors);
  });

  /** The secret control, or null for ordinary leaves. Re-evaluated on every change. */
  protected readonly secret = computed(() => {
    this.changes();
    const control = this.control();
    return control instanceof SecretControl ? control : null;
  });

  /** Mode of the secret (stored, entered, removed); null for ordinary leaves. */
  protected readonly secretMode = computed(() => {
    this.changes();
    const control = this.control();
    return control instanceof SecretControl ? control.mode : null;
  });

  protected enableGroup(): void {
    this.control().enable();
  }

  protected disableGroup(): void {
    this.control().disable();
  }

  protected addItem(): void {
    this.array().push(newArrayItem(this.schema()));
  }

  protected addEntry(): void {
    this.array().push(newMapEntry(this.schema()));
  }

  protected removeAt(index: number): void {
    this.array().removeAt(index);
  }

  protected entryKey(entry: AbstractControl): FormControl {
    return entry.get('key') as FormControl;
  }

  protected entryValue(entry: AbstractControl): AbstractControl {
    return entry.get('value') ?? entry;
  }

  protected lockedKey(entry: AbstractControl): boolean {
    return isLockedKey(entry);
  }
}
