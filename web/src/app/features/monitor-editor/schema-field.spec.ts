// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

import { ComponentFixture, TestBed } from '@angular/core/testing';
import { TranslocoService, TranslocoTestingModule } from '@jsverse/transloco';
import { beforeEach, describe, expect, it } from 'vitest';
import { SchemaField } from './schema-field';
import { JsonSchema, buildControl, schemaI18nPrefix } from './schema-form';

const EN = {
  monitorEditor: {
    schema: {
      http: {
        url: { label: 'URL', hint: 'Full address' },
        maxBodyBytes: { label: 'Maximum response size (bytes)' },
        auth: {
          type: {
            label: 'Scheme',
            options: { basic: 'Basic' },
          },
        },
      },
    },
  },
};

const DE = {
  monitorEditor: {
    schema: {
      http: {
        url: { label: 'URL', hint: 'Vollständige Adresse' },
        maxBodyBytes: { label: 'Maximale Antwortgröße (Bytes)' },
        auth: {
          type: {
            label: 'Verfahren',
            options: { basic: 'Basic' },
          },
        },
      },
    },
  },
};

const PREFIX = schemaI18nPrefix('http');

describe('SchemaField texts', () => {
  let fixture: ComponentFixture<SchemaField>;
  let transloco: TranslocoService;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [
        TranslocoTestingModule.forRoot({
          langs: { en: EN, de: DE },
          translocoConfig: { availableLangs: ['en', 'de'], defaultLang: 'en' },
        }),
      ],
    });
    transloco = TestBed.inject(TranslocoService);
    transloco.setTranslation(EN, 'en');
    transloco.setTranslation(DE, 'de');
    transloco.setActiveLang('en');
  });

  function create(schema: JsonSchema, path: string, value?: unknown, required = false) {
    fixture = TestBed.createComponent(SchemaField);
    fixture.componentRef.setInput('schema', schema);
    fixture.componentRef.setInput('control', buildControl(schema, value, required));
    fixture.componentRef.setInput('required', required);
    fixture.componentRef.setInput('i18nPrefix', PREFIX);
    fixture.componentRef.setInput('path', path);
    fixture.detectChanges();
    return fixture.componentInstance;
  }

  it('takes the label from the translation key', () => {
    const field = create({ type: 'string', title: 'Schema title' }, 'url');
    expect(field['title']()).toBe('URL');
  });

  it('takes the hint from the translation key before the schema description', () => {
    const field = create({ type: 'string', description: 'Schema hint' }, 'url');
    expect(field['description']()).toBe('Full address');
  });

  it('falls back to the schema title when no translation exists', () => {
    const field = create({ type: 'integer', title: 'Schema title' }, 'maxRedirects');
    expect(field['title']()).toBe('Schema title');
  });

  it('falls back to the humanized key when neither translation nor title exists', () => {
    const field = create({ type: 'integer' }, 'maxRedirects');
    expect(field['title']()).toBe('Max redirects');
  });

  it('uses the nested path for translation lookups', () => {
    const field = create({ type: 'integer' }, 'maxBodyBytes');
    expect(field['title']()).toBe('Maximum response size (bytes)');
  });

  it('prefers an explicit label input over everything else', () => {
    fixture = TestBed.createComponent(SchemaField);
    fixture.componentRef.setInput('schema', { type: 'string', title: 'Schema title' });
    fixture.componentRef.setInput('control', buildControl({ type: 'string' }, undefined, false));
    fixture.componentRef.setInput('label', '#2');
    fixture.componentRef.setInput('i18nPrefix', PREFIX);
    fixture.componentRef.setInput('path', 'url');
    fixture.detectChanges();
    expect(fixture.componentInstance['title']()).toBe('#2');
  });

  it('translates enum option labels and keeps the raw value without a translation', () => {
    const field = create({ type: 'string', enum: ['basic', 'bearer'] }, 'auth.type');
    expect(field['optionItems']()).toEqual([
      { value: 'basic', label: 'Basic' },
      { value: 'bearer', label: 'bearer' },
    ]);
  });

  it('shows the enum label in the form field', () => {
    create({ type: 'string', enum: ['basic'] }, 'auth.type');
    const label = fixture.nativeElement.querySelector('mat-label');
    expect(label?.textContent?.trim()).toBe('Scheme');
  });

  it('updates labels when the language changes at runtime', async () => {
    const field = create({ type: 'integer' }, 'maxBodyBytes');
    expect(field['title']()).toBe('Maximum response size (bytes)');

    transloco.setActiveLang('de');
    // Transloco events reach the field asynchronously (see SchemaField.revision).
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();

    expect(field['title']()).toBe('Maximale Antwortgröße (Bytes)');
    expect(field['description']()).toBe('');
  });

  it('uses the active language for hints too', async () => {
    const field = create({ type: 'string' }, 'url');
    transloco.setActiveLang('de');
    // Transloco events reach the field asynchronously (see SchemaField.revision).
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();
    expect(field['description']()).toBe('Vollständige Adresse');
  });

  it('orders the object properties by x-order', () => {
    const schema: JsonSchema = {
      type: 'object',
      required: ['url'],
      properties: {
        method: { type: 'string' },
        url: { type: 'string' },
        maxBodyBytes: { type: 'integer' },
      },
      'x-order': ['url', 'maxBodyBytes', 'method'],
    };
    const field = create(schema, '');
    expect(field['properties']().map((p) => p.key)).toEqual(['url', 'maxBodyBytes', 'method']);
  });

  it('puts required properties first when there is no x-order', () => {
    const schema: JsonSchema = {
      type: 'object',
      required: ['url'],
      properties: {
        method: { type: 'string' },
        url: { type: 'string' },
      },
    };
    const field = create(schema, '');
    expect(field['properties']().map((p) => p.key)).toEqual(['url', 'method']);
  });

  it('builds child paths below the node path', () => {
    const field = create({ type: 'object', properties: {} }, 'auth');
    expect(field['childPath']('type')).toBe('auth.type');
    const root = create({ type: 'object', properties: {} }, '');
    expect(root['childPath']('url')).toBe('url');
  });
});
