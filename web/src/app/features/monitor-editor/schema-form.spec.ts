// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { FormArray, FormControl, FormGroup } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import {
  JsonSchema,
  MapEntry,
  OMITTED,
  SECRET_MASK,
  SecretControl,
  buildControl,
  controlToValue,
  fieldKind,
  isLockedKey,
  isSecretSchema,
  isStoredSecret,
  newArrayItem,
  newMapEntry,
  parseList,
  validationMessage,
} from './schema-form';

/** Shaped like the HTTP(S) checker schema (internal/checker/httpcheck). */
const HTTP_SCHEMA: JsonSchema = {
  type: 'object',
  additionalProperties: false,
  required: ['url'],
  properties: {
    url: { type: 'string', title: 'URL', format: 'uri', maxLength: 2048 },
    method: { type: 'string', enum: ['GET', 'HEAD', 'POST'], default: 'GET' },
    headers: {
      type: 'object',
      additionalProperties: { type: 'string', maxLength: 4096 },
    },
    auth: {
      type: 'object',
      additionalProperties: false,
      required: ['type'],
      properties: {
        type: { type: 'string', enum: ['basic', 'bearer'] },
        password: { type: 'string', writeOnly: true },
      },
    },
    maxResponseMs: { type: 'integer', minimum: 1 },
    followRedirects: { type: 'boolean', default: true },
    maxRedirects: { type: 'integer', minimum: 0, maximum: 30, default: 10 },
    expected: { type: 'array', items: { type: 'string', maxLength: 8 }, maxItems: 3 },
  },
};

/** Shaped like the HTTP checker after ADR 0015: header values and auth secrets are writeOnly. */
const SECRET_SCHEMA: JsonSchema = {
  type: 'object',
  required: ['url'],
  properties: {
    url: { type: 'string', format: 'uri' },
    headers: {
      type: 'object',
      additionalProperties: { type: 'string', maxLength: 4096, writeOnly: true },
    },
    tokens: { type: 'array', items: { type: 'string', writeOnly: true } },
    auth: {
      type: 'object',
      required: ['type'],
      properties: {
        type: { type: 'string', enum: ['basic', 'bearer'] },
        username: { type: 'string' },
        password: { type: 'string', writeOnly: true },
      },
    },
  },
};

