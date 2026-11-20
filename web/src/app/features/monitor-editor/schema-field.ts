// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
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
import { TranslocoPipe } from '@jsverse/transloco';
import { switchMap } from 'rxjs';
import { Icon } from '../../shared/icon/icon';
import {
  JsonSchema,
  SecretControl,
  asSchema,
  fieldKind,
  isLockedKey,
  newArrayItem,
  newMapEntry,
  validationMessage,
} from './schema-form';

/**
 * Renders one node of a JSON Schema bound to its control: a leaf input, a
 * fieldset for objects (recursive), a key/value list for maps and a repeatable
 * list for arrays. Labels come from `title` (or `label`), hints from `description`.
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

  protected readonly kind = computed(() => fieldKind(this.schema()));
  protected readonly title = computed(() => this.label() || this.schema().title || '');
  protected readonly description = computed(() => this.schema().description ?? '');
  protected readonly leaf = computed(() => this.control() as FormControl);
  protected readonly group = computed(() => this.control() as FormGroup);
  protected readonly array = computed(() => this.control() as FormArray);
  protected readonly options = computed(() => (this.schema().enum ?? []) as unknown[]);
  protected readonly isUri = computed(() => this.schema().format === 'uri');

  protected readonly properties = computed(() => {
    const required = new Set(this.schema().required ?? []);
    return Object.entries(this.schema().properties ?? {}).map(([key, child]) => ({
      key,
      schema: asSchema(child),
      required: required.has(key),
    }));
  });
  protected readonly itemSchema = computed(() => asSchema(this.schema().items));
  protected readonly valueSchema = computed(() => asSchema(this.schema().additionalProperties));

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

  protected toggleGroup(enabled: boolean): void {
    if (enabled) {
      this.control().enable();
    } else {
      this.control().disable();
    }
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
