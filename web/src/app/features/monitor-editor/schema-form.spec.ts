// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

import { FormArray, FormControl, FormGroup } from '@angular/forms';
import { describe, expect, it } from 'vitest';
import {
  JsonSchema,
  OMITTED,
  buildControl,
  controlToValue,
  fieldKind,
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