describe('secrets (ADR 0015)', () => {
  it('recognises the mask and nothing else', () => {
    expect(isStoredSecret('***')).toBe(true);
    expect(isStoredSecret(SECRET_MASK)).toBe(true);
    expect(isStoredSecret('**')).toBe(false);
    expect(isStoredSecret('')).toBe(false);
    expect(isStoredSecret(undefined)).toBe(false);
  });

  it('detects writeOnly fixed names, map values and array items', () => {
    const headers = SECRET_SCHEMA.properties?.['headers'] ?? {};
    const tokens = SECRET_SCHEMA.properties?.['tokens'] ?? {};
    const password = SECRET_SCHEMA.properties?.['auth']?.properties?.['password'] ?? {};
    const url = SECRET_SCHEMA.properties?.['url'] ?? {};

    expect(isSecretSchema(password)).toBe(true);
    expect(isSecretSchema(headers.additionalProperties as JsonSchema)).toBe(true);
    expect(isSecretSchema(tokens.items as JsonSchema)).toBe(true);
    expect(isSecretSchema(url)).toBe(false);
  });

  it('builds a stored control for the mask and an entered one otherwise', () => {
    const stored = buildControl({ type: 'string', writeOnly: true }, '***', true);
    const typed = buildControl({ type: 'string', writeOnly: true }, '', false);

    expect(stored).toBeInstanceOf(SecretControl);
    expect((stored as SecretControl).mode).toBe('stored');
    expect(stored.value).toBe('***');
    expect((typed as SecretControl).mode).toBe('entered');
    expect(typed.value).toBe('');
  });

  it('submits the mask for an untouched stored secret, also in map and array rows', () => {
    const group = buildControl(
      SECRET_SCHEMA,
      {
        url: 'https://a.example',
        headers: { 'X-Api-Key': '***' },
        tokens: ['***'],
        auth: { type: 'basic', username: 'bob', password: '***' },
      },
      true,
    ) as FormGroup;

    expect(controlToValue(SECRET_SCHEMA, group, true)).toEqual({
      url: 'https://a.example',
      headers: { 'X-Api-Key': '***' },
      tokens: ['***'],
      auth: { type: 'basic', username: 'bob', password: '***' },
    });
  });

  it('sends a replaced secret as typed and keeps the other stored ones', () => {
    const group = buildControl(
      SECRET_SCHEMA,
      {
        url: 'https://a.example',
        headers: { 'X-Api-Key': '***' },
        auth: { type: 'basic', password: '***' },
      },
      true,
    ) as FormGroup;
    const password = group.get('auth')?.get('password') as SecretControl;

    password.reveal();
    expect(password.mode).toBe('entered');
    expect(password.value).toBe('');
    password.setValue('n3w');

    const value = controlToValue(SECRET_SCHEMA, group, true) as Record<string, any>;
    expect(value['auth']['password']).toBe('n3w');
    expect(value['headers']).toEqual({ 'X-Api-Key': '***' });
  });

  it('sends an empty string for a removed optional secret and restores the mask on cancel', () => {
    const group = buildControl(
      SECRET_SCHEMA,
      { url: 'https://a.example', auth: { type: 'basic', password: '***' } },
      true,
    ) as FormGroup;
    const password = group.get('auth')?.get('password') as SecretControl;

    password.remove();
    expect(password.mode).toBe('removed');
    expect(controlToValue(SECRET_SCHEMA, group, true)).toMatchObject({
      auth: { type: 'basic', password: '' },
    });

    password.restore();
    expect(password.mode).toBe('stored');
    expect(controlToValue(SECRET_SCHEMA, group, true)).toMatchObject({
      auth: { password: '***' },
    });
  });

  it('does not offer removal for required secrets, map values or array items', () => {
    const group = buildControl(
      SECRET_SCHEMA,
      { url: 'https://a.example', headers: { 'X-Api-Key': '***' } },
      true,
    ) as FormGroup;
    const value = (group.get('headers') as FormArray).at(0).get('value') as SecretControl;

    value.remove();
    expect(value.mode).toBe('stored');
    expect(controlToValue(SECRET_SCHEMA, group, true)).toMatchObject({
      headers: { 'X-Api-Key': '***' },
    });
  });

  it('never restores a mask on a control that was not stored', () => {
    const control = buildControl({ type: 'string', writeOnly: true }, '', false) as SecretControl;
    control.restore();
    control.remove();
    expect(control.mode).toBe('entered');
    expect(control.value).toBe('');

    const stored = buildControl({ type: 'string', writeOnly: true }, '***', true) as SecretControl;
    stored.remove();
    expect(stored.mode).toBe('stored');
  });

  it('omits an empty optional replacement and rejects an empty required one', () => {
    const form = buildControl(SECRET_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    const auth = form.get('auth') as FormGroup;
    auth.enable();
    auth.get('type')?.setValue('basic');
    (auth.get('password') as SecretControl).reveal();
    expect(controlToValue(SECRET_SCHEMA, form, true)).not.toHaveProperty('auth.password');

    const entry = newMapEntry(SECRET_SCHEMA.properties?.['headers'] ?? {});
    entry.get('key')?.setValue('X-Api-Key');
    expect(entry.get('value')?.hasError('required')).toBe(true);
  });

  it('applies the text rules only while a secret is typed', () => {
    const control = buildControl(
      { type: 'string', writeOnly: true, maxLength: 4 },
      '***',
      true,
    ) as SecretControl;
    expect(control.valid).toBe(true);

    control.reveal();
    control.setValue('too long');
    expect(control.hasError('maxlength')).toBe(true);
  });

  it('locks the keys of stored secret entries only', () => {
    const headers = buildControl(
      SECRET_SCHEMA,
      { url: 'https://a.example', headers: { 'X-Api-Key': '***' } },
      true,
    ) as FormGroup;
    const stored = (headers.get('headers') as FormArray).at(0) as MapEntry;
    expect(stored).toBeInstanceOf(MapEntry);
    expect(isLockedKey(stored)).toBe(true);

    const added = newMapEntry(SECRET_SCHEMA.properties?.['headers'] ?? {});
    expect(isLockedKey(added)).toBe(false);

    const plain = buildControl(
      HTTP_SCHEMA,
      { url: 'https://a.example', headers: { 'X-A': '1' } },
      true,
    ) as FormGroup;
    expect(isLockedKey((plain.get('headers') as FormArray).at(0))).toBe(false);
  });

  it('builds new secret array items as typed values', () => {
    const item = newArrayItem(SECRET_SCHEMA.properties?.['tokens'] ?? {});
    expect(item).toBeInstanceOf(SecretControl);
    expect((item as SecretControl).mode).toBe('entered');
  });
});

describe('fieldKind', () => {
  it.each<[string, JsonSchema, string]>([
    ['plain string', { type: 'string' }, 'string'],
    ['write-only string', { type: 'string', writeOnly: true }, 'secret'],
    ['password format', { type: 'string', format: 'password' }, 'secret'],
    ['enum string', { type: 'string', enum: ['a', 'b'] }, 'enum'],
    ['integer', { type: 'integer' }, 'number'],
    ['number', { type: 'number' }, 'number'],
    ['boolean', { type: 'boolean' }, 'boolean'],
    ['object with properties', { type: 'object', properties: { a: { type: 'string' } } }, 'object'],
    ['object as map', { type: 'object', additionalProperties: { type: 'string' } }, 'map'],
    ['free object', { type: 'object', additionalProperties: true }, 'json'],
    ['array with items', { type: 'array', items: { type: 'string' } }, 'array'],
    ['array without items', { type: 'array' }, 'json'],
    ['nullable string', { type: ['string', 'null'] }, 'string'],
    ['type inferred from properties', { properties: { a: { type: 'string' } } }, 'object'],
  ])('classifies %s', (_name, schema, kind) => {
    expect(fieldKind(schema)).toBe(kind);
  });
});

describe('buildControl', () => {
  it('applies schema defaults on create', () => {
    const group = buildControl(HTTP_SCHEMA, undefined, true) as FormGroup;

    expect(group.controls['method'].value).toBe('GET');
    expect(group.controls['followRedirects'].value).toBe(true);
    expect(group.controls['maxRedirects'].value).toBe(10);
    expect(group.controls['url'].value).toBe('');
    expect(group.controls['maxResponseMs'].value).toBeNull();
  });

  it('disables optional objects without a value and enables them with one', () => {
    const absent = buildControl(HTTP_SCHEMA, undefined, true) as FormGroup;
    expect(absent.controls['auth'].disabled).toBe(true);

    const present = buildControl(
      HTTP_SCHEMA,
      { url: 'https://a.example', auth: { type: 'basic' } },
      true,
    ) as FormGroup;
    expect(present.controls['auth'].enabled).toBe(true);
    expect(present.controls['auth'].get('type')?.value).toBe('basic');
  });

  it('fills map and array controls from the stored value', () => {
    const group = buildControl(
      HTTP_SCHEMA,
      { url: 'https://a.example', headers: { 'X-A': '1' }, expected: ['200'] },
      true,
    ) as FormGroup;

    const headers = group.controls['headers'] as FormArray;
    expect(headers.length).toBe(1);
    expect(headers.at(0).get('key')?.value).toBe('X-A');
    expect(headers.at(0).get('value')?.value).toBe('1');
    expect((group.controls['expected'] as FormArray).length).toBe(1);
  });

  it('creates new array items and map entries from their schemas', () => {
    expect(newArrayItem(HTTP_SCHEMA.properties?.['expected'] ?? {}).value).toBe('');
    const entry = newMapEntry(HTTP_SCHEMA.properties?.['headers'] ?? {});
    expect(entry.get('key')?.value).toBe('');
    expect(entry.get('key')?.invalid).toBe(true);
  });

  it('uses null for a non-numeric initial number', () => {
    const control = buildControl({ type: 'integer' }, 'x', false);
    expect(control.value).toBeNull();
  });
});

describe('validators', () => {
  it('requires required leaves and skips empty optional ones', () => {
    const group = buildControl(HTTP_SCHEMA, undefined, true) as FormGroup;
    expect(group.controls['url'].hasError('required')).toBe(true);
    expect(group.controls['maxResponseMs'].valid).toBe(true);
  });

  it('checks the uri format', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'not a url' }, true) as FormGroup;
    expect(group.controls['url'].hasError('uri')).toBe(true);

    group.controls['url'].setValue('https://ok.example/path');
    expect(group.controls['url'].valid).toBe(true);
  });

  it('checks integer, minimum and maximum bounds', () => {
    const group = buildControl(HTTP_SCHEMA, undefined, true) as FormGroup;
    const redirects = group.controls['maxRedirects'];

    redirects.setValue(1.5);
    expect(redirects.hasError('integer')).toBe(true);
    redirects.setValue(31);
    expect(redirects.hasError('max')).toBe(true);
    redirects.setValue(-1);
    expect(redirects.hasError('min')).toBe(true);
    redirects.setValue(5);
    expect(redirects.valid).toBe(true);
  });

  it('enforces maxItems on arrays', () => {
    const group = buildControl(
      HTTP_SCHEMA,
      { url: 'https://a.example', expected: ['a', 'b', 'c', 'd'] },
      true,
    ) as FormGroup;
    expect(group.controls['expected'].hasError('maxItems')).toBe(true);
  });

  it('requires an entry key in map rows', () => {
    const entry = newMapEntry(HTTP_SCHEMA.properties?.['headers'] ?? {});
    entry.get('key')?.setValue('X-Trace');
    expect(entry.get('key')?.valid).toBe(true);
  });
});

