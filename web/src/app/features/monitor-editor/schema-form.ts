// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import {
  AbstractControl,
  FormArray,
  FormControl,
  FormControlOptions,
  FormGroup,
  ValidationErrors,
  ValidatorFn,
  Validators,
} from '@angular/forms';

/**
 * The subset of JSON Schema (draft 2020-12) that the checker plugins use
 * (see internal/checker/**). Unknown keywords are ignored.
 */
export interface JsonSchema {
  type?: string | string[];
  title?: string;
  description?: string;
  format?: string;
  enum?: unknown[];
  default?: unknown;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  pattern?: string;
  minItems?: number;
  maxItems?: number;
  items?: JsonSchema;
  properties?: Record<string, JsonSchema>;
  required?: string[];
  additionalProperties?: boolean | JsonSchema;
  writeOnly?: boolean;
}

/**
 * How a schema node is edited:
 * - string / secret (writeOnly or format password) / enum / number / boolean
 * - object: fixed properties; optional objects get an enable switch
 * - map: string-keyed entries with one value schema (e.g. HTTP headers)
 * - array: repeatable items
 * - json: anything else, edited as JSON text
 */
export type FieldKind =
  'string' | 'secret' | 'enum' | 'number' | 'boolean' | 'object' | 'map' | 'array' | 'json';

/** Marks a value that must not be sent because it is an empty optional value. */
export const OMITTED: unique symbol = Symbol('omitted');

/** Placeholder the API returns in place of a stored secret (ADR 0015). */
export const SECRET_MASK = '***';

/** True for the placeholder that stands for a secret the server keeps. */
export function isStoredSecret(value: unknown): boolean {
  return value === SECRET_MASK;
}

/** Whether a schema node is a secret (`writeOnly`), including map values and array items. */
export function isSecretSchema(schema: JsonSchema): boolean {
  return fieldKind(schema) === 'secret';
}

/**
 * - stored: the server holds the secret; it is shown as hidden and sent as the mask.
 * - entered: the user types a value (new monitors, or after "Replace").
 * - removed: sent as "" so the server deletes the secret (optional fields only).
 */
export type SecretMode = 'stored' | 'entered' | 'removed';

/**
 * Control of a secret leaf. Keeps the secret state next to the value: a stored
 * secret is never prefilled into an input, and only `entered` values are typed.
 */
export class SecretControl extends (FormControl as new (
  value: string,
  options: FormControlOptions & { nonNullable: true },
) => FormControl<string>) {
  mode: SecretMode;
  /** Loaded from the server with a stored secret, so {@link restore} can bring it back. */
  readonly stored: boolean;

  /**
   * @param removable false for required secrets and for map values and array items,
   *   which are removed with their entry or item instead.
   */
  constructor(
    initial: string,
    rules: ValidatorFn[],
    readonly removable: boolean,
  ) {
    super(initial, { nonNullable: true, validators: [secretRules(rules)] });
    this.stored = isStoredSecret(initial);
    this.mode = this.stored ? 'stored' : 'entered';
    this.updateValueAndValidity({ emitEvent: false });
  }

  /** "Replace": the stored secret is dropped from the view and a new value is typed. */
  reveal(): void {
    this.mode = 'entered';
    this.setValue('');
  }

  /** "Remove": the secret is deleted on save. Only for stored, optional secrets. */
  remove(): void {
    if (this.mode === 'stored' && this.removable) {
      this.mode = 'removed';
      this.setValue('');
    }
  }

  /** Undoes "Replace" or "Remove" of a stored secret. */
  restore(): void {
    if (this.stored) {
      this.mode = 'stored';
      this.setValue(SECRET_MASK);
    }
  }
}

/** Form row of a map entry. Entries loaded with a secret value keep their key (ADR 0015). */
export class MapEntry extends FormGroup {
  constructor(
    key: string,
    value: AbstractControl,
    /** Stored entries cannot be renamed; the user deletes the entry and adds a new one. */
    readonly keyLocked: boolean,
  ) {
    super({
      key: new FormControl(key, { nonNullable: true, validators: [Validators.required] }),
      value,
    });
  }
}

/** Whether the key of a map entry is read-only. */
export function isLockedKey(entry: AbstractControl): boolean {
  return entry instanceof MapEntry && entry.keyLocked;
}

