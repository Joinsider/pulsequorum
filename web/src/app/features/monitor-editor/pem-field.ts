// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ChangeDetectionStrategy, Component, input, signal } from '@angular/core';
import { FormControl, ReactiveFormsModule } from '@angular/forms';
import { MatButtonModule } from '@angular/material/button';
import { MatFormFieldModule } from '@angular/material/form-field';
import { MatInputModule } from '@angular/material/input';
import { TranslocoPipe } from '@jsverse/transloco';
import { JsonSchema, ValidationMessage } from './schema-form';

/** Largest PEM file the browser reads (ADR 0042: the field holds at most 16384 characters). */
export const PEM_MAX_FILE_BYTES = 16 * 1024;

/** Marks the start of every PEM block; files without it are not PEM. */
const PEM_MARKER = '-----BEGIN';

/**
 * Whether a string schema holds PEM text and is edited with {@link PemField}: a
 * long string (`maxLength` of at least 4096) whose property name says it is a
 * certificate or key, e.g. `clientCertificate.certificate` or `privateKey`.
 */
export function isPemField(schema: JsonSchema, path: string): boolean {
  if (typeof schema.maxLength !== 'number' || schema.maxLength < 4096) {
    return false;
  }
  const name = path.split('.').pop() ?? '';
  return /certificate|private ?key|pem/i.test(name);
}

/**
 * Multi-line monospace input for PEM text with a "Datei laden" action. The file
 * is read in the browser with `File.text()`; nothing is uploaded. Files above
 * {@link PEM_MAX_FILE_BYTES} and files without a PEM block are rejected inline and
 * leave the field unchanged.
 */
@Component({
  selector: 'app-pem-field',
  imports: [
    ReactiveFormsModule,
    MatButtonModule,
    MatFormFieldModule,
    MatInputModule,
    TranslocoPipe,
  ],
  templateUrl: './pem-field.html',
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
    .actions {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
      margin: 0 0 0.5rem;
    }
    .error-text {
      color: var(--danger);
      margin: 0;
      font-size: 0.8125rem;
    }
  `,
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class PemField {
  /** Holds the PEM text; a stored secret is never passed in here (see SchemaField). */
  readonly control = input.required<FormControl<string>>();
  readonly label = input.required<string>();
  readonly hint = input<string>('');
  readonly required = input<boolean>(false);
  /** Validation error of the control, shown below the textarea. */
  readonly error = input<ValidationMessage | null>(null);

  /** Translation key of the last file error, or null. */
  protected readonly fileError = signal<string | null>(null);

  /** Reads the chosen file into the control, or sets {@link fileError}. */
  protected async onFile(event: Event): Promise<void> {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0] ?? null;
    // Clear the input so choosing the same file again fires a change event.
    input.value = '';
    if (!file) {
      return;
    }
    if (file.size > PEM_MAX_FILE_BYTES) {
      this.fileError.set('monitorEditor.pem.tooLarge');
      return;
    }
    const text = await file.text();
    if (!text.includes(PEM_MARKER)) {
      this.fileError.set('monitorEditor.pem.notPem');
      return;
    }
    this.fileError.set(null);
    const control = this.control();
    control.setValue(text.trim());
    control.markAsDirty();
    control.markAsTouched();
  }
}