describe('controlToValue', () => {
  it('omits empty optional values and keeps the required url', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    const value = controlToValue(HTTP_SCHEMA, group, true);

    expect(value).toEqual({
      url: 'https://a.example',
      method: 'GET',
      followRedirects: true,
      maxRedirects: 10,
    });
  });

  it('never sends disabled optional objects', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    expect(controlToValue(HTTP_SCHEMA, group, true)).not.toHaveProperty('auth');
  });

  it('sends an enabled optional object with its values', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    group.controls['auth'].enable();
    group.controls['auth'].get('type')?.setValue('bearer');
    group.controls['auth'].get('password')?.setValue('s3cret');

    expect(controlToValue(HTTP_SCHEMA, group, true)).toMatchObject({
      auth: { type: 'bearer', password: 's3cret' },
    });
  });

  it('drops empty map rows and keeps trimmed keys', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    const headers = group.controls['headers'] as FormArray;
    headers.push(newMapEntry(HTTP_SCHEMA.properties?.['headers'] ?? {}));
    headers.at(0).get('key')?.setValue('  X-A ');
    headers.at(0).get('value')?.setValue('1');
    headers.push(newMapEntry(HTTP_SCHEMA.properties?.['headers'] ?? {}));

    expect(controlToValue(HTTP_SCHEMA, group, true)).toMatchObject({ headers: { 'X-A': '1' } });
  });

  it('omits an empty optional map and an empty optional array', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    const value = controlToValue(HTTP_SCHEMA, group, true) as Record<string, unknown>;

    expect(value).not.toHaveProperty('headers');
    expect(value).not.toHaveProperty('expected');
  });

  it('sends a number only when it is set', () => {
    const group = buildControl(
      HTTP_SCHEMA,
      { url: 'https://a.example', maxResponseMs: 250 },
      true,
    ) as FormGroup;
    expect(controlToValue(HTTP_SCHEMA, group, true)).toMatchObject({ maxResponseMs: 250 });
  });

  it('sends booleans even when they hold their default', () => {
    const group = buildControl(HTTP_SCHEMA, { url: 'https://a.example' }, true) as FormGroup;
    group.controls['followRedirects'].setValue(false);
    expect(controlToValue(HTTP_SCHEMA, group, true)).toMatchObject({ followRedirects: false });
  });

  it('converts JSON text controls and omits blank ones', () => {
    const schema: JsonSchema = {
      type: 'object',
      properties: { extra: { type: 'object', additionalProperties: true } },
    };
    const control = new FormControl('{"a":1}');
    expect(controlToValue(schema.properties?.['extra'] ?? {}, control, false)).toEqual({ a: 1 });
    control.setValue('   ');
    expect(controlToValue(schema.properties?.['extra'] ?? {}, control, false)).toBe(OMITTED);
  });

  it('returns OMITTED for a disabled control', () => {
    const control = new FormControl('x');
    control.disable();
    expect(controlToValue({ type: 'string' }, control, true)).toBe(OMITTED);
  });
});