/** Message of a validation error, ready for a translation lookup. */
export interface ValidationMessage {
  /** Full translation key (scope included), e.g. `monitorEditor.errors.required`. */
  key?: string;
  params?: Record<string, unknown>;
  /** Text from the server; shown verbatim, never translated. */
  text?: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Narrows an unknown JSON value to a schema object; anything else becomes `{}`. */
export function asSchema(raw: unknown): JsonSchema {
  return isRecord(raw) ? (raw as JsonSchema) : {};
}

/** The first non-null type, inferred from `properties` or `items` when `type` is missing. */
export function primaryType(schema: JsonSchema): string | undefined {
  const type = Array.isArray(schema.type) ? schema.type.find((t) => t !== 'null') : schema.type;
  if (type) {
    return type;
  }
  if (schema.properties) {
    return 'object';
  }
  if (schema.items) {
    return 'array';
  }
  return undefined;
}

export function fieldKind(schema: JsonSchema): FieldKind {
  if (Array.isArray(schema.enum) && schema.enum.length > 0) {
    return 'enum';
  }
  switch (primaryType(schema)) {
    case 'boolean':
      return 'boolean';
    case 'integer':
    case 'number':
      return 'number';
    case 'string':
      return schema.writeOnly || schema.format === 'password' ? 'secret' : 'string';
    case 'array':
      return schema.items ? 'array' : 'json';
    case 'object':
      if (schema.properties && Object.keys(schema.properties).length > 0) {
        return 'object';
      }
      if (isRecord(schema.additionalProperties)) {
        return 'map';
      }
      return 'json';
    default:
      return 'json';
  }
}

/** Creates the control for one schema node. `value` undefined means "use the schema default". */
export function buildControl(
  schema: JsonSchema,
  value: unknown,
  required: boolean,
): AbstractControl {
  const initial = value === undefined ? schema.default : value;
  switch (fieldKind(schema)) {
    case 'object':
      return buildObject(schema, initial, required);
    case 'map':
      return buildMap(schema, initial);
    case 'array':
      return buildArray(schema, initial, required);
    case 'boolean':
      return new FormControl<boolean>(initial === true, { nonNullable: true });
    case 'secret':
      return new SecretControl(
        typeof initial === 'string' ? initial : '',
        stringValidators(schema, required),
        !required,
      );
    case 'number':
      return new FormControl<number | null>(
        typeof initial === 'number' ? initial : null,
        numberValidators(schema, required),
      );
    case 'enum':
      return new FormControl<unknown>(initial ?? null, required ? [Validators.required] : []);
    case 'json':
      return new FormControl<string>(
        initial === undefined ? '' : JSON.stringify(initial, null, 2),
        [...(required ? [Validators.required] : []), jsonValidator],
      );
    default:
      return new FormControl<string>(typeof initial === 'string' ? initial : '', {
        nonNullable: true,
        validators: stringValidators(schema, required),
      });
  }
}

/** A new, empty item for an array schema (defaults of the item schema applied). */
export function newArrayItem(schema: JsonSchema): AbstractControl {
  return buildControl(asSchema(schema.items), undefined, true);
}

/** A new, empty key/value entry for a map schema. */
export function newMapEntry(schema: JsonSchema): MapEntry {
  return mapEntry('', buildControl(asSchema(schema.additionalProperties), undefined, true), false);
}

/**
 * Converts a control tree back to a JSON value. Empty optional values are
 * omitted (OMITTED) so the server applies its defaults. Disabled optional
 * objects are omitted too. Booleans are always sent.
 */
export function controlToValue(
  schema: JsonSchema,
  control: AbstractControl,
  required: boolean,
): unknown {
  if (control.disabled) {
    return OMITTED;
  }
  switch (fieldKind(schema)) {
    case 'object':
      return objectToValue(schema, control as FormGroup, required);
    case 'map':
      return mapToValue(schema, control as FormArray, required);
    case 'array': {
      const itemSchema = asSchema(schema.items);
      const items = (control as FormArray).controls
        .map((item) => controlToValue(itemSchema, item, true))
        .filter((v) => v !== OMITTED);
      return items.length === 0 && !required ? OMITTED : items;
    }
    case 'boolean':
      return control.value === true;
    case 'secret':
      if (control instanceof SecretControl) {
        return secretToValue(control, required);
      }
      return plainToValue(control, required);
    case 'number': {
      const v = control.value;
      return v === null || v === undefined || v === '' ? OMITTED : Number(v);
    }
    case 'json': {
      const raw = typeof control.value === 'string' ? control.value.trim() : '';
      if (raw === '') {
        return OMITTED;
      }
      try {
        return JSON.parse(raw) as unknown;
      } catch {
        return OMITTED;
      }
    }
    default:
      return plainToValue(control, required);
  }
}

/** Splits a comma-separated text into trimmed, non-empty entries. */
export function parseList(text: string): string[] {
  return text
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

/** Maps a validation error object to a translatable message (first match wins). */
export function validationMessage(errors: ValidationErrors | null): ValidationMessage | null {
  if (!errors) {
    return null;
  }
  if (typeof errors['server'] === 'string') {
    return { text: errors['server'] };
  }
  const [name, detail] = Object.entries(errors)[0] ?? [];
  if (!name) {
    return null;
  }
  const d = isRecord(detail) ? detail : {};
  switch (name) {
    case 'required':
      return { key: 'monitorEditor.errors.required' };
    case 'minlength':
      return { key: 'monitorEditor.errors.minLength', params: { min: d['requiredLength'] } };
    case 'maxlength':
      return { key: 'monitorEditor.errors.maxLength', params: { max: d['requiredLength'] } };
    case 'min':
      return { key: 'monitorEditor.errors.min', params: { min: d['min'] } };
    case 'max':
      return { key: 'monitorEditor.errors.max', params: { max: d['max'] } };
    case 'minItems':
      return { key: 'monitorEditor.errors.minItems', params: { min: d['min'] } };
    case 'maxItems':
      return { key: 'monitorEditor.errors.maxItems', params: { max: d['max'] } };
    case 'pattern':
    case 'uri':
    case 'integer':
    case 'json':
      return { key: `monitorEditor.errors.${name}` };
    default:
      // Custom validators of the monitor form, e.g. timeoutLimit with { max }.
      return { key: `monitorEditor.errors.${name}`, params: isRecord(detail) ? detail : undefined };
  }
}

function buildObject(schema: JsonSchema, initial: unknown, required: boolean): FormGroup {
  const requiredKeys = new Set(schema.required ?? []);
  const source = isRecord(initial) ? initial : {};
  const controls: Record<string, AbstractControl> = {};
  for (const [key, child] of Object.entries(schema.properties ?? {})) {
    controls[key] = buildControl(asSchema(child), source[key], requiredKeys.has(key));
  }
  const group = new FormGroup(controls);
  if (!required && initial === undefined) {
    group.disable({ emitEvent: false });
  }
  return group;
}

function buildMap(schema: JsonSchema, initial: unknown): FormArray {
  const valueSchema = asSchema(schema.additionalProperties);
  const entries = isRecord(initial) ? Object.entries(initial) : [];
  // Entries loaded from the server with secret values are stored: their keys stay fixed.
  const locked = isSecretSchema(valueSchema);
  return new FormArray(
    entries.map(([key, value]) => mapEntry(key, buildControl(valueSchema, value, true), locked)),
  );
}

function buildArray(schema: JsonSchema, initial: unknown, required: boolean): FormArray {
  const itemSchema = asSchema(schema.items);
  const items = Array.isArray(initial) ? initial : [];
  return new FormArray(
    items.map((item) => buildControl(itemSchema, item, true)),
    arrayValidators(schema, required),
  );
}

function mapEntry(key: string, value: AbstractControl, keyLocked: boolean): MapEntry {
  return new MapEntry(key, value, keyLocked);
}

/** Submitted value of a secret: the mask keeps it, "" removes it, typed text replaces it. */
function secretToValue(control: SecretControl, required: boolean): unknown {
  switch (control.mode) {
    case 'stored':
      return SECRET_MASK;
    case 'removed':
      return '';
    default:
      return control.value === '' && !required ? OMITTED : control.value;
  }
}

/** Submitted value of an ordinary leaf: empty required values are "", empty optional ones are omitted. */
function plainToValue(control: AbstractControl, required: boolean): unknown {
  const v = control.value;
  if (v === null || v === undefined || v === '') {
    return required ? '' : OMITTED;
  }
  return v;
}

/**
 * Validators of a secret while it is typed. A stored or removed secret holds the
 * mask or "" by design and skips the text rules.
 */
function secretRules(rules: ValidatorFn[]): ValidatorFn {
  const composed = Validators.compose(rules);
  return (control: AbstractControl) => {
    const mode = control instanceof SecretControl ? control.mode : undefined;
    if (mode === 'stored' || mode === 'removed') {
      return null;
    }
    return composed ? composed(control) : null;
  };
}

function objectToValue(schema: JsonSchema, group: FormGroup, required: boolean): unknown {
  const requiredKeys = new Set(schema.required ?? []);
  const out: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(schema.properties ?? {})) {
    const control = group.get(key);
    if (!control) {
      continue;
    }
    const value = controlToValue(asSchema(child), control, requiredKeys.has(key));
    if (value !== OMITTED) {
      out[key] = value;
    }
  }
  return Object.keys(out).length === 0 && !required ? OMITTED : out;
}

function mapToValue(schema: JsonSchema, array: FormArray, required: boolean): unknown {
  const valueSchema = asSchema(schema.additionalProperties);
  const out: Record<string, unknown> = {};
  for (const entry of array.controls) {
    const key = String(entry.get('key')?.value ?? '').trim();
    const valueControl = entry.get('value');
    if (key === '' || !valueControl) {
      continue;
    }
    const value = controlToValue(valueSchema, valueControl, true);
    if (value !== OMITTED) {
      out[key] = value;
    }
  }
  return Object.keys(out).length === 0 && !required ? OMITTED : out;
}

function stringValidators(schema: JsonSchema, required: boolean): ValidatorFn[] {
  const validators: ValidatorFn[] = [];
  if (required) {
    validators.push(Validators.required);
  }
  if (typeof schema.minLength === 'number') {
    validators.push(Validators.minLength(schema.minLength));
  }
  if (typeof schema.maxLength === 'number') {
    validators.push(Validators.maxLength(schema.maxLength));
  }
  if (typeof schema.pattern === 'string') {
    const regex = safeRegExp(schema.pattern);
    if (regex) {
      validators.push(Validators.pattern(regex));
    }
  }
  if (schema.format === 'uri') {
    validators.push(uriValidator);
  }
  return validators;
}

function numberValidators(schema: JsonSchema, required: boolean): ValidatorFn[] {
  const validators: ValidatorFn[] = [];
  if (required) {
    validators.push(Validators.required);
  }
  if (primaryType(schema) === 'integer') {
    validators.push(integerValidator);
  }
  if (typeof schema.minimum === 'number') {
    validators.push(Validators.min(schema.minimum));
  }
  if (typeof schema.maximum === 'number') {
    validators.push(Validators.max(schema.maximum));
  }
  return validators;
}

function arrayValidators(schema: JsonSchema, required: boolean): ValidatorFn[] {
  const validators: ValidatorFn[] = [];
  if (typeof schema.minItems === 'number') {
    const min = schema.minItems;
    validators.push((c) => {
      const n = Array.isArray(c.value) ? c.value.length : 0;
      return n === 0 && !required ? null : n < min ? { minItems: { min } } : null;
    });
  }
  if (typeof schema.maxItems === 'number') {
    const max = schema.maxItems;
    validators.push((c) => {
      const n = Array.isArray(c.value) ? c.value.length : 0;
      return n > max ? { maxItems: { max } } : null;
    });
  }
  return validators;
}

const integerValidator: ValidatorFn = (c) => {
  const v = c.value;
  if (v === null || v === undefined || v === '') {
    return null;
  }
  return Number.isInteger(Number(v)) ? null : { integer: true };
};

const uriValidator: ValidatorFn = (c) => {
  const v = c.value;
  if (typeof v !== 'string' || v === '') {
    return null;
  }
  try {
    return new URL(v).protocol ? null : { uri: true };
  } catch {
    return { uri: true };
  }
};

const jsonValidator: ValidatorFn = (c) => {
  const v = c.value;
  if (typeof v !== 'string' || v.trim() === '') {
    return null;
  }
  try {
    JSON.parse(v);
    return null;
  } catch {
    return { json: true };
  }
};

function safeRegExp(pattern: string): RegExp | null {
  try {
    return new RegExp(pattern);
  } catch {
    return null;
  }
}