describe('validationMessage', () => {
  it('maps built-in errors to translation keys with parameters', () => {
    expect(validationMessage({ required: true })).toEqual({ key: 'monitorEditor.errors.required' });
    expect(validationMessage({ minlength: { requiredLength: 3, actualLength: 1 } })).toEqual({
      key: 'monitorEditor.errors.minLength',
      params: { min: 3 },
    });
    expect(validationMessage({ max: { max: 30, actual: 31 } })).toEqual({
      key: 'monitorEditor.errors.max',
      params: { max: 30 },
    });
  });

  it('passes custom error parameters through', () => {
    expect(validationMessage({ timeoutLimit: { max: 48 } })).toEqual({
      key: 'monitorEditor.errors.timeoutLimit',
      params: { max: 48 },
    });
  });

  it('shows server messages verbatim', () => {
    expect(validationMessage({ server: 'must be a public host' })).toEqual({
      text: 'must be a public host',
    });
  });

  it('returns null for valid controls', () => {
    expect(validationMessage(null)).toBeNull();
  });
});

describe('parseList', () => {
  it.each([
    ['', []],
    ['a', ['a']],
    [' a , b ,, c ', ['a', 'b', 'c']],
  ])('splits %j into %j', (text, expected) => {
    expect(parseList(text)).toEqual(expected);
  });
});
